package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type Activity struct {
	ID         string          `json:"id"`
	BoxID      string          `json:"boxId"`
	Time       time.Time       `json:"time"`
	ActorType  string          `json:"actorType"`
	ActorID    string          `json:"actorId"`
	ActorLabel string          `json:"actorLabel"`
	Category   string          `json:"category"`
	Action     string          `json:"action"`
	Detail     json.RawMessage `json:"detail"`
	DurationMS *int64          `json:"durationMs,omitempty"`
	Outcome    *string         `json:"outcome,omitempty"`
}

// APIKeyUsage summarizes recorded API-key activity over the supplied window.
func (s *Store) APIKeyUsage(ctx context.Context, keyID string, since time.Time) (map[string]int64, error) {
	usage := map[string]int64{"requests": 0, "sandboxesCreated": 0}
	var requests, created int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests),0) FROM api_key_daily_usage WHERE api_key_id=? AND day>=?`, keyID, since.UTC().Format("2006-01-02")).Scan(&requests)
	if err == nil {
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT box_id) FROM box_activity WHERE actor_type='api_key' AND actor_id=? AND category='lifecycle' AND action='created' AND time>=?`, keyID, encodeActivityTime(since)).Scan(&created)
	}
	usage["requests"], usage["sandboxesCreated"] = requests, created
	return usage, err
}

// AddAPIKeyRequestCounts flushes in-memory request totals in one write transaction.
func (s *Store) AddAPIKeyRequestCounts(ctx context.Context, counts map[string]int64, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	day := at.UTC().Format("2006-01-02")
	for id, count := range counts {
		if count == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO api_key_daily_usage(api_key_id,day,requests) VALUES(?,?,?) ON CONFLICT(api_key_id,day) DO UPDATE SET requests=requests+excluded.requests`, id, day, count); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_key_daily_usage WHERE day < ?`, at.UTC().Add(-30*24*time.Hour).Format("2006-01-02")); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RecordActivity(ctx context.Context, a Activity) error {
	if a.ID == "" {
		a.ID = fmt.Sprintf("act_%d", time.Now().UnixNano())
	}
	if a.Time.IsZero() {
		a.Time = time.Now().UTC()
	}
	if len(a.Detail) == 0 {
		a.Detail = []byte(`{}`)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO box_activity(id,box_id,time,actor_type,actor_id,actor_label,category,action,detail,duration_ms,outcome) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, a.ID, a.BoxID, encodeActivityTime(a.Time), a.ActorType, a.ActorID, a.ActorLabel, a.Category, a.Action, string(a.Detail), a.DurationMS, a.Outcome)
	return err
}

func (s *Store) ListActivity(ctx context.Context, boxID, category, before string, limit int) ([]Activity, error) {
	var cursor *time.Time
	if before != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, before); err == nil {
			cursor = &parsed
		}
	}
	return s.ListActivityPage(ctx, boxID, category, cursor, "", limit)
}

