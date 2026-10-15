CREATE TABLE chatxo.instances (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    id text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE chatxo.users (
    id uuid PRIMARY KEY,
    instance_id text NOT NULL REFERENCES chatxo.instances(id),
    username text NOT NULL CHECK (username ~ '^[a-z0-9_]{3,32}$'),
    password_hash text NOT NULL,
    is_owner boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (instance_id, username)
);
CREATE UNIQUE INDEX one_owner ON chatxo.users(instance_id) WHERE is_owner;

CREATE TABLE chatxo.sessions (
    id uuid PRIMARY KEY,
    instance_id text NOT NULL REFERENCES chatxo.instances(id),
    user_id uuid NOT NULL REFERENCES chatxo.users(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE,
    device_name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_by_user ON chatxo.sessions(user_id, expires_at);
CREATE INDEX sessions_expiry ON chatxo.sessions(expires_at);

CREATE TABLE chatxo.invites (
    id uuid PRIMARY KEY,
    instance_id text NOT NULL REFERENCES chatxo.instances(id),
    token_hash bytea NOT NULL UNIQUE,
    created_by uuid NOT NULL REFERENCES chatxo.users(id),
    remaining_uses integer NOT NULL CHECK (remaining_uses BETWEEN 0 AND 100),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX invites_by_creator ON chatxo.invites(created_by, expires_at);

CREATE TABLE chatxo.roles (
    id uuid PRIMARY KEY,
    instance_id text NOT NULL REFERENCES chatxo.instances(id),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    permissions bigint NOT NULL CHECK (permissions BETWEEN 0 AND 31),
    is_default boolean NOT NULL DEFAULT false,
    UNIQUE (instance_id, name)
);
CREATE UNIQUE INDEX one_default_role ON chatxo.roles(instance_id) WHERE is_default;
CREATE TABLE chatxo.user_roles (
    user_id uuid NOT NULL REFERENCES chatxo.users(id) ON DELETE CASCADE,
    role_id uuid NOT NULL REFERENCES chatxo.roles(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, role_id)
);

CREATE TABLE chatxo.categories (
    id uuid PRIMARY KEY,
    instance_id text NOT NULL REFERENCES chatxo.instances(id),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 64)
);
CREATE TABLE chatxo.channels (
    id uuid PRIMARY KEY,
    instance_id text NOT NULL REFERENCES chatxo.instances(id),
    category_id uuid REFERENCES chatxo.categories(id) ON DELETE SET NULL,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    kind text NOT NULL CHECK (kind IN ('text', 'voice'))
);
CREATE TABLE chatxo.category_permissions (
    category_id uuid NOT NULL REFERENCES chatxo.categories(id) ON DELETE CASCADE,
    role_id uuid NOT NULL REFERENCES chatxo.roles(id) ON DELETE CASCADE,
    allow_bits bigint NOT NULL CHECK (allow_bits BETWEEN 0 AND 7),
    deny_bits bigint NOT NULL CHECK (deny_bits BETWEEN 0 AND 7),
    CHECK ((allow_bits & deny_bits) = 0),
    PRIMARY KEY (category_id, role_id)
);
CREATE TABLE chatxo.channel_permissions (
    channel_id uuid NOT NULL REFERENCES chatxo.channels(id) ON DELETE CASCADE,
    role_id uuid NOT NULL REFERENCES chatxo.roles(id) ON DELETE CASCADE,
    allow_bits bigint NOT NULL CHECK (allow_bits BETWEEN 0 AND 7),
    deny_bits bigint NOT NULL CHECK (deny_bits BETWEEN 0 AND 7),
    CHECK ((allow_bits & deny_bits) = 0),
    PRIMARY KEY (channel_id, role_id)
);
