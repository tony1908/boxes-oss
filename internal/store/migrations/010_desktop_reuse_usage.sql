UPDATE box_usage_counts
SET desktop_sessions_count = COALESCE((
    SELECT COUNT(*) FROM box_activity
    WHERE box_activity.box_id = box_usage_counts.box_id
      AND box_activity.category = 'access'
      AND box_activity.action IN ('desktop session created', 'desktop session reused')
), 0);

DROP TRIGGER IF EXISTS box_activity_usage_count_insert;
CREATE TRIGGER box_activity_usage_count_insert AFTER INSERT ON box_activity
BEGIN
    INSERT INTO box_usage_counts(box_id, commands_count, agent_runs_count, desktop_sessions_count)
    VALUES(
        NEW.box_id,
        CASE WHEN NEW.category='command' AND NEW.action='exec' THEN 1 ELSE 0 END,
        CASE WHEN NEW.category='agent' AND NEW.action='prompt run queued' THEN 1 ELSE 0 END,
        CASE WHEN NEW.category='access' AND NEW.action IN ('desktop session created', 'desktop session reused') THEN 1 ELSE 0 END
    )
    ON CONFLICT(box_id) DO UPDATE SET
        commands_count = commands_count + excluded.commands_count,
        agent_runs_count = agent_runs_count + excluded.agent_runs_count,
        desktop_sessions_count = desktop_sessions_count + excluded.desktop_sessions_count;
END;
