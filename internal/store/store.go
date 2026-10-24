// Package store provides the SQLite persistence layer for boxes-server.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tony1908/boxes-oss/internal/types"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

const (
	busyTimeoutMilliseconds = 5000
	timeLayout              = time.RFC3339Nano
)

// Store is the persistent SQLite database.
type Store struct {
	db *sql.DB
}

// BoxRecord contains persisted box data that is not part of the public Box API.
type BoxRecord struct {
	types.Box
	DesktopPasswordEnc []byte
	BoxdTokenEnc       []byte
	IdempotencyKey     string
	RunningSeconds     int64
	RunningSince       *time.Time
	SetupStatus        string
	// RestoreSnapshotID is the snapshot the next resume restores from; resume clears it.
	RestoreSnapshotID string
	// Usage totals accumulate at each stop (see server stopSandbox); they are not part of the public box JSON.
	TotalCPUSeconds float64
	TotalRxBytes    int64
	TotalTxBytes    int64
}

// NodeRecord includes the node authentication hash, which must never be returned
// in the public node representation.
type NodeRecord struct {
	Node      types.Node
	TokenHash string
}

// Session stores a hashed session token and its expiration.
type Session struct {
	ID        string
	UserID    string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// APIKeyRecord stores an API key's hash separately from its one-time raw value.
type APIKeyRecord struct {
	types.APIKey
	UserID  string
	KeyHash string
}

// EnvironmentVersion is an immutable persisted environment revision.
type EnvironmentVersion struct {
	EnvironmentID string
	Version       int
	Document      map[string]any
	SecretsEnc    []byte
	CreatedAt     time.Time
}

// HostRecord describes an exposed box port and its optional access token hash.
type HostRecord struct {
	BoxID     string
	Port      int
	Private   bool
	TokenHash string
}

// DesktopSessionRecord stores an encrypted desktop token and its route metadata.
type DesktopSessionRecord struct {
	BoxID     string
	TokenEnc  []byte
	ExpiresAt time.Time
	Public    bool
	Host      string
	Browser   bool
	Profile   string
}

// IdempotencyRecord stores a previously returned response for a request key.
type IdempotencyRecord struct {
	Key       string
	Scope     string
	Response  []byte
	CreatedAt time.Time
}

// Open opens or creates a database, configures SQLite for concurrent server use,
// and applies any embedded migrations not yet recorded in schema_migrations.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("open store: database path is empty")
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		path = filepath.Clean(path)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	cleanup := func(err error) (*Store, error) {
		_ = db.Close()
		return nil, err
	}
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		return cleanup(fmt.Errorf("connect sqlite database: %w", err))
	}
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeoutMilliseconds),
		"PRAGMA journal_mode = WAL",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return cleanup(fmt.Errorf("configure sqlite (%s): %w", pragma, err))
		}
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		return cleanup(err)
	}
	return s, nil
}

// Close releases the database connection.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	entries, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(entries)
	for _, name := range entries {
		version := strings.TrimSuffix(filepath.Base(name), ".sql")
		var exists int
		err := s.db.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = ?`, version).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		script, err := migrations.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, version, encodeTime(time.Now())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}
	}
	return nil
}

// CreateUser inserts a new user.
func (s *Store) CreateUser(ctx context.Context, user types.User) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(id, email, role, password_hash, created_at)
		VALUES(?, ?, ?, ?, ?)`, user.ID, user.Email, user.Role, user.PasswordHash, encodeTime(user.CreatedAt))
	if err != nil {
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

// GetUser returns a user by ID.
func (s *Store) GetUser(ctx context.Context, id string) (types.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT id, email, role, password_hash, created_at FROM users WHERE id = ?`, id))
}

// UserByEmail returns a user by case-insensitive email address.
func (s *Store) UserByEmail(ctx context.Context, email string) (types.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT id, email, role, password_hash, created_at FROM users WHERE email = ? COLLATE NOCASE`, email))
}

// ListUsers returns all users ordered by creation time and ID.
func (s *Store) ListUsers(ctx context.Context) ([]types.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, email, role, password_hash, created_at FROM users ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	users := make([]types.User, 0)
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	return users, nil
}

// DeleteUser deletes a user and cascades to user-owned sessions and API keys.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "users", "id", id)
}

// CreateSession inserts a session. ID must be a hash of the bearer token.
func (s *Store) CreateSession(ctx context.Context, session Session) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(id, user_id, expires_at, created_at) VALUES(?, ?, ?, ?)`,
		session.ID, session.UserID, encodeTime(session.ExpiresAt), encodeTime(session.CreatedAt))
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// GetSession returns a session by its token hash.
func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	var session Session
	var expiresAt, createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, expires_at, created_at FROM sessions WHERE id = ?`, id).
		Scan(&session.ID, &session.UserID, &expiresAt, &createdAt)
	if err != nil {
		return Session{}, err
	}
	if session.ExpiresAt, err = decodeTime(expiresAt); err != nil {
		return Session{}, err
	}
	if session.CreatedAt, err = decodeTime(createdAt); err != nil {
		return Session{}, err
	}
	return session, nil
}

// DeleteSession revokes a session by token hash.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "sessions", "id", id)
}

// DeleteExpiredSessions removes sessions whose expiration is at or before now.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, encodeTime(now))
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return result.RowsAffected()
}

// CreateAPIKey inserts an API key, persisting only keyHash rather than key.Key.
func (s *Store) CreateAPIKey(ctx context.Context, userID string, key types.APIKey, keyHash string) error {
	scopes, err := encodeJSON(key.Scopes)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO api_keys(id, user_id, name, key_hash, scopes, expires_at, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`, key.ID, userID, key.Name, keyHash, scopes,
		nullableTimePtr(key.ExpiresAt), encodeTime(key.CreatedAt))
	if err != nil {
		return fmt.Errorf("create API key: %w", err)
	}
	return nil
}

// APIKeyByHash looks up an API key by its SHA-256 hash.
func (s *Store) APIKeyByHash(ctx context.Context, keyHash string) (APIKeyRecord, error) {
	return scanAPIKey(s.db.QueryRowContext(ctx, apiKeySelect+` WHERE key_hash = ?`, keyHash))
}

// ListAPIKeys returns keys belonging to a user without exposing raw key values.
func (s *Store) ListAPIKeys(ctx context.Context, userID string) ([]APIKeyRecord, error) {
	rows, err := s.db.QueryContext(ctx, apiKeySelect+` WHERE user_id = ? ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list API keys: %w", err)
	}
	defer rows.Close()
	keys := make([]APIKeyRecord, 0)
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, fmt.Errorf("scan API key: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list API keys: %w", err)
	}
	return keys, nil
}

