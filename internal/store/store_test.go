package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tony1908/boxes-oss/internal/types"
)

func TestBoxLifecycleAccountingFieldsRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Date(2026, time.September, 24, 12, 0, 0, 0, time.UTC)
	if err := s.CreateNode(ctx, NodeRecord{Node: types.Node{ID: "nd_lifecycle", Name: "node", Status: "healthy"}}); err != nil {
		t.Fatalf("CreateNode(): %v", err)
	}
	since := now.Add(-time.Minute)
	box := BoxRecord{Box: types.Box{ID: "bx_lifecycle123", NodeID: "nd_lifecycle", Type: "small", State: types.StateReady, CreatedAt: now, IdleStopSeconds: 1800},
		RunningSeconds: 45, RunningSince: &since, SetupStatus: "running"}
	if err := s.CreateBox(ctx, box); err != nil {
		t.Fatalf("CreateBox(): %v", err)
	}
	got, err := s.GetBox(ctx, box.ID)
	if err != nil {
		t.Fatalf("GetBox(): %v", err)
	}
	if got.RunningSeconds != box.RunningSeconds || got.RunningSince == nil || !got.RunningSince.Equal(since) || got.SetupStatus != "running" {
		t.Fatalf("lifecycle fields = %#v, want runningSeconds=%d runningSince=%s setupStatus=running", got, box.RunningSeconds, since)
	}
	if got.Box.IdleStopSeconds != 1800 {
		t.Fatalf("idle stop seconds = %d", got.Box.IdleStopSeconds)
	}
}

func TestBoxTagsRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "tags.db"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Date(2026, time.October, 24, 12, 0, 0, 0, time.UTC)
	if err := s.CreateNode(ctx, NodeRecord{Node: types.Node{ID: "nd_tags", Name: "node", Status: "healthy"}}); err != nil {
		t.Fatalf("CreateNode(): %v", err)
	}
	tags := map[string]string{"team": "agents", "env.name": "prod"}
	if err := s.CreateBox(ctx, BoxRecord{Box: types.Box{ID: "bx_tags1234567", NodeID: "nd_tags", Type: "small", State: types.StateReady, CreatedAt: now, Tags: tags}}); err != nil {
		t.Fatalf("CreateBox(): %v", err)
	}
	got, err := s.GetBox(ctx, "bx_tags1234567")
	if err != nil {
		t.Fatalf("GetBox(): %v", err)
	}
	if !reflect.DeepEqual(got.Box.Tags, tags) {
		t.Fatalf("created tags = %#v, want %#v", got.Box.Tags, tags)
	}
	got.Box.Tags = map[string]string{"owner": "ci"}
	if err := s.UpdateBox(ctx, got); err != nil {
		t.Fatalf("UpdateBox(): %v", err)
	}
	updated, err := s.GetBox(ctx, "bx_tags1234567")
	if err != nil {
		t.Fatalf("GetBox() after update: %v", err)
	}
	if !reflect.DeepEqual(updated.Box.Tags, map[string]string{"owner": "ci"}) {
		t.Fatalf("updated tags = %#v", updated.Box.Tags)
	}
	got.Box.Tags = nil
	if err := s.UpdateBox(ctx, got); err != nil {
		t.Fatalf("UpdateBox() clear: %v", err)
	}
	cleared, err := s.GetBox(ctx, "bx_tags1234567")
	if err != nil {
		t.Fatalf("GetBox() after clear: %v", err)
	}
	if cleared.Box.Tags != nil {
		t.Fatalf("cleared tags = %#v, want nil", cleared.Box.Tags)
	}
}

