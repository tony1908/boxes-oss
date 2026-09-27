CREATE TABLE users (
    id TEXT PRIMARY KEY,
    email TEXT NOT NULL UNIQUE COLLATE NOCASE,
    role TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX sessions_user_id_idx ON sessions(user_id);
CREATE INDEX sessions_expires_at_idx ON sessions(expires_at);

CREATE TABLE api_keys (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    key_hash TEXT NOT NULL UNIQUE,
    scopes TEXT NOT NULL CHECK (json_valid(scopes)),
    expires_at TEXT,
    created_at TEXT NOT NULL
);
CREATE INDEX api_keys_user_id_idx ON api_keys(user_id);

CREATE TABLE nodes (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    host TEXT NOT NULL DEFAULT '',
    host_base TEXT NOT NULL DEFAULT '',
    token_hash TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    cpus INTEGER NOT NULL DEFAULT 0,
    memory_bytes INTEGER NOT NULL DEFAULT 0,
    free_memory_bytes INTEGER NOT NULL DEFAULT 0,
    disk_free_bytes INTEGER NOT NULL DEFAULT 0,
    last_seen TEXT
);

CREATE TABLE environments (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    version INTEGER NOT NULL CHECK (version > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE environment_versions (
    environment_id TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    version INTEGER NOT NULL CHECK (version > 0),
    document TEXT NOT NULL CHECK (json_valid(document)),
    secrets_enc BLOB NOT NULL DEFAULT X'',
    created_at TEXT NOT NULL,
    PRIMARY KEY (environment_id, version)
);

CREATE TABLE boxes (
    id TEXT PRIMARY KEY,
    node_id TEXT NOT NULL REFERENCES nodes(id),
    type TEXT NOT NULL,
    state TEXT NOT NULL,
    image TEXT NOT NULL,
    env_id TEXT REFERENCES environments(id) ON DELETE SET NULL,
    env_version INTEGER NOT NULL DEFAULT 0,
    ttl_seconds INTEGER NOT NULL DEFAULT 0,
    expires_at TEXT,
    desktop_password_enc BLOB NOT NULL DEFAULT X'',
    boxd_token_enc BLOB NOT NULL DEFAULT X'',
    idempotency_key TEXT UNIQUE,
    from_snapshot TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    stopped_at TEXT
);
CREATE INDEX boxes_node_id_idx ON boxes(node_id);
CREATE INDEX boxes_state_idx ON boxes(state);
CREATE INDEX boxes_expires_at_idx ON boxes(expires_at);

CREATE TABLE snapshots (
    id TEXT PRIMARY KEY,
    box_id TEXT NOT NULL REFERENCES boxes(id) ON DELETE CASCADE,
    restic_id TEXT NOT NULL,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL
);
CREATE INDEX snapshots_box_id_idx ON snapshots(box_id, created_at);

CREATE TABLE named_snapshots (
    name TEXT PRIMARY KEY,
    snapshot_id TEXT NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL
);

CREATE TABLE conversations (
    id TEXT PRIMARY KEY,
    box_id TEXT NOT NULL REFERENCES boxes(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    model TEXT NOT NULL DEFAULT '',
    prompts INTEGER NOT NULL DEFAULT 0,
    running INTEGER NOT NULL DEFAULT 0 CHECK (running IN (0, 1)),
    last_prompt TEXT NOT NULL DEFAULT '',
    current INTEGER NOT NULL DEFAULT 0 CHECK (current IN (0, 1)),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX conversations_box_id_idx ON conversations(box_id, updated_at);

CREATE TABLE runs (
    id TEXT PRIMARY KEY,
    conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    status TEXT NOT NULL,
    error TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT
);
CREATE INDEX runs_conversation_id_idx ON runs(conversation_id, created_at);

CREATE TABLE events (
    cursor INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    box_id TEXT NOT NULL REFERENCES boxes(id) ON DELETE CASCADE,
    conversation_id TEXT REFERENCES conversations(id) ON DELETE SET NULL,
    run_id TEXT,
    type TEXT NOT NULL,
    time TEXT NOT NULL,
    data TEXT NOT NULL CHECK (json_valid(data))
);
CREATE INDEX events_box_cursor_idx ON events(box_id, cursor);
CREATE INDEX events_conversation_cursor_idx ON events(conversation_id, cursor);

CREATE TABLE hosts (
    box_id TEXT NOT NULL REFERENCES boxes(id) ON DELETE CASCADE,
    port INTEGER NOT NULL CHECK (port > 0 AND port <= 65535),
    private INTEGER NOT NULL DEFAULT 0 CHECK (private IN (0, 1)),
    token_hash TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (box_id, port)
);

CREATE TABLE webhooks (
    id TEXT PRIMARY KEY,
    url TEXT NOT NULL,
    events TEXT NOT NULL CHECK (json_valid(events)),
    secret TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE idempotency (
    key TEXT PRIMARY KEY,
    scope TEXT NOT NULL,
    response BLOB NOT NULL,
    created_at TEXT NOT NULL
);