// DeleteAPIKey deletes a key owned by userID.
func (s *Store) DeleteAPIKey(ctx context.Context, userID, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ? AND user_id = ?`, id, userID)
	return checkUpdated(result, err, "delete API key")
}

const apiKeySelect = `SELECT id, user_id, name, key_hash, scopes, expires_at, created_at FROM api_keys`

func scanAPIKey(row interface{ Scan(...any) error }) (APIKeyRecord, error) {
	var record APIKeyRecord
	var scopes, createdAt string
	var expiresAt sql.NullString
	err := row.Scan(&record.ID, &record.UserID, &record.Name, &record.KeyHash, &scopes, &expiresAt, &createdAt)
	if err != nil {
		return APIKeyRecord{}, err
	}
	if err := json.Unmarshal([]byte(scopes), &record.Scopes); err != nil {
		return APIKeyRecord{}, fmt.Errorf("decode API key scopes: %w", err)
	}
	if expiresAt.Valid {
		t, err := decodeTime(expiresAt.String)
		if err != nil {
			return APIKeyRecord{}, err
		}
		record.ExpiresAt = &t
	}
	if record.CreatedAt, err = decodeTime(createdAt); err != nil {
		return APIKeyRecord{}, err
	}
	return record, nil
}

func scanUser(row interface{ Scan(...any) error }) (types.User, error) {
	var user types.User
	var created string
	if err := row.Scan(&user.ID, &user.Email, &user.Role, &user.PasswordHash, &created); err != nil {
		return types.User{}, err
	}
	var err error
	user.CreatedAt, err = decodeTime(created)
	if err != nil {
		return types.User{}, err
	}
	return user, nil
}

// CreateNode inserts a node including its token hash.
func (s *Store) CreateNode(ctx context.Context, node NodeRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO nodes(id, name, host, host_base, token_hash, status,
		cpus, memory_bytes, free_memory_bytes, disk_free_bytes, last_seen, mem_available_bytes, swap_free_bytes, memory_pressure_some_avg10)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, node.Node.ID, node.Node.Name,
		node.Node.Host, node.Node.HostBase, node.TokenHash, node.Node.Status, node.Node.CPUs,
		node.Node.MemoryBytes, node.Node.FreeMemoryBytes, node.Node.DiskFreeBytes,
		nullableTime(node.Node.LastSeen), node.Node.MemAvailableBytes, node.Node.SwapFreeBytes, node.Node.MemoryPressureSomeAvg10)
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	return nil
}

// GetNode returns a node by ID.
func (s *Store) GetNode(ctx context.Context, id string) (NodeRecord, error) {
	return scanNode(s.db.QueryRowContext(ctx, `SELECT id, name, host, host_base, token_hash, status,
		cpus, memory_bytes, free_memory_bytes, disk_free_bytes, last_seen, mem_available_bytes, swap_free_bytes, memory_pressure_some_avg10 FROM nodes WHERE id = ?`, id))
}

// ListNodes returns nodes ordered by ID.
func (s *Store) ListNodes(ctx context.Context) ([]NodeRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, host, host_base, token_hash, status,
		cpus, memory_bytes, free_memory_bytes, disk_free_bytes, last_seen, mem_available_bytes, swap_free_bytes, memory_pressure_some_avg10 FROM nodes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()
	nodes := make([]NodeRecord, 0)
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	return nodes, nil
}

// UpdateNode updates all mutable node fields and its token hash.
func (s *Store) UpdateNode(ctx context.Context, node NodeRecord) error {
	result, err := s.db.ExecContext(ctx, `UPDATE nodes SET name=?, host=?, host_base=?, token_hash=?, status=?,
		cpus=?, memory_bytes=?, free_memory_bytes=?, disk_free_bytes=?, last_seen=?, mem_available_bytes=?, swap_free_bytes=?, memory_pressure_some_avg10=? WHERE id=?`,
		node.Node.Name, node.Node.Host, node.Node.HostBase, node.TokenHash, node.Node.Status,
		node.Node.CPUs, node.Node.MemoryBytes, node.Node.FreeMemoryBytes, node.Node.DiskFreeBytes,
		nullableTime(node.Node.LastSeen), node.Node.MemAvailableBytes, node.Node.SwapFreeBytes, node.Node.MemoryPressureSomeAvg10, node.Node.ID)
	return checkUpdated(result, err, "update node")
}

func scanNode(row interface{ Scan(...any) error }) (NodeRecord, error) {
	var record NodeRecord
	var lastSeen sql.NullString
	err := row.Scan(&record.Node.ID, &record.Node.Name, &record.Node.Host, &record.Node.HostBase,
		&record.TokenHash, &record.Node.Status, &record.Node.CPUs, &record.Node.MemoryBytes,
		&record.Node.FreeMemoryBytes, &record.Node.DiskFreeBytes, &lastSeen, &record.Node.MemAvailableBytes, &record.Node.SwapFreeBytes, &record.Node.MemoryPressureSomeAvg10)
	if err != nil {
		return NodeRecord{}, err
	}
	if lastSeen.Valid {
		record.Node.LastSeen, err = decodeTime(lastSeen.String)
		if err != nil {
			return NodeRecord{}, err
		}
	}
	return record, nil
}

// CreateBox inserts a box. Its node must already exist.
func (s *Store) CreateBox(ctx context.Context, record BoxRecord) error {
	box := record.Box
	var envID any
	if box.EnvironmentID != "" {
		envID = box.EnvironmentID
	}
	var idempotencyKey any
	if record.IdempotencyKey != "" {
		idempotencyKey = record.IdempotencyKey
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO boxes(id, node_id, type, state, image, env_id, env_version,
		ttl_seconds, idle_stop_seconds, expires_at, desktop_password_enc, boxd_token_enc, idempotency_key, from_snapshot, created_at, stopped_at,
		running_seconds, running_since, setup_status, error_code, error, tags, network_policy, source, keep_snapshots, restore_snapshot_id)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, box.ID, box.NodeID, box.Type,
		string(box.State), box.Image, envID, box.EnvironmentVersion, box.TTLSeconds, box.IdleStopSeconds, nullableTimePtr(box.ExpiresAt),
		nonNilBytes(record.DesktopPasswordEnc), nonNilBytes(record.BoxdTokenEnc), idempotencyKey,
		box.FromSnapshot, encodeTime(box.CreatedAt), nullableTimePtr(box.StoppedAt), record.RunningSeconds,
		nullableTimePtr(record.RunningSince), setupStatus(record.SetupStatus), box.ErrorCode, box.Error, tagsJSON(box.Tags), networkPolicyJSON(box.NetworkPolicy), sourceJSON(box.Source), box.KeepSnapshots, record.RestoreSnapshotID)
	if err != nil {
		return fmt.Errorf("create box: %w", err)
	}
	return nil
}

// GetBox returns a box by ID.
func (s *Store) GetBox(ctx context.Context, id string) (BoxRecord, error) {
	return scanBox(s.db.QueryRowContext(ctx, boxSelect+` WHERE id = ?`, id))
}

// BoxByIdempotencyKey returns the box associated with an idempotency key.
func (s *Store) BoxByIdempotencyKey(ctx context.Context, key string) (BoxRecord, error) {
	return scanBox(s.db.QueryRowContext(ctx, boxSelect+` WHERE idempotency_key = ?`, key))
}

// ListBoxes returns all boxes, or only boxes on nodeID when it is non-empty.
func (s *Store) ListBoxes(ctx context.Context, nodeID string) ([]BoxRecord, error) {
	query, args := boxSelect+` ORDER BY created_at, id`, []any{}
	if nodeID != "" {
		query, args = boxSelect+` WHERE node_id = ? ORDER BY created_at, id`, []any{nodeID}
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list boxes: %w", err)
	}
	defer rows.Close()
	boxes := make([]BoxRecord, 0)
	for rows.Next() {
		box, err := scanBox(rows)
		if err != nil {
			return nil, fmt.Errorf("scan box: %w", err)
		}
		boxes = append(boxes, box)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list boxes: %w", err)
	}
	return boxes, nil
}

// UpdateBox updates the complete stored box record.
func (s *Store) UpdateBox(ctx context.Context, record BoxRecord) error {
	box := record.Box
	var envID, idempotencyKey any
	if box.EnvironmentID != "" {
		envID = box.EnvironmentID
	}
	if record.IdempotencyKey != "" {
		idempotencyKey = record.IdempotencyKey
	}
	result, err := s.db.ExecContext(ctx, `UPDATE boxes SET node_id=?, type=?, state=?, image=?, env_id=?, env_version=?,
		ttl_seconds=?, idle_stop_seconds=?, expires_at=?, desktop_password_enc=?, boxd_token_enc=?, idempotency_key=?, from_snapshot=?, created_at=?, stopped_at=?,
		running_seconds=?, running_since=?, setup_status=?, error_code=?, error=?, tags=?, network_policy=?, source=?, keep_snapshots=?, restore_snapshot_id=?, total_cpu_seconds=?, total_rx_bytes=?, total_tx_bytes=? WHERE id=?`,
		box.NodeID, box.Type, string(box.State), box.Image, envID, box.EnvironmentVersion, box.TTLSeconds,
		box.IdleStopSeconds, nullableTimePtr(box.ExpiresAt), nonNilBytes(record.DesktopPasswordEnc), nonNilBytes(record.BoxdTokenEnc),
		idempotencyKey, box.FromSnapshot, encodeTime(box.CreatedAt), nullableTimePtr(box.StoppedAt), record.RunningSeconds,
		nullableTimePtr(record.RunningSince), setupStatus(record.SetupStatus), box.ErrorCode, box.Error, tagsJSON(box.Tags), networkPolicyJSON(box.NetworkPolicy), sourceJSON(box.Source), box.KeepSnapshots, record.RestoreSnapshotID, record.TotalCPUSeconds, record.TotalRxBytes, record.TotalTxBytes, box.ID)
	return checkUpdated(result, err, "update box")
}

// DeleteBox deletes a box and its dependent data.
func (s *Store) DeleteBox(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "boxes", "id", id)
}

const boxSelect = `SELECT id, node_id, type, state, image, env_id, env_version, ttl_seconds, idle_stop_seconds,
	expires_at, desktop_password_enc, boxd_token_enc, idempotency_key, from_snapshot, created_at, stopped_at,
		running_seconds, running_since, setup_status, error_code, error, tags, network_policy, source, keep_snapshots, restore_snapshot_id, total_cpu_seconds, total_rx_bytes, total_tx_bytes FROM boxes`

func scanBox(row interface{ Scan(...any) error }) (BoxRecord, error) {
	var record BoxRecord
	var state string
	var envID, idempotencyKey, expires, stopped, runningSince sql.NullString
	var created, tags string
	var networkPolicy, source sql.NullString
	err := row.Scan(&record.Box.ID, &record.Box.NodeID, &record.Box.Type, &state, &record.Box.Image,
		&envID, &record.Box.EnvironmentVersion, &record.Box.TTLSeconds, &record.Box.IdleStopSeconds, &expires,
		&record.DesktopPasswordEnc, &record.BoxdTokenEnc, &idempotencyKey, &record.Box.FromSnapshot,
		&created, &stopped, &record.RunningSeconds, &runningSince, &record.SetupStatus,
		&record.Box.ErrorCode, &record.Box.Error, &tags, &networkPolicy, &source, &record.Box.KeepSnapshots, &record.RestoreSnapshotID,
		&record.TotalCPUSeconds, &record.TotalRxBytes, &record.TotalTxBytes)
	if err != nil {
		return BoxRecord{}, err
	}
	if networkPolicy.Valid {
		var policy types.NetworkPolicy
		if err := json.Unmarshal([]byte(networkPolicy.String), &policy); err != nil {
			return BoxRecord{}, fmt.Errorf("decode box network policy: %w", err)
		}
		record.Box.NetworkPolicy = &policy
	}
	if source.Valid {
		var boxSource types.BoxSource
		if err := json.Unmarshal([]byte(source.String), &boxSource); err != nil {
			return BoxRecord{}, fmt.Errorf("decode box source: %w", err)
		}
		record.Box.Source = &boxSource
	}
	if err := json.Unmarshal([]byte(tags), &record.Box.Tags); err != nil {
		return BoxRecord{}, fmt.Errorf("decode box tags: %w", err)
	}
	if len(record.Box.Tags) == 0 {
		record.Box.Tags = nil
	}
	record.Box.State = types.BoxState(state)
	if envID.Valid {
		record.Box.EnvironmentID = envID.String
	}
	if idempotencyKey.Valid {
		record.IdempotencyKey = idempotencyKey.String
	}
	record.Box.CreatedAt, err = decodeTime(created)
	if err != nil {
		return BoxRecord{}, err
	}
	if expires.Valid {
		t, err := decodeTime(expires.String)
		if err != nil {
			return BoxRecord{}, err
		}
		record.Box.ExpiresAt = &t
	}
	if stopped.Valid {
		t, err := decodeTime(stopped.String)
		if err != nil {
			return BoxRecord{}, err
		}
		record.Box.StoppedAt = &t
	}
	if runningSince.Valid {
		t, err := decodeTime(runningSince.String)
		if err != nil {
			return BoxRecord{}, err
		}
		record.RunningSince = &t
	}
	if record.SetupStatus == "" {
		record.SetupStatus = "none"
	}
	return record, nil
}

// networkPolicyJSON encodes a policy as JSON; a nil policy stores NULL.
func networkPolicyJSON(policy *types.NetworkPolicy) any {
	if policy == nil {
		return nil
	}
	data, err := json.Marshal(policy)
	if err != nil {
		return nil
	}
	return string(data)
}

// sourceJSON encodes a box source as JSON; a nil source stores NULL.
func sourceJSON(source *types.BoxSource) any {
	if source == nil {
		return nil
	}
	data, err := json.Marshal(source)
	if err != nil {
		return nil
	}
	return string(data)
}

// tagsJSON encodes tags as a JSON object; nil and empty maps both store "{}".
func tagsJSON(tags map[string]string) string {
	if len(tags) == 0 {
		return "{}"
	}
	data, err := json.Marshal(tags)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func setupStatus(value string) string {
	if value == "" {
		return "none"
	}
	return value
}

// CreateEnvironment stores the environment and its initial immutable version.
func (s *Store) CreateEnvironment(ctx context.Context, env types.Environment, secretsEnc []byte) error {
	if env.Version == 0 {
		env.Version = 1
	}
	if env.CreatedAt.IsZero() {
		env.CreatedAt = time.Now().UTC()
	}
	if env.UpdatedAt.IsZero() {
		env.UpdatedAt = env.CreatedAt
	}
	document, err := encodeJSON(env.Document)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create environment: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO environments(id, name, version, created_at, updated_at) VALUES(?, ?, ?, ?, ?)`,
		env.ID, env.Name, env.Version, encodeTime(env.CreatedAt), encodeTime(env.UpdatedAt)); err != nil {
		return fmt.Errorf("create environment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO environment_versions(environment_id, version, document, secrets_enc, created_at)
		VALUES(?, ?, ?, ?, ?)`, env.ID, env.Version, document, nonNilBytes(secretsEnc), encodeTime(env.CreatedAt)); err != nil {
		return fmt.Errorf("create environment version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create environment: commit: %w", err)
	}
	return nil
}

// GetEnvironment returns the current environment document.
func (s *Store) GetEnvironment(ctx context.Context, id string) (types.Environment, error) {
	var env types.Environment
	var createdAt, updatedAt string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, version, created_at, updated_at FROM environments WHERE id = ?`, id).
		Scan(&env.ID, &env.Name, &env.Version, &createdAt, &updatedAt)
	if err != nil {
		return types.Environment{}, err
	}
	if env.CreatedAt, err = decodeTime(createdAt); err != nil {
		return types.Environment{}, err
	}
	if env.UpdatedAt, err = decodeTime(updatedAt); err != nil {
		return types.Environment{}, err
	}
	version, err := s.GetEnvironmentVersion(ctx, id, env.Version)
	if err != nil {
		return types.Environment{}, err
	}
	env.Document = version.Document
	return env, nil
}