func TestIdleActivityAndOpenProxyTracking(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "idle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateNode(ctx, NodeRecord{Node: types.Node{ID: "nd_idle", Name: "node", Status: "healthy"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBox(ctx, BoxRecord{Box: types.Box{ID: "bx_idle1234567", NodeID: "nd_idle", Type: "small", State: types.StateReady, CreatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	for i, category := range []string{"command", "agent", "access"} {
		at := now.Add(time.Duration(i) * time.Second)
		if err := s.RecordActivity(ctx, Activity{ID: types.NewID("ac_"), BoxID: "bx_idle1234567", Time: at, ActorType: "system", ActorLabel: "system", Category: category, Action: "activity"}); err != nil {
			t.Fatal(err)
		}
		got, err := s.BoxIdleSince(ctx, "bx_idle1234567")
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(at) {
			t.Fatalf("%s activity time = %v, want %v", category, got, at)
		}
	}
	if err := s.RecordActivity(ctx, Activity{ID: types.NewID("ac_"), BoxID: "bx_idle1234567", Time: now.Add(time.Hour), ActorType: "system", ActorLabel: "system", Category: "lifecycle", Action: "stop requested"}); err != nil {
		t.Fatal(err)
	}
	last, _ := s.BoxIdleSince(ctx, "bx_idle1234567")
	if !last.Equal(now.Add(2 * time.Second)) {
		t.Fatalf("lifecycle row reset idle timer: %v", last)
	}
	opened := now.Add(3 * time.Second)
	_ = s.RecordActivity(ctx, Activity{ID: types.NewID("ac_"), BoxID: "bx_idle1234567", Time: opened, ActorType: "system", ActorLabel: "system", Category: "access", Action: "tcp proxy session opened"})
	if active, err := s.BoxHasOpenProxySession(ctx, "bx_idle1234567"); err != nil || !active {
		t.Fatalf("open proxy = %v, %v", active, err)
	}
	_ = s.RecordActivity(ctx, Activity{ID: types.NewID("ac_"), BoxID: "bx_idle1234567", Time: opened.Add(time.Minute), ActorType: "system", ActorLabel: "system", Category: "access", Action: "tcp proxy session closed"})
	if active, err := s.BoxHasOpenProxySession(ctx, "bx_idle1234567"); err != nil || active {
		t.Fatalf("closed proxy = %v, %v", active, err)
	}
}

func TestAPIKeyDailyUsageFlushAndSandboxAttribution(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "api-usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	if err := s.AddAPIKeyRequestCounts(ctx, map[string]int64{"key_usage": 1204}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordActivity(ctx, Activity{ID: "act_usage", BoxID: "bx_usage", Time: now, ActorType: "api_key", ActorID: "key_usage", ActorLabel: "deploy", Category: "lifecycle", Action: "created"}); err != nil {
		t.Fatal(err)
	}
	usage, err := s.APIKeyUsage(ctx, "key_usage", now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if usage["requests"] != 1204 || usage["sandboxesCreated"] != 1 {
		t.Fatalf("API key usage = %#v", usage)
	}
}

func TestOpenMigratesAndPersistsUsers(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "boxes.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	if err := s.db.PingContext(ctx); err != nil {
		t.Fatalf("database ping: %v", err)
	}

	var journalMode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatalf("read journal mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}
	var foreignKeys int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}

	user := types.User{
		ID:           "usr_test",
		Email:        "owner@example.com",
		Role:         "owner",
		PasswordHash: "hashed-password",
		CreatedAt:    time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC),
	}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser(): %v", err)
	}
	got, err := s.UserByEmail(ctx, user.Email)
	if err != nil {
		t.Fatalf("UserByEmail(): %v", err)
	}
	if got.ID != user.ID || got.PasswordHash != user.PasswordHash || !got.CreatedAt.Equal(user.CreatedAt) {
		t.Fatalf("UserByEmail() = %#v, want %#v", got, user)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer s.Close()
	if _, err := s.GetUser(ctx, user.ID); err != nil {
		t.Fatalf("GetUser() after reopen: %v", err)
	}
}

