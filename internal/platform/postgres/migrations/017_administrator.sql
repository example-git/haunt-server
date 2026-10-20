ALTER TABLE chatxo.roles DROP CONSTRAINT roles_permissions_check;
ALTER TABLE chatxo.roles ADD CONSTRAINT roles_permissions_check CHECK (permissions BETWEEN 0 AND 65535);
