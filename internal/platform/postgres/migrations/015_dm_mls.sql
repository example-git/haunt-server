CREATE TABLE chatxo.dm_devices (
 id uuid PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES chatxo.users(id),
 public_key bytea NOT NULL CHECK(octet_length(public_key)=32),
 credential text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 revoked boolean NOT NULL DEFAULT false
);
CREATE INDEX dm_devices_user ON chatxo.dm_devices(user_id);
CREATE TABLE chatxo.dm_key_packages (
 id uuid PRIMARY KEY,
 device_id uuid NOT NULL REFERENCES chatxo.dm_devices(id),
 data bytea NOT NULL CHECK(octet_length(data) BETWEEN 100 AND 8192),
 used_by uuid REFERENCES chatxo.conversations(id),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dm_packages_unused ON chatxo.dm_key_packages(device_id,created_at) WHERE used_by IS NULL;
CREATE TABLE chatxo.dm_crypto_groups (
 conversation_id uuid PRIMARY KEY REFERENCES chatxo.conversations(id),
 revision bigint NOT NULL DEFAULT 0,
 devices uuid[] NOT NULL DEFAULT '{}'
);
CREATE TABLE chatxo.dm_crypto_events (
 conversation_id uuid NOT NULL REFERENCES chatxo.conversations(id),
 revision bigint NOT NULL,
 request_id uuid NOT NULL UNIQUE,
 request_hash bytea NOT NULL,
 actor uuid NOT NULL REFERENCES chatxo.dm_devices(id),
 commit_data bytea NOT NULL,
 welcome bytea,
 devices uuid[] NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(conversation_id,revision)
);
ALTER TABLE chatxo.storage ADD COLUMN dm_crypto_bytes bigint NOT NULL DEFAULT 0 CHECK(dm_crypto_bytes>=0);
