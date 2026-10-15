ALTER TABLE chatxo.roles DROP CONSTRAINT roles_permissions_check;
ALTER TABLE chatxo.roles ADD CONSTRAINT roles_permissions_check CHECK (permissions BETWEEN 0 AND 8191);
ALTER TABLE chatxo.roles ADD COLUMN position integer NOT NULL DEFAULT 1 CHECK(position BETWEEN 0 AND 1000);
UPDATE chatxo.roles SET position=0 WHERE is_default;
ALTER TABLE chatxo.roles ADD CONSTRAINT default_role_position CHECK (NOT is_default OR position=0);
ALTER TABLE chatxo.users ADD COLUMN kicked boolean NOT NULL DEFAULT false, ADD COLUMN deleted_at timestamptz;
ALTER TABLE chatxo.sessions ADD COLUMN remote_ip inet;

CREATE TABLE chatxo.member_ips (
  user_id uuid NOT NULL REFERENCES chatxo.users(id) ON DELETE CASCADE,
  ip inet NOT NULL, first_seen timestamptz NOT NULL DEFAULT now(), last_seen timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(user_id,ip)
);
CREATE TABLE chatxo.bans (
  id uuid PRIMARY KEY, instance_id text NOT NULL REFERENCES chatxo.instances(id),
  user_id uuid REFERENCES chatxo.users(id), network cidr,
  reason text NOT NULL CHECK(length(reason) BETWEEN 1 AND 512),
  created_by uuid NOT NULL REFERENCES chatxo.users(id), created_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz, appeal_token text NOT NULL UNIQUE, last_appeal_at timestamptz,
  CHECK((user_id IS NULL) <> (network IS NULL))
);
CREATE UNIQUE INDEX active_user_ban ON chatxo.bans(user_id) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX active_network_ban ON chatxo.bans(instance_id,network) WHERE revoked_at IS NULL;
CREATE INDEX banned_networks ON chatxo.bans USING gist(network inet_ops) WHERE revoked_at IS NULL;
CREATE TABLE chatxo.ban_appeals (
  id uuid PRIMARY KEY, ban_id uuid NOT NULL REFERENCES chatxo.bans(id) ON DELETE CASCADE,
  body text NOT NULL CHECK(length(body) BETWEEN 10 AND 1000), created_at timestamptz NOT NULL DEFAULT now(),
  reviewed_at timestamptz, reviewed_by uuid REFERENCES chatxo.users(id)
);
CREATE INDEX appeals_by_ban ON chatxo.ban_appeals(ban_id,created_at DESC);
CREATE TABLE chatxo.audit_log (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  instance_id text NOT NULL REFERENCES chatxo.instances(id), created_at timestamptz NOT NULL DEFAULT now(),
  actor_id uuid, target_id text NOT NULL DEFAULT '', action text NOT NULL CHECK(length(action)<=80),
  reason text NOT NULL DEFAULT '' CHECK(length(reason)<=512), ip inet
);
CREATE INDEX audit_by_instance ON chatxo.audit_log(instance_id,id DESC);
CREATE TRIGGER gateway_bans AFTER INSERT OR UPDATE OR DELETE ON chatxo.bans FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('sessions');
