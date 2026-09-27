CREATE TABLE box_activity (
    id TEXT PRIMARY KEY,
    box_id TEXT NOT NULL,
    time TEXT NOT NULL,
    actor_type TEXT NOT NULL CHECK(actor_type IN ('user','api_key','system')),
    actor_id TEXT NOT NULL DEFAULT '',
    actor_label TEXT NOT NULL,
    category TEXT NOT NULL CHECK(category IN ('lifecycle','command','agent','access','snapshot')),
    action TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(detail)),
    duration_ms INTEGER,
    outcome TEXT CHECK(outcome IS NULL OR outcome IN ('ok','error'))
);
CREATE INDEX box_activity_box_time_idx ON box_activity(box_id, time DESC);
CREATE INDEX box_activity_time_idx ON box_activity(time);
CREATE TABLE box_running_intervals (
    id TEXT PRIMARY KEY,
    box_id TEXT NOT NULL,
    started_at TEXT NOT NULL,
    stopped_at TEXT
);
CREATE INDEX box_running_intervals_box_start_idx ON box_running_intervals(box_id, started_at);
