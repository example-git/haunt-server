ALTER TABLE chatxo.users DROP CONSTRAINT users_avatar_check,
    ADD CONSTRAINT users_avatar_check CHECK (octet_length(avatar) <= 20971520);
ALTER TABLE chatxo.instances DROP CONSTRAINT instances_icon_check,
    DROP CONSTRAINT instances_banner_check,
    ADD CONSTRAINT instances_icon_check CHECK (octet_length(icon) <= 20971520),
    ADD CONSTRAINT instances_banner_check CHECK (octet_length(banner) <= 20971520);
CREATE TABLE chatxo.emojis (
    id uuid PRIMARY KEY,
    instance_id text NOT NULL REFERENCES chatxo.instances(id) ON DELETE CASCADE,
    name text NOT NULL CHECK (name ~ '^[a-z0-9_]{2,32}$'),
    version uuid NOT NULL,
    animated boolean NOT NULL,
    image bytea NOT NULL CHECK (octet_length(image) BETWEEN 1 AND 1048576),
    UNIQUE (instance_id, name)
);
CREATE TRIGGER gateway_emojis AFTER INSERT OR UPDATE OR DELETE ON chatxo.emojis
    FOR EACH STATEMENT EXECUTE FUNCTION chatxo.gateway_hint('directory');
