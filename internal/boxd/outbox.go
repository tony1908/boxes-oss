package boxd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Conversation is a durable provider conversation.
type Conversation struct {
	ID         string    `json:"id"`
	Provider   string    `json:"provider"`
	Model      string    `json:"model,omitempty"`
	Prompts    int       `json:"prompts"`
	Running    bool      `json:"running"`
	LastPrompt string    `json:"lastPrompt,omitempty"`
	Current    bool      `json:"current"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	SessionID  string    `json:"-"`
}

// Run describes one queued or active prompt.
type Run struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversationId"`
	Status         string     `json:"status"`
	Error          string     `json:"error,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
}

// Event is a durable event in the boxd outbox.
type Event struct {
	Cursor         int64          `json:"cursor"`
	ID             string         `json:"id"`
	ConversationID string         `json:"conversationId,omitempty"`
	RunID          string         `json:"runId,omitempty"`
	Type           string         `json:"type"`
	Time           time.Time      `json:"time"`
	Data           map[string]any `json:"data"`
}

// Outbox stores conversations, runs and emitted events in local SQLite.
type Outbox struct{ db *sql.DB }

// OpenOutbox opens (and initializes) the durable local outbox database.
func OpenOutbox(path string) (*Outbox, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS conversations (
 id TEXT PRIMARY KEY, provider TEXT NOT NULL, model TEXT NOT NULL DEFAULT '', prompts INTEGER NOT NULL DEFAULT 0,
 running INTEGER NOT NULL DEFAULT 0, last_prompt TEXT NOT NULL DEFAULT '', current INTEGER NOT NULL DEFAULT 1,
 session_id TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS runs (
 id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL, status TEXT NOT NULL, error TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL, started_at TEXT NOT NULL DEFAULT '', finished_at TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS runs_conversation_id ON runs(conversation_id);
CREATE TABLE IF NOT EXISTS events (
 cursor INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT NOT NULL, conversation_id TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '',
 type TEXT NOT NULL, time TEXT NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS events_conversation_cursor ON events(conversation_id,cursor);`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize boxd outbox: %w", err)
	}
	return &Outbox{db: db}, nil
}

func (o *Outbox) Close() error { return o.db.Close() }

func (o *Outbox) SaveConversation(c Conversation) error {
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	_, err := o.db.Exec(`INSERT INTO conversations(id,provider,model,prompts,running,last_prompt,current,session_id,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET provider=excluded.provider,model=excluded.model,
prompts=excluded.prompts,running=excluded.running,last_prompt=excluded.last_prompt,current=excluded.current,
session_id=excluded.session_id,updated_at=excluded.updated_at`, c.ID, c.Provider, c.Model, c.Prompts, c.Running,
		c.LastPrompt, c.Current, c.SessionID, c.CreatedAt.UTC().Format(time.RFC3339Nano), c.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

func (o *Outbox) SetSessionID(conversationID, sessionID string) error {
	_, err := o.db.Exec(`UPDATE conversations SET session_id=?,updated_at=? WHERE id=?`, sessionID, time.Now().UTC().Format(time.RFC3339Nano), conversationID)
	return err
}

func (o *Outbox) Conversation(id string) (Conversation, error) {
	var c Conversation
	var created, updated string
	err := o.db.QueryRow(`SELECT id,provider,model,prompts,running,last_prompt,current,session_id,created_at,updated_at FROM conversations WHERE id=?`, id).
		Scan(&c.ID, &c.Provider, &c.Model, &c.Prompts, &c.Running, &c.LastPrompt, &c.Current, &c.SessionID, &created, &updated)
	if err != nil {
		return Conversation{}, err
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return c, nil
}

func (o *Outbox) Conversations() ([]Conversation, error) {
	rows, err := o.db.Query(`SELECT id,provider,model,prompts,running,last_prompt,current,session_id,created_at,updated_at FROM conversations ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Conversation
	for rows.Next() {
		var c Conversation
		var created, updated string
		if err := rows.Scan(&c.ID, &c.Provider, &c.Model, &c.Prompts, &c.Running, &c.LastPrompt, &c.Current, &c.SessionID, &created, &updated); err != nil {
			return nil, err
		}
		c.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		c.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		result = append(result, c)
	}
	return result, rows.Err()
}

func (o *Outbox) SaveRun(r Run) error {
	_, err := o.db.Exec(`INSERT INTO runs(id,conversation_id,status,error,created_at,started_at,finished_at) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET status=excluded.status,error=excluded.error,started_at=excluded.started_at,finished_at=excluded.finished_at`,
		r.ID, r.ConversationID, r.Status, r.Error, r.CreatedAt.UTC().Format(time.RFC3339Nano), formatTime(r.StartedAt), formatTime(r.FinishedAt))
	return err
}

func (o *Outbox) Run(id string) (Run, error) {
	var r Run
	var created, started, finished string
	err := o.db.QueryRow(`SELECT id,conversation_id,status,error,created_at,started_at,finished_at FROM runs WHERE id=?`, id).Scan(&r.ID, &r.ConversationID, &r.Status, &r.Error, &created, &started, &finished)
	if err != nil {
		return Run{}, err
	}
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	r.StartedAt = parseTime(started)
	r.FinishedAt = parseTime(finished)
	return r, nil
}

func (o *Outbox) AddEvent(e Event) (Event, error) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	data, err := json.Marshal(e.Data)
	if err != nil {
		return Event{}, err
	}
	result, err := o.db.Exec(`INSERT INTO events(id,conversation_id,run_id,type,time,data) VALUES(?,?,?,?,?,?)`, e.ID, e.ConversationID, e.RunID, e.Type, e.Time.UTC().Format(time.RFC3339Nano), string(data))
	if err != nil {
		return Event{}, err
	}
	e.Cursor, _ = result.LastInsertId()
	return e, nil
}

func (o *Outbox) Events(after int64, conversationID string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := o.db.Query(`SELECT cursor,id,conversation_id,run_id,type,time,data FROM events WHERE cursor>? AND (?='' OR conversation_id=?) ORDER BY cursor LIMIT ?`, after, conversationID, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Event
	for rows.Next() {
		var e Event
		var at, data string
		if err := rows.Scan(&e.Cursor, &e.ID, &e.ConversationID, &e.RunID, &e.Type, &at, &data); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, at)
		if err := json.Unmarshal([]byte(data), &e.Data); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return &t
}
