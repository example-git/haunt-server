ALTER TABLE chatxo.instances ADD COLUMN runtime_settings jsonb;
ALTER TABLE chatxo.instances ADD COLUMN settings_revision bigint NOT NULL DEFAULT 0;
ALTER TABLE chatxo.roles DROP CONSTRAINT roles_permissions_check;
ALTER TABLE chatxo.roles ADD CONSTRAINT roles_permissions_check CHECK (permissions BETWEEN 0 AND 32767);
CREATE TABLE chatxo.storage_providers (
 id uuid PRIMARY KEY,
 name text NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
 configuration jsonb NOT NULL,
 credentials bytea,
 active boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX one_active_storage_provider ON chatxo.storage_providers (active) WHERE active;
ALTER TABLE chatxo.attachments ADD COLUMN storage_provider_id uuid REFERENCES chatxo.storage_providers(id);
CREATE INDEX attachments_provider ON chatxo.attachments(storage_provider_id);
