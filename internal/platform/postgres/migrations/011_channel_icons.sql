ALTER TABLE chatxo.channels ADD COLUMN icon text NOT NULL DEFAULT '' CHECK (length(icon) <= 32);
