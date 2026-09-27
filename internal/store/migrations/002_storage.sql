CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value BLOB NOT NULL
);

ALTER TABLE snapshots ADD COLUMN repository TEXT NOT NULL DEFAULT '';
ALTER TABLE snapshots ADD COLUMN storage_env_enc BLOB NOT NULL DEFAULT X'';
