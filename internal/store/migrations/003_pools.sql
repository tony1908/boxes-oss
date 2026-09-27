CREATE TABLE pools (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    provider TEXT NOT NULL,
    credentials_enc BLOB NOT NULL,
    region TEXT NOT NULL,
    size TEXT NOT NULL,
    image TEXT NOT NULL,
    ssh_keys TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(ssh_keys)),
    min_nodes INTEGER NOT NULL DEFAULT 0,
    max_nodes INTEGER NOT NULL,
    idle_minutes INTEGER NOT NULL DEFAULT 10,
    boot_timeout_minutes INTEGER NOT NULL DEFAULT 15,
    created_at TEXT NOT NULL
);

ALTER TABLE nodes ADD COLUMN pool_id TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN provider_instance_id TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN created_at TEXT NOT NULL DEFAULT '';
ALTER TABLE nodes ADD COLUMN idle_since TEXT;
CREATE INDEX nodes_pool_id_idx ON nodes(pool_id);

CREATE TABLE pending_boxes (
    box_id TEXT PRIMARY KEY REFERENCES boxes(id) ON DELETE CASCADE,
    payload_enc BLOB NOT NULL
);
