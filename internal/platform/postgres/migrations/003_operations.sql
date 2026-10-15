ALTER TABLE chatxo.users ADD COLUMN disabled boolean NOT NULL DEFAULT false;
ALTER TABLE chatxo.users ADD CONSTRAINT owner_enabled CHECK(NOT(is_owner AND disabled));