func TestSettingsPersistStorageDocument(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.SetSetting(ctx, "storage", []byte(`{"repository":"s3:https://s3.example.test/boxes"}`)); err != nil {
		t.Fatalf("SetSetting(): %v", err)
	}
	got, err := s.GetSetting(ctx, "storage")
	if err != nil || string(got) != `{"repository":"s3:https://s3.example.test/boxes"}` {
		t.Fatalf("GetSetting() = %q, %v", got, err)
	}
	if err := s.SetSetting(ctx, "storage", []byte(`{"repository":"s3:https://s3.example.test/other"}`)); err != nil {
		t.Fatalf("replace setting: %v", err)
	}
	got, err = s.GetSetting(ctx, "storage")
	if err != nil || string(got) != `{"repository":"s3:https://s3.example.test/other"}` {
		t.Fatalf("GetSetting() after update = %q, %v", got, err)
	}
}

func TestBackfillSnapshotStorageLeavesPreviouslyAssignedRepositoriesAlone(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "snapshot-storage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateNode(ctx, NodeRecord{Node: types.Node{ID: "nd_storagebackfill", Name: "node", Status: "healthy"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBox(ctx, BoxRecord{Box: types.Box{ID: "bx_storagebackfill", NodeID: "nd_storagebackfill", Type: "small", State: types.StateStopped, CreatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range []types.Snapshot{
		{ID: "sn_legacy", BoxID: "bx_storagebackfill", ResticID: "legacy", CreatedAt: now},
		{ID: "sn_moved", BoxID: "bx_storagebackfill", ResticID: "moved", CreatedAt: now, Repository: "s3:https://old.example.test/bucket", StorageEnvEnc: []byte("old-encrypted-env")},
	} {
		if err := s.CreateSnapshot(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.BackfillSnapshotStorage(ctx, "/var/lib/boxes/restic", []byte("legacy-encrypted-env")); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.GetSnapshot(ctx, "sn_legacy")
	if err != nil || legacy.Repository != "/var/lib/boxes/restic" || string(legacy.StorageEnvEnc) != "legacy-encrypted-env" {
		t.Fatalf("backfilled snapshot = %#v, %v", legacy, err)
	}
	moved, err := s.GetSnapshot(ctx, "sn_moved")
	if err != nil || moved.Repository != "s3:https://old.example.test/bucket" || string(moved.StorageEnvEnc) != "old-encrypted-env" {
		t.Fatalf("existing snapshot repository changed = %#v, %v", moved, err)
	}
}

func TestStorePersistsTypedRecords(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "records.db"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()
	now := time.Date(2026, 9, 24, 12, 30, 0, 0, time.UTC)

	user := types.User{ID: "usr_owner", Email: "owner@example.com", Role: "owner", PasswordHash: "hash", CreatedAt: now}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser(): %v", err)
	}
	session := Session{ID: "session-hash", UserID: user.ID, ExpiresAt: now.Add(time.Hour), CreatedAt: now}
	if err := s.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession(): %v", err)
	}
	gotSession, err := s.GetSession(ctx, session.ID)
	if err != nil || !reflect.DeepEqual(gotSession, session) {
		t.Fatalf("GetSession() = %#v, %v; want %#v", gotSession, err, session)
	}

	apiKey := types.APIKey{ID: "key_test", Name: "deploy", Scopes: []string{"*"}, CreatedAt: now}
	if err := s.CreateAPIKey(ctx, user.ID, apiKey, "sha256-key-hash"); err != nil {
		t.Fatalf("CreateAPIKey(): %v", err)
	}
	gotKey, err := s.APIKeyByHash(ctx, "sha256-key-hash")
	if err != nil || gotKey.ID != apiKey.ID || gotKey.UserID != user.ID || gotKey.KeyHash != "sha256-key-hash" || !reflect.DeepEqual(gotKey.Scopes, apiKey.Scopes) {
		t.Fatalf("APIKeyByHash() = %#v, %v", gotKey, err)
	}

	envDoc := map[string]any{"vars": map[string]any{"MODE": "test"}}
	env := types.Environment{ID: "env_test", Name: "testing", Version: 1, Document: envDoc, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateEnvironment(ctx, env, []byte("encrypted-secrets")); err != nil {
		t.Fatalf("CreateEnvironment(): %v", err)
	}
	gotEnv, err := s.GetEnvironment(ctx, env.ID)
	if err != nil || !reflect.DeepEqual(gotEnv, env) {
		t.Fatalf("GetEnvironment() = %#v, %v; want %#v", gotEnv, err, env)
	}
	gotVersion, err := s.GetEnvironmentVersion(ctx, env.ID, 1)
	if err != nil || !reflect.DeepEqual(gotVersion.Document, envDoc) || string(gotVersion.SecretsEnc) != "encrypted-secrets" {
		t.Fatalf("GetEnvironmentVersion() = %#v, %v", gotVersion, err)
	}

	node := NodeRecord{Node: types.Node{ID: "nd_test", Name: "node", Status: "healthy", CPUs: 8, MemoryBytes: 1 << 30, LastSeen: now}, TokenHash: "node-hash"}
	if err := s.CreateNode(ctx, node); err != nil {
		t.Fatalf("CreateNode(): %v", err)
	}
	box := BoxRecord{Box: types.Box{ID: "bx_abcdefghijkl", NodeID: node.Node.ID, Type: "small", State: types.StateReady, Image: "boxes/box:dev", EnvironmentID: env.ID, EnvironmentVersion: 1, TTLSeconds: 3600, CreatedAt: now}, DesktopPasswordEnc: []byte("pw-cipher"), BoxdTokenEnc: []byte("token-cipher"), IdempotencyKey: "request-1"}
	box.SetupStatus = "none"
	if err := s.CreateBox(ctx, box); err != nil {
		t.Fatalf("CreateBox(): %v", err)
	}
	gotBox, err := s.BoxByIdempotencyKey(ctx, "request-1")
	if err != nil || !reflect.DeepEqual(gotBox, box) {
		t.Fatalf("BoxByIdempotencyKey() = %#v, %v; want %#v", gotBox, err, box)
	}

	snapshot := types.Snapshot{ID: "sn_test", BoxID: box.ID, ResticID: "restic-id", SizeBytes: 1234, CreatedAt: now,
		Repository: "s3:https://s3.example.test/boxes/old", StorageEnvEnc: []byte("encrypted-storage-env")}
	if err := s.CreateSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("CreateSnapshot(): %v", err)
	}
	gotSnapshot, err := s.GetSnapshot(ctx, snapshot.ID)
	if err != nil || !reflect.DeepEqual(gotSnapshot, snapshot) {
		t.Fatalf("GetSnapshot() = %#v, %v; want %#v", gotSnapshot, err, snapshot)
	}
	named := types.NamedSnapshot{Name: "baseline", SnapshotID: snapshot.ID, CreatedAt: now}
	if err := s.CreateNamedSnapshot(ctx, named); err != nil {
		t.Fatalf("CreateNamedSnapshot(): %v", err)
	}
	gotNamed, err := s.GetNamedSnapshot(ctx, named.Name)
	if err != nil || !reflect.DeepEqual(gotNamed, named) {
		t.Fatalf("GetNamedSnapshot() = %#v, %v; want %#v", gotNamed, err, named)
	}

	conversation := types.Conversation{ID: "cv_test", BoxID: box.ID, Provider: "claude", Model: "sonnet", Prompts: 1, Running: true, LastPrompt: "hello", Current: true, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateConversation(ctx, conversation); err != nil {
		t.Fatalf("CreateConversation(): %v", err)
	}
	gotConversation, err := s.GetConversation(ctx, conversation.ID)
	if err != nil || !reflect.DeepEqual(gotConversation, conversation) {
		t.Fatalf("GetConversation() = %#v, %v; want %#v", gotConversation, err, conversation)
	}
	run := types.Run{ID: "run_test", ConversationID: conversation.ID, Status: "running", CreatedAt: now}
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun(): %v", err)
	}
	gotRun, err := s.GetRun(ctx, run.ID)
	if err != nil || !reflect.DeepEqual(gotRun, run) {
		t.Fatalf("GetRun() = %#v, %v; want %#v", gotRun, err, run)
	}
	event := types.Event{ID: "ev_test", BoxID: box.ID, ConversationID: conversation.ID, RunID: run.ID, Type: "message", Time: now, Data: map[string]any{"text": "hello"}}
	if _, err := s.CreateEvent(ctx, event); err != nil {
		t.Fatalf("CreateEvent(): %v", err)
	}
	events, err := s.ListEvents(ctx, box.ID, 0, 10)
	if err != nil || len(events) != 1 || events[0].ID != event.ID || events[0].Data["text"] != "hello" || events[0].Cursor == 0 {
		t.Fatalf("ListEvents() = %#v, %v", events, err)
	}

	host := HostRecord{BoxID: box.ID, Port: 3000, Private: true, TokenHash: "host-hash"}
	if err := s.CreateHost(ctx, host); err != nil {
		t.Fatalf("CreateHost(): %v", err)
	}
	hosts, err := s.ListHosts(ctx, box.ID)
	if err != nil || len(hosts) != 1 || !reflect.DeepEqual(hosts[0], host) {
		t.Fatalf("ListHosts() = %#v, %v", hosts, err)
	}

	webhook := types.Webhook{ID: "wh_test", URL: "https://example.com/hooks", Events: []string{"sandbox.ready"}, Secret: "signing-secret", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateWebhook(ctx, webhook); err != nil {
		t.Fatalf("CreateWebhook(): %v", err)
	}
	gotWebhook, err := s.GetWebhook(ctx, webhook.ID)
	if err != nil || !reflect.DeepEqual(gotWebhook, webhook) {
		t.Fatalf("GetWebhook() = %#v, %v; want %#v", gotWebhook, err, webhook)
	}

	response := json.RawMessage(`{"id":"bx_abcdefghijkl"}`)
	if err := s.PutIdempotency(ctx, IdempotencyRecord{Key: "idem-1", Scope: "sandbox.create", Response: response, CreatedAt: now}); err != nil {
		t.Fatalf("PutIdempotency(): %v", err)
	}
	gotIdempotency, err := s.GetIdempotency(ctx, "idem-1")
	if err != nil || gotIdempotency.Scope != "sandbox.create" || string(gotIdempotency.Response) != string(response) {
		t.Fatalf("GetIdempotency() = %#v, %v", gotIdempotency, err)
	}
}