// ListEnvironments returns environments ordered by name.
func (s *Store) ListEnvironments(ctx context.Context) ([]types.Environment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, version FROM environments ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	defer rows.Close()
	environments := make([]types.Environment, 0)
	for rows.Next() {
		var env types.Environment
		if err := rows.Scan(&env.ID, &env.Name, &env.Version); err != nil {
			return nil, fmt.Errorf("scan environment: %w", err)
		}
		environments = append(environments, env)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list environments: %w", err)
	}
	return environments, nil
}

// CreateEnvironmentVersion appends an immutable version and makes it current.
func (s *Store) CreateEnvironmentVersion(ctx context.Context, version EnvironmentVersion) error {
	if version.Version < 1 {
		return errors.New("create environment version: version must be positive")
	}
	if version.CreatedAt.IsZero() {
		version.CreatedAt = time.Now().UTC()
	}
	document, err := encodeJSON(version.Document)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create environment version: begin transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO environment_versions(environment_id, version, document, secrets_enc, created_at)
		VALUES(?, ?, ?, ?, ?)`, version.EnvironmentID, version.Version, document, nonNilBytes(version.SecretsEnc), encodeTime(version.CreatedAt)); err != nil {
		return fmt.Errorf("create environment version: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE environments SET version = ?, updated_at = ? WHERE id = ? AND version < ?`,
		version.Version, encodeTime(version.CreatedAt), version.EnvironmentID, version.Version)
	if err != nil {
		return fmt.Errorf("update current environment version: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update current environment version: %w", err)
	}
	if count != 1 {
		return errors.New("create environment version: version must exceed the current version")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create environment version: commit: %w", err)
	}
	return nil
}

