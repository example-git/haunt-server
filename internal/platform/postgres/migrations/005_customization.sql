ALTER TABLE chatxo.users
    ADD COLUMN display_name text NOT NULL DEFAULT '' CHECK (length(display_name) <= 64),
    ADD COLUMN bio text NOT NULL DEFAULT '' CHECK (length(bio) <= 500),
    ADD COLUMN accent_color text NOT NULL DEFAULT '#77d9b0' CHECK (accent_color ~ '^#[0-9a-fA-F]{6}$'),
    ADD COLUMN avatar_version text NOT NULL DEFAULT '',
    ADD COLUMN avatar bytea CHECK (octet_length(avatar) <= 2097152);
ALTER TABLE chatxo.instances
    ADD COLUMN name text NOT NULL DEFAULT '' CHECK (length(name) <= 100),
    ADD COLUMN description text NOT NULL DEFAULT '' CHECK (length(description) <= 1000),
    ADD COLUMN accent_color text NOT NULL DEFAULT '#77d9b0' CHECK (accent_color ~ '^#[0-9a-fA-F]{6}$'),
    ADD COLUMN icon_version text NOT NULL DEFAULT '',
    ADD COLUMN icon bytea CHECK (octet_length(icon) <= 2097152),
    ADD COLUMN banner_version text NOT NULL DEFAULT '',
    ADD COLUMN banner bytea CHECK (octet_length(banner) <= 4194304);
ALTER TABLE chatxo.channels ADD COLUMN topic text NOT NULL DEFAULT '' CHECK (length(topic) <= 512);
