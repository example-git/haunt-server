ALTER TABLE chatxo.messages ADD COLUMN mention_user_ids uuid[] NOT NULL DEFAULT '{}';
-- Usernames are immutable. Backfill retained messages once, using the same
-- punctuation boundary as the clients; email addresses are not mentions.
UPDATE chatxo.messages m SET mention_user_ids=ARRAY(
    SELECT DISTINCT u.id FROM regexp_matches(m.body,$rx$(^|[\s([{"'.,!?:;])@([a-zA-Z0-9_]{3,32})(?![a-zA-Z0-9_])$rx$,'g') AS found(parts)
    JOIN chatxo.users u ON lower(u.username)=lower(parts[2])
    JOIN chatxo.conversations c ON c.id=m.conversation_id AND c.instance_id=u.instance_id
    WHERE NOT m.deleted ORDER BY u.id
);
-- Account for the newly persisted JSON field without changing existing event
-- payloads or resurrecting expired history.
WITH costs AS (SELECT id,octet_length(',"mention_user_ids":'||array_to_json(mention_user_ids)::text) delta FROM chatxo.messages WHERE cardinality(mention_user_ids)>0),
changed AS (UPDATE chatxo.messages m SET bytes=m.bytes+c.delta FROM costs c WHERE m.id=c.id RETURNING c.delta)
UPDATE chatxo.storage SET text_bytes=text_bytes+(SELECT COALESCE(sum(delta),0) FROM changed) WHERE singleton;
CREATE TABLE chatxo.read_positions (
    user_id uuid NOT NULL REFERENCES chatxo.users(id) ON DELETE CASCADE,
    conversation_id uuid NOT NULL REFERENCES chatxo.conversations(id) ON DELETE CASCADE,
    last_seq bigint NOT NULL DEFAULT 0 CHECK(last_seq>=0),
    PRIMARY KEY(user_id,conversation_id)
);
CREATE INDEX messages_unread ON chatxo.messages(conversation_id,seq) WHERE NOT deleted;

-- NOTIFY is a coalescible wake-up hint, delivered only after commit. Durable
-- payloads remain in the bounded event log; reconnects use cursors/snapshots.
CREATE FUNCTION chatxo.gateway_hint() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('chatxo_gateway', TG_ARGV[0]);
    RETURN NULL;
END
$$;
CREATE TRIGGER gateway_events AFTER INSERT ON chatxo.events FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('events');
CREATE TRIGGER gateway_users AFTER INSERT OR UPDATE OR DELETE ON chatxo.users FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('directory');
CREATE TRIGGER gateway_instance AFTER UPDATE ON chatxo.instances FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('directory');
CREATE TRIGGER gateway_channels AFTER INSERT OR UPDATE OR DELETE ON chatxo.channels FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('directory');
CREATE TRIGGER gateway_categories AFTER INSERT OR UPDATE OR DELETE ON chatxo.categories FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('directory');
CREATE TRIGGER gateway_roles AFTER INSERT OR UPDATE OR DELETE ON chatxo.roles FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('directory');
CREATE TRIGGER gateway_user_roles AFTER INSERT OR UPDATE OR DELETE ON chatxo.user_roles FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('directory');
CREATE TRIGGER gateway_channel_permissions AFTER INSERT OR UPDATE OR DELETE ON chatxo.channel_permissions FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('directory');
CREATE TRIGGER gateway_category_permissions AFTER INSERT OR UPDATE OR DELETE ON chatxo.category_permissions FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('directory');
CREATE TRIGGER gateway_conversations AFTER INSERT OR DELETE ON chatxo.conversations FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('directory');
CREATE TRIGGER gateway_members AFTER INSERT OR UPDATE OR DELETE ON chatxo.conversation_members FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('directory');
CREATE TRIGGER gateway_sessions AFTER UPDATE OR DELETE ON chatxo.sessions FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('sessions');
CREATE FUNCTION chatxo.gateway_read_hint() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('chatxo_gateway', 'read:'||NEW.user_id::text||':'||NEW.conversation_id::text);
    RETURN NULL;
END
$$;
CREATE TRIGGER gateway_reads AFTER INSERT OR UPDATE ON chatxo.read_positions FOR EACH ROW EXECUTE FUNCTION chatxo.gateway_read_hint();