// GetEnvironmentVersion loads one immutable environment revision, including its encrypted secrets.
func (s *Store) GetEnvironmentVersion(ctx context.Context, id string, version int) (EnvironmentVersion, error) {
	var record EnvironmentVersion
	var document string
	var createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT environment_id, version, document, secrets_enc, created_at
		FROM environment_versions WHERE environment_id = ? AND version = ?`, id, version).
		Scan(&record.EnvironmentID, &record.Version, &document, &record.SecretsEnc, &createdAt)
	if err != nil {
		return EnvironmentVersion{}, err
	}
	if err := json.Unmarshal([]byte(document), &record.Document); err != nil {
		return EnvironmentVersion{}, fmt.Errorf("decode environment document: %w", err)
	}
	if record.CreatedAt, err = decodeTime(createdAt); err != nil {
		return EnvironmentVersion{}, err
	}
	return record, nil
}

// DeleteEnvironment deletes an environment and its versions. Boxes referencing it have env_id cleared.
func (s *Store) DeleteEnvironment(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "environments", "id", id)
}

// CreateSnapshot inserts a snapshot record.
func (s *Store) CreateSnapshot(ctx context.Context, snapshot types.Snapshot) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO snapshots(id, box_id, restic_id, size_bytes, created_at, repository, storage_env_enc)
		VALUES(?, ?, ?, ?, ?, ?, ?)`, snapshot.ID, snapshot.BoxID, snapshot.ResticID, snapshot.SizeBytes,
		encodeTime(snapshot.CreatedAt), snapshot.Repository, nonNilBytes(snapshot.StorageEnvEnc))
	if err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}
	return nil
}

