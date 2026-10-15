-- Keep legacy DM records; subsequent channel cleanup must not evict them.
ALTER TABLE chatxo.storage ADD COLUMN dm_attachment_bytes bigint NOT NULL DEFAULT 0 CHECK(dm_attachment_bytes >= 0);
ALTER TABLE chatxo.storage ADD COLUMN dm_text_bytes bigint NOT NULL DEFAULT 0 CHECK(dm_text_bytes >= 0);
ALTER TABLE chatxo.attachments ADD COLUMN is_dm boolean NOT NULL DEFAULT false;
ALTER TABLE chatxo.attachments ADD COLUMN encrypted boolean NOT NULL DEFAULT false;
ALTER TABLE chatxo.attachments ALTER COLUMN expires_at DROP NOT NULL;
UPDATE chatxo.attachments a SET is_dm=true FROM chatxo.conversations c WHERE c.id=a.conversation_id AND c.kind='dm';
UPDATE chatxo.storage SET dm_attachment_bytes=(SELECT COALESCE(sum(bytes),0) FROM chatxo.attachments WHERE is_dm), dm_text_bytes=(SELECT COALESCE(sum(m.bytes),0) FROM chatxo.messages m JOIN chatxo.conversations c ON c.id=m.conversation_id WHERE c.kind='dm');
CREATE INDEX dm_uploads_by_owner ON chatxo.attachments(user_id,created_at,id) WHERE is_dm;
-- Keep counters correct through message deletion, moderation, and maintenance.
CREATE FUNCTION chatxo.account_dm_attachment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' AND OLD.is_dm THEN UPDATE chatxo.storage SET dm_attachment_bytes=dm_attachment_bytes-OLD.bytes WHERE singleton; END IF;
    IF TG_OP <> 'DELETE' AND NEW.is_dm THEN UPDATE chatxo.storage SET dm_attachment_bytes=dm_attachment_bytes+NEW.bytes WHERE singleton; END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER dm_attachment_account AFTER INSERT OR DELETE OR UPDATE OF bytes,is_dm ON chatxo.attachments FOR EACH ROW EXECUTE FUNCTION chatxo.account_dm_attachment();
CREATE FUNCTION chatxo.account_dm_message() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' AND EXISTS(SELECT 1 FROM chatxo.conversations WHERE id=OLD.conversation_id AND kind='dm') THEN UPDATE chatxo.storage SET dm_text_bytes=dm_text_bytes-OLD.bytes WHERE singleton; END IF;
    IF TG_OP <> 'DELETE' AND EXISTS(SELECT 1 FROM chatxo.conversations WHERE id=NEW.conversation_id AND kind='dm') THEN UPDATE chatxo.storage SET dm_text_bytes=dm_text_bytes+NEW.bytes WHERE singleton; END IF;
    RETURN NULL;
END $$;
CREATE TRIGGER dm_message_account AFTER INSERT OR DELETE OR UPDATE OF bytes ON chatxo.messages FOR EACH ROW EXECUTE FUNCTION chatxo.account_dm_message();
-- UTC-day transfer reservations. Failed/retried transfers still consume budget.
CREATE TABLE chatxo.dm_transfers (
    day date NOT NULL,
    account text NOT NULL,
    uploaded bigint NOT NULL DEFAULT 0 CHECK(uploaded>=0),
    downloaded bigint NOT NULL DEFAULT 0 CHECK(downloaded>=0),
    PRIMARY KEY(day,account)
);
-- One shared predicate keeps history, edits, unread counts and replay consistent.
CREATE FUNCTION chatxo.message_retained(conv uuid, created timestamptz, channel_days bigint, dm_days bigint) RETURNS boolean LANGUAGE sql STABLE AS $$
    SELECT CASE WHEN kind='dm' THEN dm_days=0 OR created>now()-dm_days*interval '1 day' ELSE created>now()-channel_days*interval '1 day' END FROM chatxo.conversations WHERE id=conv
$$;
-- Runtime policies now include separate DM limits; retain full configuration
-- audit entries. Public moderation reasons still have their 512-character limit.
ALTER TABLE chatxo.audit_log DROP CONSTRAINT audit_log_reason_check;
ALTER TABLE chatxo.audit_log ADD CONSTRAINT audit_log_reason_check CHECK(length(reason)<=4096);
