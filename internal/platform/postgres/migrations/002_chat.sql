CREATE TABLE chatxo.storage (
    singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
    text_bytes bigint NOT NULL DEFAULT 0 CHECK(text_bytes >= 0),
    attachment_bytes bigint NOT NULL DEFAULT 0 CHECK(attachment_bytes >= 0),
    event_seq bigint NOT NULL DEFAULT 0,
    event_floor bigint NOT NULL DEFAULT 0
);
INSERT INTO chatxo.storage(singleton) VALUES (true);
CREATE TABLE chatxo.conversations (
    id uuid PRIMARY KEY,
    instance_id text NOT NULL REFERENCES chatxo.instances(id),
    kind text NOT NULL CHECK(kind IN ('channel','dm')),
    channel_id uuid UNIQUE REFERENCES chatxo.channels(id) ON DELETE SET NULL,
    dm_key text UNIQUE,
    last_seq bigint NOT NULL DEFAULT 0,
    pruned_through bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE chatxo.conversation_members (
    conversation_id uuid NOT NULL REFERENCES chatxo.conversations(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES chatxo.users(id),
    PRIMARY KEY(conversation_id,user_id)
);
CREATE TABLE chatxo.messages (
    id uuid PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES chatxo.conversations(id),
    author_id uuid NOT NULL REFERENCES chatxo.users(id),
    seq bigint NOT NULL,
    body text NOT NULL,
    attachment_ids uuid[] NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL,
    edited_at timestamptz,
    deleted boolean NOT NULL DEFAULT false,
    request_id uuid NOT NULL,
    request_hash bytea NOT NULL,
    bytes bigint NOT NULL CHECK(bytes > 0),
    UNIQUE(conversation_id,seq),
    UNIQUE(author_id,request_id)
);
CREATE INDEX message_acceptance ON chatxo.messages(created_at,id);
CREATE TABLE chatxo.events (
    id bigint PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES chatxo.conversations(id),
    message_id uuid REFERENCES chatxo.messages(id) ON DELETE CASCADE,
    payload bytea NOT NULL,
    bytes bigint NOT NULL CHECK(bytes > 0),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_by_conversation ON chatxo.events(conversation_id,id);
CREATE INDEX events_expiry ON chatxo.events(created_at,id);
CREATE TABLE chatxo.attachments (
    id uuid PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES chatxo.conversations(id),
    user_id uuid NOT NULL REFERENCES chatxo.users(id),
    message_id uuid REFERENCES chatxo.messages(id) ON DELETE SET NULL,
    claimed boolean NOT NULL DEFAULT false,
    status text NOT NULL CHECK(status IN ('uploading','ready','deleting')),
    filename text NOT NULL,
    content_type text NOT NULL DEFAULT '',
    bytes bigint NOT NULL CHECK(bytes > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX attachments_expiry ON chatxo.attachments(status,expires_at);