// GetSnapshot returns a snapshot by ID.
func (s *Store) GetSnapshot(ctx context.Context, id string) (types.Snapshot, error) {
	return scanSnapshot(s.db.QueryRowContext(ctx, snapshotSelect+` WHERE id = ?`, id))
}

// ListSnapshots returns snapshots for a box in creation order.
func (s *Store) ListSnapshots(ctx context.Context, boxID string) ([]types.Snapshot, error) {
	rows, err := s.db.QueryContext(ctx, snapshotSelect+` WHERE box_id = ? ORDER BY created_at, id`, boxID)
	if err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	defer rows.Close()
	snapshots := make([]types.Snapshot, 0)
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("scan snapshot: %w", err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	return snapshots, nil
}

// ListSnapshotPage returns completed snapshots across extant boxes, newest first.
func (s *Store) ListSnapshotPage(ctx context.Context, beforeCreatedAt string, beforeID string, limit int) ([]types.Snapshot, error) {
	base := `SELECT snapshots.id, snapshots.box_id, snapshots.restic_id, snapshots.size_bytes, snapshots.created_at, snapshots.repository, snapshots.storage_env_enc
		FROM snapshots JOIN boxes ON boxes.id = snapshots.box_id`
	args := []any{}
	if beforeCreatedAt != "" {
		base += ` WHERE snapshots.created_at < ? OR (snapshots.created_at = ? AND snapshots.id < ?)`
		args = append(args, beforeCreatedAt, beforeCreatedAt, beforeID)
	}
	base += ` ORDER BY snapshots.created_at DESC, snapshots.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, base, args...)
	if err != nil {
		return nil, fmt.Errorf("list snapshot page: %w", err)
	}
	defer rows.Close()
	snapshots := make([]types.Snapshot, 0, limit)
	for rows.Next() {
		snapshot, err := scanSnapshot(rows)
		if err != nil {
			return nil, fmt.Errorf("scan snapshot: %w", err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list snapshot page: %w", err)
	}
	return snapshots, nil
}

// LatestSnapshot returns the newest completed snapshot for a box.
func (s *Store) LatestSnapshot(ctx context.Context, boxID string) (types.Snapshot, error) {
	return scanSnapshot(s.db.QueryRowContext(ctx, snapshotSelect+` WHERE box_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, boxID))
}

// DeleteSnapshot deletes a snapshot and named references to it.
func (s *Store) DeleteSnapshot(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "snapshots", "id", id)
}

func (s *Store) ZeroDataRetention(ctx context.Context) (bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM account_settings WHERE key='zero_data_retention'`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return value == "true", err
}

func (s *Store) SetZeroDataRetention(ctx context.Context, enabled bool) error {
	value := "false"
	if enabled {
		value = "true"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO account_settings(key,value) VALUES('zero_data_retention',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, value)
	return err
}

// BackfillSnapshotStorage associates snapshots from older schema versions with
// the repository and credentials that were configured when the server started.
func (s *Store) BackfillSnapshotStorage(ctx context.Context, repository string, encryptedEnv []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE snapshots SET repository = ?, storage_env_enc = ? WHERE repository = ''`, repository, nonNilBytes(encryptedEnv))
	if err != nil {
		return fmt.Errorf("backfill snapshot storage: %w", err)
	}
	return nil
}

const snapshotSelect = `SELECT id, box_id, restic_id, size_bytes, created_at, repository, storage_env_enc FROM snapshots`

func scanSnapshot(row interface{ Scan(...any) error }) (types.Snapshot, error) {
	var snapshot types.Snapshot
	var createdAt string
	if err := row.Scan(&snapshot.ID, &snapshot.BoxID, &snapshot.ResticID, &snapshot.SizeBytes, &createdAt, &snapshot.Repository, &snapshot.StorageEnvEnc); err != nil {
		return types.Snapshot{}, err
	}
	var err error
	snapshot.CreatedAt, err = decodeTime(createdAt)
	return snapshot, err
}

// CreateNamedSnapshot pins a snapshot under a unique name.
func (s *Store) CreateNamedSnapshot(ctx context.Context, snapshot types.NamedSnapshot) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO named_snapshots(name, snapshot_id, created_at) VALUES(?, ?, ?)`,
		snapshot.Name, snapshot.SnapshotID, encodeTime(snapshot.CreatedAt))
	if err != nil {
		return fmt.Errorf("create named snapshot: %w", err)
	}
	return nil
}

// GetNamedSnapshot returns a named snapshot by name.
func (s *Store) GetNamedSnapshot(ctx context.Context, name string) (types.NamedSnapshot, error) {
	var snapshot types.NamedSnapshot
	var createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT name, snapshot_id, created_at FROM named_snapshots WHERE name = ?`, name).
		Scan(&snapshot.Name, &snapshot.SnapshotID, &createdAt)
	if err != nil {
		return types.NamedSnapshot{}, err
	}
	snapshot.CreatedAt, err = decodeTime(createdAt)
	return snapshot, err
}

// ListNamedSnapshots returns named snapshots ordered by name.
func (s *Store) ListNamedSnapshots(ctx context.Context) ([]types.NamedSnapshot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, snapshot_id, created_at FROM named_snapshots ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list named snapshots: %w", err)
	}
	defer rows.Close()
	snapshots := make([]types.NamedSnapshot, 0)
	for rows.Next() {
		var snapshot types.NamedSnapshot
		var createdAt string
		if err := rows.Scan(&snapshot.Name, &snapshot.SnapshotID, &createdAt); err != nil {
			return nil, fmt.Errorf("scan named snapshot: %w", err)
		}
		if snapshot.CreatedAt, err = decodeTime(createdAt); err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list named snapshots: %w", err)
	}
	return snapshots, nil
}

