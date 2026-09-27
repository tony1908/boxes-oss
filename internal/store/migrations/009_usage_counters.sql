CREATE TABLE box_usage_counts (
    box_id TEXT PRIMARY KEY,
    commands_count INTEGER NOT NULL DEFAULT 0,
    agent_runs_count INTEGER NOT NULL DEFAULT 0,
    desktop_sessions_count INTEGER NOT NULL DEFAULT 0,
    snapshots_count INTEGER NOT NULL DEFAULT 0
);

INSERT INTO box_usage_counts(box_id)
SELECT id FROM boxes;

UPDATE box_usage_counts
SET snapshots_count = COALESCE((SELECT COUNT(*) FROM snapshots WHERE snapshots.box_id = box_usage_counts.box_id), 0);

INSERT INTO box_running_intervals(id, box_id, started_at)
SELECT 'run_migrated_' || id, id, running_since
FROM boxes
WHERE running_since IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM box_running_intervals WHERE box_running_intervals.box_id = boxes.id AND stopped_at IS NULL);

INSERT INTO box_usage_counts(box_id, commands_count, agent_runs_count, desktop_sessions_count)
SELECT box_id,
       SUM(CASE WHEN category='command' AND action='exec' THEN 1 ELSE 0 END),
       SUM(CASE WHEN category='agent' AND action='prompt run queued' THEN 1 ELSE 0 END),
       SUM(CASE WHEN category='access' AND action='desktop session created' THEN 1 ELSE 0 END)
FROM box_activity
GROUP BY box_id
ON CONFLICT(box_id) DO UPDATE SET
    commands_count = excluded.commands_count,
    agent_runs_count = excluded.agent_runs_count,
    desktop_sessions_count = excluded.desktop_sessions_count;

CREATE TRIGGER box_activity_usage_count_insert AFTER INSERT ON box_activity
BEGIN
    INSERT INTO box_usage_counts(box_id, commands_count, agent_runs_count, desktop_sessions_count)
    VALUES(
        NEW.box_id,
        CASE WHEN NEW.category='command' AND NEW.action='exec' THEN 1 ELSE 0 END,
        CASE WHEN NEW.category='agent' AND NEW.action='prompt run queued' THEN 1 ELSE 0 END,
        CASE WHEN NEW.category='access' AND NEW.action='desktop session created' THEN 1 ELSE 0 END
    )
    ON CONFLICT(box_id) DO UPDATE SET
        commands_count = commands_count + excluded.commands_count,
        agent_runs_count = agent_runs_count + excluded.agent_runs_count,
        desktop_sessions_count = desktop_sessions_count + excluded.desktop_sessions_count;
END;

CREATE TRIGGER box_snapshot_usage_count_insert AFTER INSERT ON snapshots
BEGIN
    INSERT INTO box_usage_counts(box_id, snapshots_count)
    VALUES(NEW.box_id, 1)
    ON CONFLICT(box_id) DO UPDATE SET snapshots_count = snapshots_count + 1;
END;
