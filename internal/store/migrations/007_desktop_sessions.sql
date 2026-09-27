CREATE TABLE desktop_sessions (
    box_id TEXT PRIMARY KEY,
    token_enc BLOB NOT NULL,
    expires_at TEXT NOT NULL,
    public INTEGER NOT NULL,
    host TEXT NOT NULL,
    browser INTEGER NOT NULL DEFAULT 0,
    profile TEXT NOT NULL DEFAULT ''
);
