ALTER TABLE chatxo.roles DROP CONSTRAINT roles_permissions_check;
ALTER TABLE chatxo.roles ADD CONSTRAINT roles_permissions_check CHECK (permissions BETWEEN 0 AND 262143);
UPDATE chatxo.roles SET permissions = permissions | 196608 WHERE (permissions & 384) <> 0;

CREATE TABLE chatxo.voice_restrictions (
    instance_id text NOT NULL REFERENCES chatxo.instances(id),
    channel_id uuid NOT NULL REFERENCES chatxo.channels(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES chatxo.users(id) ON DELETE CASCADE,
    muted boolean NOT NULL DEFAULT false,
    deafened boolean NOT NULL DEFAULT false,
    PRIMARY KEY (instance_id, channel_id, user_id)
);
