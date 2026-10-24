ALTER TABLE boxes ADD COLUMN source TEXT CHECK (source IS NULL OR json_valid(source));