// DeleteNamedSnapshot removes a name without deleting its underlying snapshot.
func (s *Store) DeleteNamedSnapshot(ctx context.Context, name string) error {
	return deleteByID(ctx, s.db, "named_snapshots", "name", name)
}

// CreateConversation inserts a conversation.
func (s *Store) CreateConversation(ctx context.Context, conversation types.Conversation) error {
	if conversation.CreatedAt.IsZero() {
		conversation.CreatedAt = time.Now().UTC()
	}
	if conversation.UpdatedAt.IsZero() {
		conversation.UpdatedAt = conversation.CreatedAt
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO conversations(id, box_id, provider, model, prompts, running,
		last_prompt, current, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		conversation.ID, conversation.BoxID, conversation.Provider, conversation.Model, conversation.Prompts,
		boolInt(conversation.Running), conversation.LastPrompt, boolInt(conversation.Current),
		encodeTime(conversation.CreatedAt), encodeTime(conversation.UpdatedAt))
	if err != nil {
		return fmt.Errorf("create conversation: %w", err)
	}
	return nil
}

// GetConversation returns a conversation by ID.
func (s *Store) GetConversation(ctx context.Context, id string) (types.Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx, conversationSelect+` WHERE id = ?`, id))
}

// ListConversations returns conversations for a box ordered by most recently updated.
func (s *Store) ListConversations(ctx context.Context, boxID string) ([]types.Conversation, error) {
	rows, err := s.db.QueryContext(ctx, conversationSelect+` WHERE box_id = ? ORDER BY updated_at DESC, id`, boxID)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()
	conversations := make([]types.Conversation, 0)
	for rows.Next() {
		conversation, err := scanConversation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan conversation: %w", err)
		}
		conversations = append(conversations, conversation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	return conversations, nil
}

// UpdateConversation updates mutable conversation state.
func (s *Store) UpdateConversation(ctx context.Context, conversation types.Conversation) error {
	if conversation.UpdatedAt.IsZero() {
		conversation.UpdatedAt = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE conversations SET provider=?, model=?, prompts=?, running=?,
		last_prompt=?, current=?, updated_at=? WHERE id=?`, conversation.Provider, conversation.Model,
		conversation.Prompts, boolInt(conversation.Running), conversation.LastPrompt,
		boolInt(conversation.Current), encodeTime(conversation.UpdatedAt), conversation.ID)
	return checkUpdated(result, err, "update conversation")
}

// DeleteConversation deletes a conversation and its runs.
func (s *Store) DeleteConversation(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "conversations", "id", id)
}

const conversationSelect = `SELECT id, box_id, provider, model, prompts, running, last_prompt, current, created_at, updated_at FROM conversations`

func scanConversation(row interface{ Scan(...any) error }) (types.Conversation, error) {
	var conversation types.Conversation
	var running, current int
	var createdAt, updatedAt string
	err := row.Scan(&conversation.ID, &conversation.BoxID, &conversation.Provider, &conversation.Model,
		&conversation.Prompts, &running, &conversation.LastPrompt, &current, &createdAt, &updatedAt)
	if err != nil {
		return types.Conversation{}, err
	}
	conversation.Running = running != 0
	conversation.Current = current != 0
	if conversation.CreatedAt, err = decodeTime(createdAt); err != nil {
		return types.Conversation{}, err
	}
	if conversation.UpdatedAt, err = decodeTime(updatedAt); err != nil {
		return types.Conversation{}, err
	}
	return conversation, nil
}

// CreateRun inserts a run.
func (s *Store) CreateRun(ctx context.Context, run types.Run) error {
	return s.CreateRunWithStopWhenDone(ctx, run, false)
}

// CreateRunWithStopWhenDone inserts a run and its completion policy atomically.
func (s *Store) CreateRunWithStopWhenDone(ctx context.Context, run types.Run, stopWhenDone bool) error {
	if run.CreatedAt.IsZero() {
		run.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs(id, conversation_id, status, error, created_at, started_at, finished_at, stop_when_done)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, run.ID, run.ConversationID, run.Status, run.Error,
		encodeTime(run.CreatedAt), nullableTimePtr(run.StartedAt), nullableTimePtr(run.FinishedAt), boolInt(stopWhenDone))
	if err != nil {
		return fmt.Errorf("create run: %w", err)
	}
	return nil
}

// RunStopWhenDone reports whether a run should stop and snapshot its box at completion.
func (s *Store) RunStopWhenDone(ctx context.Context, runID string) (bool, error) {
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT stop_when_done FROM runs WHERE id = ?`, runID).Scan(&enabled)
	return enabled != 0, err
}

// SetRunStopWhenDone updates the completion policy of a stored run.
func (s *Store) SetRunStopWhenDone(ctx context.Context, runID string, enabled bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET stop_when_done = ? WHERE id = ?`, boolInt(enabled), runID)
	return checkUpdated(result, err, "update run completion policy")
}

// GetRun returns a run by ID.
func (s *Store) GetRun(ctx context.Context, id string) (types.Run, error) {
	return scanRun(s.db.QueryRowContext(ctx, runSelect+` WHERE id = ?`, id))
}

// ListRuns returns runs for a conversation in creation order.
func (s *Store) ListRuns(ctx context.Context, conversationID string) ([]types.Run, error) {
	rows, err := s.db.QueryContext(ctx, runSelect+` WHERE conversation_id = ? ORDER BY created_at, id`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	runs := make([]types.Run, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	return runs, nil
}

// UpdateRun updates a run's status and completion details.
func (s *Store) UpdateRun(ctx context.Context, run types.Run) error {
	result, err := s.db.ExecContext(ctx, `UPDATE runs SET status=?, error=?, started_at=?, finished_at=? WHERE id=?`,
		run.Status, run.Error, nullableTimePtr(run.StartedAt), nullableTimePtr(run.FinishedAt), run.ID)
	return checkUpdated(result, err, "update run")
}

// DeleteRun deletes a run.
func (s *Store) DeleteRun(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "runs", "id", id)
}

const runSelect = `SELECT id, conversation_id, status, error, created_at, started_at, finished_at FROM runs`

func scanRun(row interface{ Scan(...any) error }) (types.Run, error) {
	var run types.Run
	var createdAt string
	var startedAt, finishedAt sql.NullString
	err := row.Scan(&run.ID, &run.ConversationID, &run.Status, &run.Error, &createdAt, &startedAt, &finishedAt)
	if err != nil {
		return types.Run{}, err
	}
	if run.CreatedAt, err = decodeTime(createdAt); err != nil {
		return types.Run{}, err
	}
	if startedAt.Valid {
		t, err := decodeTime(startedAt.String)
		if err != nil {
			return types.Run{}, err
		}
		run.StartedAt = &t
	}
	if finishedAt.Valid {
		t, err := decodeTime(finishedAt.String)
		if err != nil {
			return types.Run{}, err
		}
		run.FinishedAt = &t
	}
	return run, nil
}

// CreateEvent appends an event and returns its monotonically increasing cursor.
func (s *Store) CreateEvent(ctx context.Context, event types.Event) (int64, error) {
	data, err := encodeJSON(event.Data)
	if err != nil {
		return 0, err
	}
	var conversationID any
	if event.ConversationID != "" {
		conversationID = event.ConversationID
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO events(id, box_id, conversation_id, run_id, type, time, data)
		VALUES(?, ?, ?, ?, ?, ?, ?)`, event.ID, event.BoxID, conversationID, nullableString(event.RunID),
		event.Type, encodeTime(event.Time), data)
	if err != nil {
		return 0, fmt.Errorf("create event: %w", err)
	}
	cursor, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("create event: get cursor: %w", err)
	}
	return cursor, nil
}

