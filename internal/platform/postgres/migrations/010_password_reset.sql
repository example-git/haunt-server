ALTER TABLE chatxo.users ADD COLUMN password_change_required boolean NOT NULL DEFAULT false;
ALTER TABLE chatxo.roles DROP CONSTRAINT roles_permissions_check;
ALTER TABLE chatxo.roles ADD CONSTRAINT roles_permissions_check CHECK (permissions BETWEEN 0 AND 16383);
