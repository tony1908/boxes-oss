CREATE TABLE api_key_daily_usage (
    api_key_id TEXT NOT NULL,
    day TEXT NOT NULL,
    requests INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(api_key_id, day)
);
