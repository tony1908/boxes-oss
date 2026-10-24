ALTER TABLE boxes ADD COLUMN network_policy TEXT CHECK (network_policy IS NULL OR json_valid(network_policy));