func TestRunStopWhenDonePolicyPersistsAtomically(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "run-policy.db"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.CreateNode(ctx, NodeRecord{Node: types.Node{ID: "nd_runpolicy", Name: "node", Status: "healthy"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBox(ctx, BoxRecord{Box: types.Box{ID: "bx_runpolicy123", NodeID: "nd_runpolicy", Type: "small", State: types.StateReady, CreatedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	conversation := types.Conversation{ID: "cv_runpolicy", BoxID: "bx_runpolicy123", Provider: "claude", CreatedAt: time.Now().UTC()}
	if err := s.CreateConversation(ctx, conversation); err != nil {
		t.Fatal(err)
	}
	run := types.Run{ID: "run_policy", ConversationID: conversation.ID, Status: "queued"}
	if err := s.CreateRunWithStopWhenDone(ctx, run, true); err != nil {
		t.Fatalf("CreateRunWithStopWhenDone(): %v", err)
	}
	if enabled, err := s.RunStopWhenDone(ctx, run.ID); err != nil || !enabled {
		t.Fatalf("RunStopWhenDone() = %v, %v; want true", enabled, err)
	}
}

func TestCreateBoxEnforcesNodeForeignKey(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "boxes.db"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()

	err = s.CreateBox(context.Background(), BoxRecord{
		Box: types.Box{
			ID:        "bx_abcdefghijkl",
			NodeID:    "nd_missing",
			Type:      "small",
			State:     types.StateProvisioning,
			Image:     "boxes/box:dev",
			CreatedAt: time.Now().UTC(),
		},
	})
	if err == nil {
		t.Fatal("CreateBox() succeeded with a missing node")
	}
}

func TestBoxNetworkPolicyRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "network.db"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Date(2026, time.October, 24, 12, 0, 0, 0, time.UTC)
	if err := s.CreateNode(ctx, NodeRecord{Node: types.Node{ID: "nd_net", Name: "node", Status: "healthy"}}); err != nil {
		t.Fatalf("CreateNode(): %v", err)
	}
	policy := &types.NetworkPolicy{Mode: types.NetworkAllowlist, AllowDomains: []string{"api.github.com"}, AllowCIDRs: []string{"10.0.0.0/8"}, DenyCIDRs: []string{"10.1.0.0/16"}}
	if err := s.CreateBox(ctx, BoxRecord{Box: types.Box{ID: "bx_netpolicy1234", NodeID: "nd_net", Type: "small", State: types.StateReady, CreatedAt: now, NetworkPolicy: policy}}); err != nil {
		t.Fatalf("CreateBox(): %v", err)
	}
	got, err := s.GetBox(ctx, "bx_netpolicy1234")
	if err != nil {
		t.Fatalf("GetBox(): %v", err)
	}
	if !reflect.DeepEqual(got.Box.NetworkPolicy, policy) {
		t.Fatalf("created policy = %#v, want %#v", got.Box.NetworkPolicy, policy)
	}
	got.Box.NetworkPolicy = &types.NetworkPolicy{Mode: types.NetworkDenyAll}
	if err := s.UpdateBox(ctx, got); err != nil {
		t.Fatalf("UpdateBox(): %v", err)
	}
	updated, err := s.GetBox(ctx, "bx_netpolicy1234")
	if err != nil {
		t.Fatalf("GetBox() after update: %v", err)
	}
	if updated.Box.NetworkPolicy == nil || updated.Box.NetworkPolicy.Mode != types.NetworkDenyAll || len(updated.Box.NetworkPolicy.AllowDomains) != 0 {
		t.Fatalf("updated policy = %#v", updated.Box.NetworkPolicy)
	}
	updated.Box.NetworkPolicy = nil
	if err := s.UpdateBox(ctx, updated); err != nil {
		t.Fatalf("UpdateBox(nil): %v", err)
	}
	cleared, err := s.GetBox(ctx, "bx_netpolicy1234")
	if err != nil {
		t.Fatalf("GetBox() after clear: %v", err)
	}
	if cleared.Box.NetworkPolicy != nil {
		t.Fatalf("cleared policy = %#v, want nil", cleared.Box.NetworkPolicy)
	}
}

func TestBoxSourceRoundTripOmitsCredentials(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.CreateNode(ctx, NodeRecord{Node: types.Node{ID: "nd_source", Name: "node", Status: "healthy"}}); err != nil {
		t.Fatalf("CreateNode(): %v", err)
	}
	source := &types.BoxSource{Type: "git", URL: "https://example.test/repo.git", Revision: "v1.0.0", Path: "project"}
	if err := s.CreateBox(ctx, BoxRecord{Box: types.Box{ID: "bx_source1234567", NodeID: "nd_source", Type: "small", State: types.StateReady, CreatedAt: time.Now().UTC(), Source: source}}); err != nil {
		t.Fatalf("CreateBox(): %v", err)
	}
	got, err := s.GetBox(ctx, "bx_source1234567")
	if err != nil {
		t.Fatalf("GetBox(): %v", err)
	}
	if !reflect.DeepEqual(got.Box.Source, source) {
		t.Fatalf("source = %#v, want %#v", got.Box.Source, source)
	}
	got.Box.Source = nil
	if err := s.UpdateBox(ctx, got); err != nil {
		t.Fatalf("UpdateBox(): %v", err)
	}
	cleared, err := s.GetBox(ctx, "bx_source1234567")
	if err != nil {
		t.Fatalf("GetBox() after clear: %v", err)
	}
	if cleared.Box.Source != nil {
		t.Fatalf("cleared source = %#v, want nil", cleared.Box.Source)
	}
}
