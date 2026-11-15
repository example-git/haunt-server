ALTER TABLE chatxo.channels DROP CONSTRAINT channels_kind_check;
ALTER TABLE chatxo.channels ADD CONSTRAINT channels_kind_check CHECK(kind IN ('text','voice','ai'));
ALTER TABLE chatxo.roles DROP CONSTRAINT roles_permissions_check;
ALTER TABLE chatxo.roles ADD CONSTRAINT roles_permissions_check CHECK(permissions BETWEEN 0 AND 1048575);
UPDATE chatxo.roles SET permissions=permissions|786432 WHERE is_default;
ALTER TABLE chatxo.category_permissions DROP CONSTRAINT category_permissions_allow_bits_check;
ALTER TABLE chatxo.category_permissions DROP CONSTRAINT category_permissions_deny_bits_check;
ALTER TABLE chatxo.channel_permissions DROP CONSTRAINT channel_permissions_allow_bits_check;
ALTER TABLE chatxo.channel_permissions DROP CONSTRAINT channel_permissions_deny_bits_check;
ALTER TABLE chatxo.category_permissions ADD CHECK(allow_bits >= 0 AND (allow_bits & ~786439::bigint)=0), ADD CHECK(deny_bits >= 0 AND (deny_bits & ~786439::bigint)=0);
ALTER TABLE chatxo.channel_permissions ADD CHECK(allow_bits >= 0 AND (allow_bits & ~786439::bigint)=0), ADD CHECK(deny_bits >= 0 AND (deny_bits & ~786439::bigint)=0);
ALTER TABLE chatxo.messages ALTER COLUMN author_id DROP NOT NULL;
ALTER TABLE chatxo.messages ADD COLUMN ai_author jsonb;
ALTER TABLE chatxo.messages ADD CONSTRAINT messages_author_check CHECK ((author_id IS NOT NULL AND ai_author IS NULL) OR (author_id IS NULL AND ai_author IS NOT NULL AND jsonb_typeof(ai_author)='object' AND ai_author ? 'id' AND ai_author ? 'name'));
CREATE UNIQUE INDEX ai_message_request ON chatxo.messages(conversation_id,request_id) WHERE author_id IS NULL;
CREATE TABLE chatxo.ai_accounts (
 id uuid PRIMARY KEY, instance_id text NOT NULL REFERENCES chatxo.instances(id), provider text NOT NULL CHECK(provider IN ('claude','codex','antigravity')),
 created_by uuid NOT NULL REFERENCES chatxo.users(id), label text NOT NULL DEFAULT '', status text NOT NULL DEFAULT 'connected' CHECK(status IN ('connected','requires_auth','disconnected')),
 credentials bytea, credential_revision bigint NOT NULL DEFAULT 1, expires_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE chatxo.ai_channel_settings (
 channel_id uuid PRIMARY KEY REFERENCES chatxo.channels(id) ON DELETE CASCADE, account_id uuid REFERENCES chatxo.ai_accounts(id) ON DELETE SET NULL,
 provider text NOT NULL DEFAULT 'claude' CHECK(provider IN ('claude','codex','antigravity')), model text NOT NULL DEFAULT '', instructions text NOT NULL DEFAULT '' CHECK(octet_length(instructions)<=32768),
 name text NOT NULL DEFAULT 'Assistant' CHECK(length(name) BETWEEN 1 AND 80), avatar_url text NOT NULL DEFAULT '' CHECK(octet_length(avatar_url)<=524288),
 bytes bigint NOT NULL DEFAULT 0, active_preset_id uuid, revision bigint NOT NULL DEFAULT 1, history_epoch bigint NOT NULL DEFAULT 0, max_bytes bigint NOT NULL DEFAULT 67108864 CHECK(max_bytes>=4096)
);
CREATE TABLE chatxo.ai_presets (
 id uuid PRIMARY KEY, channel_id uuid NOT NULL REFERENCES chatxo.ai_channel_settings(channel_id) ON DELETE CASCADE,
 name text NOT NULL CHECK(length(name) BETWEEN 1 AND 80), avatar_url text NOT NULL DEFAULT '' CHECK(octet_length(avatar_url)<=524288), instructions text NOT NULL DEFAULT '' CHECK(octet_length(instructions)<=32768), bytes bigint NOT NULL DEFAULT 0, revision bigint NOT NULL DEFAULT 1,
 UNIQUE(channel_id,id)
);
ALTER TABLE chatxo.ai_channel_settings ADD FOREIGN KEY(channel_id,active_preset_id) REFERENCES chatxo.ai_presets(channel_id,id) DEFERRABLE INITIALLY DEFERRED;
CREATE TABLE chatxo.ai_oauth_sessions (
 id uuid PRIMARY KEY, instance_id text NOT NULL REFERENCES chatxo.instances(id), user_id uuid NOT NULL REFERENCES chatxo.users(id) ON DELETE CASCADE,
 provider text NOT NULL, origin text NOT NULL, redirect_uri text NOT NULL, state text NOT NULL, verifier bytea NOT NULL, account_id uuid REFERENCES chatxo.ai_accounts(id) ON DELETE CASCADE,
 expires_at timestamptz NOT NULL, consumed_at timestamptz, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ai_oauth_expiry ON chatxo.ai_oauth_sessions(expires_at);
CREATE TABLE chatxo.ai_turns (
 id uuid PRIMARY KEY, channel_id uuid NOT NULL REFERENCES chatxo.ai_channel_settings(channel_id) ON DELETE CASCADE,
 trigger_message_id uuid NOT NULL REFERENCES chatxo.messages(id) ON DELETE CASCADE, actor_id uuid NOT NULL REFERENCES chatxo.users(id),
 snapshot jsonb NOT NULL, boundary_seq bigint NOT NULL, history_epoch bigint NOT NULL, response_request_id uuid NOT NULL, reservation_bytes bigint NOT NULL DEFAULT 0 CHECK(reservation_bytes>=0), status text NOT NULL CHECK(status IN ('queued','running','waiting','requires_auth','completed','failed','cancelled','interrupted')),
 revision bigint NOT NULL DEFAULT 1, partial_text text NOT NULL DEFAULT '' CHECK(octet_length(partial_text)<=262144), error_code text NOT NULL DEFAULT '', retry_at timestamptz,
 attempt integer NOT NULL DEFAULT 0, bytes bigint NOT NULL DEFAULT 0, created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ai_one_active ON chatxo.ai_turns(channel_id) WHERE status='running';
CREATE INDEX ai_turn_queue ON chatxo.ai_turns(status,created_at,id);
CREATE INDEX ai_turn_history ON chatxo.ai_turns(channel_id,boundary_seq);
CREATE TABLE chatxo.ai_turn_steps (
 id bigserial PRIMARY KEY, attempt integer NOT NULL DEFAULT 0, kind text NOT NULL DEFAULT 'step' CHECK(kind IN ('step','replay')), turn_id uuid NOT NULL REFERENCES chatxo.ai_turns(id) ON DELETE CASCADE,
 data jsonb NOT NULL CHECK(octet_length(data::text)<=1048576), bytes bigint NOT NULL DEFAULT 0, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ai_steps_turn ON chatxo.ai_turn_steps(turn_id,id);
CREATE UNIQUE INDEX ai_replay_attempt ON chatxo.ai_turn_steps(turn_id,attempt) WHERE kind='replay';
CREATE TABLE chatxo.ai_avatars (
 channel_id uuid NOT NULL REFERENCES chatxo.ai_channel_settings(channel_id) ON DELETE CASCADE,
 version text NOT NULL CHECK(version ~ '^[0-9a-f]{32}$'), data bytea NOT NULL CHECK(octet_length(data)<=20971520), bytes bigint NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(channel_id,version)
);
CREATE FUNCTION chatxo.ai_measure_bytes() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 NEW.bytes := octet_length((to_jsonb(NEW)-'bytes')::text)+128+COALESCE((to_jsonb(NEW)->>'reservation_bytes')::bigint,0);
 RETURN NEW;
END $$;
CREATE FUNCTION chatxo.ai_account_bytes() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='DELETE' THEN
  UPDATE chatxo.storage SET text_bytes=text_bytes-OLD.bytes WHERE singleton;
 ELSIF TG_OP='INSERT' THEN
  UPDATE chatxo.storage SET text_bytes=text_bytes+NEW.bytes WHERE singleton;
 ELSE
  UPDATE chatxo.storage SET text_bytes=text_bytes+NEW.bytes-OLD.bytes WHERE singleton;
 END IF;
 RETURN NULL;
END $$;
CREATE TRIGGER ai_turn_measure BEFORE INSERT OR UPDATE ON chatxo.ai_turns FOR EACH ROW EXECUTE FUNCTION chatxo.ai_measure_bytes();
CREATE TRIGGER ai_turn_bytes AFTER INSERT OR UPDATE OR DELETE ON chatxo.ai_turns FOR EACH ROW EXECUTE FUNCTION chatxo.ai_account_bytes();
CREATE TRIGGER ai_step_measure BEFORE INSERT OR UPDATE ON chatxo.ai_turn_steps FOR EACH ROW EXECUTE FUNCTION chatxo.ai_measure_bytes();
CREATE TRIGGER ai_step_bytes AFTER INSERT OR UPDATE OR DELETE ON chatxo.ai_turn_steps FOR EACH ROW EXECUTE FUNCTION chatxo.ai_account_bytes();
CREATE TRIGGER ai_settings_measure BEFORE INSERT OR UPDATE ON chatxo.ai_channel_settings FOR EACH ROW EXECUTE FUNCTION chatxo.ai_measure_bytes();
CREATE TRIGGER ai_settings_bytes AFTER INSERT OR UPDATE OR DELETE ON chatxo.ai_channel_settings FOR EACH ROW EXECUTE FUNCTION chatxo.ai_account_bytes();
CREATE TRIGGER ai_preset_measure BEFORE INSERT OR UPDATE ON chatxo.ai_presets FOR EACH ROW EXECUTE FUNCTION chatxo.ai_measure_bytes();
CREATE TRIGGER ai_preset_bytes AFTER INSERT OR UPDATE OR DELETE ON chatxo.ai_presets FOR EACH ROW EXECUTE FUNCTION chatxo.ai_account_bytes();

CREATE TRIGGER ai_avatar_measure BEFORE INSERT OR UPDATE ON chatxo.ai_avatars FOR EACH ROW EXECUTE FUNCTION chatxo.ai_measure_bytes();
CREATE TRIGGER ai_avatar_bytes AFTER INSERT OR UPDATE OR DELETE ON chatxo.ai_avatars FOR EACH ROW EXECUTE FUNCTION chatxo.ai_account_bytes();