func (s *Store) ListActivityPage(ctx context.Context, boxID, category string, before *time.Time, beforeID string, limit int) ([]Activity, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	query := `SELECT id,box_id,time,actor_type,actor_id,actor_label,category,action,detail,duration_ms,outcome FROM box_activity WHERE box_id=?`
	args := []any{boxID}
	if category != "" {
		query += ` AND category=?`
		args = append(args, category)
	}
	if before != nil {
		if beforeID != "" {
			query += ` AND (time < ? OR (time = ? AND id < ?))`
			stamp := encodeActivityTime(*before)
			args = append(args, stamp, stamp, beforeID)
		} else {
			query += ` AND time < ?`
			args = append(args, encodeActivityTime(*before))
		}
	}
	query += ` ORDER BY time DESC,id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []Activity{}
	for rows.Next() {
		var a Activity
		var stamp, detail string
		var duration sql.NullInt64
		var outcome sql.NullString
		if err := rows.Scan(&a.ID, &a.BoxID, &stamp, &a.ActorType, &a.ActorID, &a.ActorLabel, &a.Category, &a.Action, &detail, &duration, &outcome); err != nil {
			return nil, err
		}
		a.Time, err = decodeTime(stamp)
		if err != nil {
			return nil, err
		}
		a.Detail = json.RawMessage(detail)
		if duration.Valid {
			a.DurationMS = &duration.Int64
		}
		if outcome.Valid {
			a.Outcome = &outcome.String
		}
		items = append(items, a)
	}
	return items, rows.Err()
}

func (s *Store) RecordPeriodicSnapshot(ctx context.Context, boxID string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	day := at.UTC().Format("2006-01-02")
	id := "periodic_" + boxID + "_" + strings.ReplaceAll(day, "-", "")
	_, err := s.db.ExecContext(ctx, `INSERT INTO box_activity(id,box_id,time,actor_type,actor_id,actor_label,category,action,detail) VALUES(?, ?, ?, 'system', '', 'system', 'snapshot', 'periodic snapshots', json_object('count', 1))
		ON CONFLICT(id) DO UPDATE SET time=excluded.time, detail=json_object('count', COALESCE(CAST(json_extract(box_activity.detail, '$.count') AS INTEGER), 0) + 1)`, id, boxID, encodeActivityTime(at))
	return err
}

type ActivityCounts struct {
	Commands        int64
	AgentRuns       int64
	DesktopSessions int64
	Snapshots       int64
}

func (s *Store) ActivityCounts(ctx context.Context, boxID string) (ActivityCounts, error) {
	var counts ActivityCounts
	err := s.db.QueryRowContext(ctx, `SELECT commands_count, agent_runs_count, desktop_sessions_count, snapshots_count
		FROM box_usage_counts WHERE box_id=?`, boxID).Scan(&counts.Commands, &counts.AgentRuns, &counts.DesktopSessions, &counts.Snapshots)
	if err == sql.ErrNoRows {
		return ActivityCounts{}, nil
	}
	return counts, err
}

func (s *Store) GetPromptRunQueuedActivity(ctx context.Context, runID string) (Activity, error) {
	var a Activity
	var stamp, detail string
	var duration sql.NullInt64
	var outcome sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,box_id,time,actor_type,actor_id,actor_label,category,action,detail,duration_ms,outcome
		FROM box_activity WHERE category='agent' AND action='prompt run queued' AND json_extract(detail, '$.runId')=? ORDER BY time DESC LIMIT 1`, runID).
		Scan(&a.ID, &a.BoxID, &stamp, &a.ActorType, &a.ActorID, &a.ActorLabel, &a.Category, &a.Action, &detail, &duration, &outcome)
	if err != nil {
		return Activity{}, err
	}
	a.Time, err = decodeTime(stamp)
	if err != nil {
		return Activity{}, err
	}
	a.Detail = json.RawMessage(detail)
	if duration.Valid {
		a.DurationMS = &duration.Int64
	}
	if outcome.Valid {
		a.Outcome = &outcome.String
	}
	return a, nil
}

func (s *Store) UpdateCommandActivityResult(ctx context.Context, boxID, processID string, exitCode int, finishedAt time.Time) error {
	var id, stamp, encodedDetail string
	err := s.db.QueryRowContext(ctx, `SELECT id,time,detail FROM box_activity
		WHERE box_id=? AND category='command' AND action='exec' AND json_extract(detail, '$.processId')=?
		ORDER BY time DESC LIMIT 1`, boxID, processID).Scan(&id, &stamp, &encodedDetail)
	if err != nil {
		return err
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(encodedDetail), &detail); err != nil {
		return err
	}
	detail["exitCode"] = exitCode
	detail["completed"] = true
	encoded, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	startedAt, err := decodeTime(stamp)
	if err != nil {
		return err
	}
	duration := finishedAt.Sub(startedAt).Milliseconds()
	if duration < 0 {
		duration = 0
	}
	outcome := "ok"
	if exitCode != 0 {
		outcome = "error"
	}
	_, err = s.db.ExecContext(ctx, `UPDATE box_activity SET detail=?,duration_ms=?,outcome=? WHERE id=?`, string(encoded), duration, outcome, id)
	return err
}

