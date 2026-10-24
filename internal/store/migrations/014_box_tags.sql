ALTER TABLE boxes ADD COLUMN tags TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(tags));