// ListEvents returns a box's events after cursor in ascending cursor order.
func (s *Store) ListEvents(ctx context.Context, boxID string, after int64, limit int) ([]types.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT cursor, id, box_id, conversation_id, run_id, type, time, data
		FROM events WHERE box_id = ? AND cursor > ? ORDER BY cursor LIMIT ?`, boxID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	events := make([]types.Event, 0)
	for rows.Next() {
		var event types.Event
		var conversationID, runID sql.NullString
		var eventTime, data string
		if err := rows.Scan(&event.Cursor, &event.ID, &event.BoxID, &conversationID, &runID, &event.Type, &eventTime, &data); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		if conversationID.Valid {
			event.ConversationID = conversationID.String
		}
		if runID.Valid {
			event.RunID = runID.String
		}
		if event.Time, err = decodeTime(eventTime); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &event.Data); err != nil {
			return nil, fmt.Errorf("decode event data: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	return events, nil
}

// GetTerminalRunEvent returns the latest stored terminal event for a run.
func (s *Store) GetTerminalRunEvent(ctx context.Context, boxID, runID string) (types.Event, error) {
	var event types.Event
	var conversationID, storedRunID sql.NullString
	var eventTime, data string
	err := s.db.QueryRowContext(ctx, `SELECT cursor, id, box_id, conversation_id, run_id, type, time, data
		FROM events WHERE box_id = ? AND run_id = ? AND type IN ('turn.completed', 'turn.failed', 'turn.interrupted', 'interrupted')
		ORDER BY cursor DESC LIMIT 1`, boxID, runID).Scan(&event.Cursor, &event.ID, &event.BoxID, &conversationID, &storedRunID, &event.Type, &eventTime, &data)
	if err != nil {
		return types.Event{}, err
	}
	if conversationID.Valid {
		event.ConversationID = conversationID.String
	}
	if storedRunID.Valid {
		event.RunID = storedRunID.String
	}
	if event.Time, err = decodeTime(eventTime); err != nil {
		return types.Event{}, err
	}
	if err := json.Unmarshal([]byte(data), &event.Data); err != nil {
		return types.Event{}, fmt.Errorf("decode event data: %w", err)
	}
	return event, nil
}

// CreateHost inserts a box's exposed port configuration.
func (s *Store) SaveDesktopSession(ctx context.Context, session DesktopSessionRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO desktop_sessions(box_id, token_enc, expires_at, public, host, browser, profile)
		VALUES(?, ?, ?, ?, ?, ?, ?) ON CONFLICT(box_id) DO UPDATE SET token_enc=excluded.token_enc, expires_at=excluded.expires_at,
		public=excluded.public, host=excluded.host, browser=excluded.browser, profile=excluded.profile`,
		session.BoxID, nonNilBytes(session.TokenEnc), encodeTime(session.ExpiresAt), boolInt(session.Public), session.Host, boolInt(session.Browser), session.Profile)
	if err != nil {
		return fmt.Errorf("save desktop session: %w", err)
	}
	return nil
}

func (s *Store) ListDesktopSessions(ctx context.Context) ([]DesktopSessionRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT box_id, token_enc, expires_at, public, host, browser, profile FROM desktop_sessions`)
	if err != nil {
		return nil, fmt.Errorf("list desktop sessions: %w", err)
	}
	defer rows.Close()
	var sessions []DesktopSessionRecord
	for rows.Next() {
		var item DesktopSessionRecord
		var expiry string
		var public, browser int
		if err := rows.Scan(&item.BoxID, &item.TokenEnc, &expiry, &public, &item.Host, &browser, &item.Profile); err != nil {
			return nil, err
		}
		item.ExpiresAt, err = decodeTime(expiry)
		if err != nil {
			return nil, err
		}
		item.Public, item.Browser = public != 0, browser != 0
		sessions = append(sessions, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sessions, nil
}

func (s *Store) DeleteDesktopSession(ctx context.Context, boxID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM desktop_sessions WHERE box_id=?`, boxID)
	if err != nil {
		return fmt.Errorf("delete desktop session: %w", err)
	}
	return nil
}

// CreateHost inserts a box's exposed port configuration.
func (s *Store) CreateHost(ctx context.Context, host HostRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO hosts(box_id, port, private, token_hash) VALUES(?, ?, ?, ?)`,
		host.BoxID, host.Port, boolInt(host.Private), host.TokenHash)
	if err != nil {
		return fmt.Errorf("create host: %w", err)
	}
	return nil
}

// GetHost retrieves a configured host by box and port.
func (s *Store) GetHost(ctx context.Context, boxID string, port int) (HostRecord, error) {
	return scanHost(s.db.QueryRowContext(ctx, `SELECT box_id, port, private, token_hash FROM hosts WHERE box_id = ? AND port = ?`, boxID, port))
}

// ListHosts returns host configurations for a box ordered by port.
func (s *Store) ListHosts(ctx context.Context, boxID string) ([]HostRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT box_id, port, private, token_hash FROM hosts WHERE box_id = ? ORDER BY port`, boxID)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	defer rows.Close()
	hosts := make([]HostRecord, 0)
	for rows.Next() {
		host, err := scanHost(rows)
		if err != nil {
			return nil, fmt.Errorf("scan host: %w", err)
		}
		hosts = append(hosts, host)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	return hosts, nil
}