func (s *Store) PruneActivity(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM box_activity WHERE time < ?`, encodeActivityTime(before))
	return err
}

// BoxIdleSince returns the last user activity time relevant to automatic idle stop.
func (s *Store) BoxIdleSince(ctx context.Context, boxID string) (time.Time, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT MAX(time) FROM box_activity WHERE box_id=? AND category IN ('command','agent','access')`, boxID).Scan(&raw)
	if err != nil {
		return time.Time{}, err
	}
	if !raw.Valid {
		return time.Time{}, nil
	}
	return decodeTime(raw.String)
}

// BoxHasRunningAgentRun reports whether any run for the box is queued or running.
func (s *Store) BoxHasRunningAgentRun(ctx context.Context, boxID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs r JOIN conversations c ON c.id=r.conversation_id WHERE c.box_id=? AND r.status IN ('queued','running'))`, boxID).Scan(&exists)
	return exists != 0, err
}

func (s *Store) BoxHasOpenProxySession(ctx context.Context, boxID string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM box_activity opened WHERE opened.box_id=? AND opened.category='access' AND opened.action LIKE '%proxy session opened' AND NOT EXISTS (SELECT 1 FROM box_activity closed WHERE closed.box_id=opened.box_id AND closed.category='access' AND closed.action=REPLACE(opened.action,'opened','closed') AND closed.time>=opened.time))`, boxID).Scan(&exists)
	return exists != 0, err
}

func encodeActivityTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func (s *Store) OpenRunningInterval(ctx context.Context, boxID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO box_running_intervals(id,box_id,started_at) VALUES(?,?,?)`, fmt.Sprintf("run_%d", at.UnixNano()), boxID, encodeActivityTime(at))
	return err
}
func (s *Store) CloseRunningInterval(ctx context.Context, boxID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE box_running_intervals SET stopped_at=? WHERE id=(SELECT id FROM box_running_intervals WHERE box_id=? AND stopped_at IS NULL ORDER BY started_at DESC LIMIT 1)`, encodeActivityTime(at), boxID)
	return err
}

func (s *Store) RunningUsage(ctx context.Context, boxID string, now time.Time) (int64, int64, map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT started_at,stopped_at FROM box_running_intervals WHERE box_id=?`, boxID)
	if err != nil {
		return 0, 0, nil, err
	}
	defer rows.Close()
	total, month := int64(0), int64(0)
	days := map[string]int64{}
	monthStart := time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	window := now.UTC().AddDate(0, 0, -29)
	window = time.Date(window.Year(), window.Month(), window.Day(), 0, 0, 0, 0, time.UTC)
	for rows.Next() {
		var a string
		var b sql.NullString
		if err := rows.Scan(&a, &b); err != nil {
			return 0, 0, nil, err
		}
		start, e := decodeTime(a)
		if e != nil {
			return 0, 0, nil, e
		}
		end := now.UTC()
		if b.Valid {
			end, e = decodeTime(b.String)
			if e != nil {
				return 0, 0, nil, e
			}
		}
		if end.After(now) {
			end = now
		}
		if !end.After(start) {
			continue
		}
		total += int64(end.Sub(start).Seconds())
		lo := start
		if lo.Before(monthStart) {
			lo = monthStart
		}
		if end.After(lo) {
			month += int64(end.Sub(lo).Seconds())
		}
		lo = start
		if lo.Before(window) {
			lo = window
		}
		for d := time.Date(lo.Year(), lo.Month(), lo.Day(), 0, 0, 0, 0, time.UTC); d.Before(end); d = d.AddDate(0, 0, 1) {
			x := lo
			if d.After(x) {
				x = d
			}
			y := end
			if d.AddDate(0, 0, 1).Before(y) {
				y = d.AddDate(0, 0, 1)
			}
			if y.After(x) {
				days[d.Format("2006-01-02")] += int64(y.Sub(x).Seconds())
			}
		}
	}
	return total, month, days, rows.Err()
}