// DeleteHost removes a host by its box and port.
func (s *Store) DeleteHost(ctx context.Context, boxID string, port int) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM hosts WHERE box_id = ? AND port = ?`, boxID, port)
	return checkUpdated(result, err, "delete host")
}

func scanHost(row interface{ Scan(...any) error }) (HostRecord, error) {
	var host HostRecord
	var private int
	if err := row.Scan(&host.BoxID, &host.Port, &private, &host.TokenHash); err != nil {
		return HostRecord{}, err
	}
	host.Private = private != 0
	return host, nil
}

// CreateWebhook inserts a webhook destination and its signing secret.
func (s *Store) CreateWebhook(ctx context.Context, webhook types.Webhook) error {
	events, err := encodeJSON(webhook.Events)
	if err != nil {
		return err
	}
	if webhook.CreatedAt.IsZero() {
		webhook.CreatedAt = time.Now().UTC()
	}
	if webhook.UpdatedAt.IsZero() {
		webhook.UpdatedAt = webhook.CreatedAt
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO webhooks(id, url, events, secret, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?)`,
		webhook.ID, webhook.URL, events, webhook.Secret, encodeTime(webhook.CreatedAt), encodeTime(webhook.UpdatedAt))
	if err != nil {
		return fmt.Errorf("create webhook: %w", err)
	}
	return nil
}

// GetWebhook returns a webhook including its secret for internal delivery.
func (s *Store) GetWebhook(ctx context.Context, id string) (types.Webhook, error) {
	return scanWebhook(s.db.QueryRowContext(ctx, webhookSelect+` WHERE id = ?`, id))
}

// ListWebhooks returns all configured webhooks ordered by creation time.
func (s *Store) ListWebhooks(ctx context.Context) ([]types.Webhook, error) {
	rows, err := s.db.QueryContext(ctx, webhookSelect+` ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	defer rows.Close()
	webhooks := make([]types.Webhook, 0)
	for rows.Next() {
		webhook, err := scanWebhook(rows)
		if err != nil {
			return nil, fmt.Errorf("scan webhook: %w", err)
		}
		webhooks = append(webhooks, webhook)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	return webhooks, nil
}

// UpdateWebhook updates a webhook's destination, subscriptions, and secret.
func (s *Store) UpdateWebhook(ctx context.Context, webhook types.Webhook) error {
	events, err := encodeJSON(webhook.Events)
	if err != nil {
		return err
	}
	if webhook.UpdatedAt.IsZero() {
		webhook.UpdatedAt = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE webhooks SET url=?, events=?, secret=?, updated_at=? WHERE id=?`,
		webhook.URL, events, webhook.Secret, encodeTime(webhook.UpdatedAt), webhook.ID)
	return checkUpdated(result, err, "update webhook")
}

// DeleteWebhook removes a webhook.
func (s *Store) DeleteWebhook(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "webhooks", "id", id)
}

const webhookSelect = `SELECT id, url, events, secret, created_at, updated_at FROM webhooks`

func scanWebhook(row interface{ Scan(...any) error }) (types.Webhook, error) {
	var webhook types.Webhook
	var events, createdAt, updatedAt string
	err := row.Scan(&webhook.ID, &webhook.URL, &events, &webhook.Secret, &createdAt, &updatedAt)
	if err != nil {
		return types.Webhook{}, err
	}
	if err := json.Unmarshal([]byte(events), &webhook.Events); err != nil {
		return types.Webhook{}, fmt.Errorf("decode webhook events: %w", err)
	}
	if webhook.CreatedAt, err = decodeTime(createdAt); err != nil {
		return types.Webhook{}, err
	}
	if webhook.UpdatedAt, err = decodeTime(updatedAt); err != nil {
		return types.Webhook{}, err
	}
	return webhook, nil
}

// PutIdempotency stores the response associated with a request key.
func (s *Store) PutIdempotency(ctx context.Context, record IdempotencyRecord) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO idempotency(key, scope, response, created_at) VALUES(?, ?, ?, ?)`,
		record.Key, record.Scope, record.Response, encodeTime(record.CreatedAt))
	if err != nil {
		return fmt.Errorf("store idempotency record: %w", err)
	}
	return nil
}

// GetIdempotency returns the stored response for a key.
func (s *Store) GetIdempotency(ctx context.Context, key string) (IdempotencyRecord, error) {
	var record IdempotencyRecord
	var createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT key, scope, response, created_at FROM idempotency WHERE key = ?`, key).
		Scan(&record.Key, &record.Scope, &record.Response, &createdAt)
	if err != nil {
		return IdempotencyRecord{}, err
	}
	record.CreatedAt, err = decodeTime(createdAt)
	return record, err
}

// DeleteIdempotency removes an idempotency key and its saved response.
func (s *Store) DeleteIdempotency(ctx context.Context, key string) error {
	return deleteByID(ctx, s.db, "idempotency", "key", key)
}

// GetSetting returns an opaque persisted setting value.
func (s *Store) GetSetting(ctx context.Context, key string) ([]byte, error) {
	var value []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	return value, err
}

// SetSetting atomically replaces an opaque persisted setting value.
func (s *Store) SetSetting(ctx context.Context, key string, value []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, nonNilBytes(value))
	if err != nil {
		return fmt.Errorf("set setting: %w", err)
	}
	return nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func deleteByID(ctx context.Context, db *sql.DB, table, column, id string) error {
	// Table and column are internal constants supplied by the typed methods above.
	result, err := db.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+column+` = ?`, id)
	return checkUpdated(result, err, "delete "+strings.TrimSuffix(table, "s"))
}

func checkUpdated(result sql.Result, err error, action string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", action, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: get affected rows: %w", action, err)
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func encodeTime(value time.Time) string { return value.UTC().Format(timeLayout) }

func decodeTime(value string) (time.Time, error) {
	t, err := time.Parse(timeLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored time %q: %w", value, err)
	}
	return t, nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return encodeTime(value)
}

func nullableTimePtr(value *time.Time) any {
	if value == nil {
		return nil
	}
	return encodeTime(*value)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nonNilBytes(value []byte) []byte {
	if value == nil {
		return []byte{}
	}
	return value
}

func encodeJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode JSON: %w", err)
	}
	return string(data), nil
}
