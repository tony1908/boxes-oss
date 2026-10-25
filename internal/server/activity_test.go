package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tony1908/boxes-oss/internal/store"
	"github.com/tony1908/boxes-oss/internal/tunnel"
	"github.com/tony1908/boxes-oss/internal/types"
	"golang.org/x/crypto/ssh"
)

func TestActivityContractCursorAndAPIKeyActor(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ownerToken := addPlatformSession(t, srv)
	apiKey := types.APIKey{ID: "key_activity", Name: "nightly deploy", Key: "bx_live_activity-secret", Scopes: []string{"*"}, CreatedAt: time.Now().UTC()}
	if err := srv.store.CreateAPIKey(context.Background(), "usr_platform", apiKey, tokenHash(apiKey.Key)); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"act_1", "act_2", "act_3"} {
		if err := srv.store.RecordActivity(context.Background(), store.Activity{ID: id, BoxID: "bx_activity", Time: at, ActorType: "api_key", ActorID: apiKey.ID, ActorLabel: apiKey.Name, Category: "command", Action: "exec", Detail: []byte(`{"cwd":"/tmp"}`), Outcome: stringPointer("ok")}); err != nil {
			t.Fatal(err)
		}
	}
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	var first map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes/bx_activity/activity?limit=2", apiKey.Key, nil, http.StatusOK, &first)
	items, ok := first["items"].([]any)
	if !ok || len(items) != 2 || first["nextCursor"] == "" {
		t.Fatalf("first page = %#v", first)
	}
	item := items[0].(map[string]any)
	wantFields := map[string]bool{"id": true, "time": true, "actorType": true, "actorLabel": true, "category": true, "action": true, "detail": true, "outcome": true}
	if len(item) != len(wantFields) {
		t.Fatalf("activity item shape has unexpected fields: %#v", item)
	}
	for key := range item {
		if !wantFields[key] {
			t.Errorf("unexpected activity item field %q", key)
		}
	}
	if item["actorType"] != "api_key" || item["actorLabel"] != "nightly deploy" {
		t.Fatalf("API key actor = %#v", item)
	}
	cursor := first["nextCursor"].(string)
	var second map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes/bx_activity/activity?before="+cursor+"&limit=2", apiKey.Key, nil, http.StatusOK, &second)
	if len(second["items"].([]any)) != 1 || second["nextCursor"] != "" {
		t.Fatalf("second page = %#v", second)
	}
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes/bx_activity/activity?limit=101", ownerToken, nil, http.StatusBadRequest, nil)
}

func TestPricingSettingsAreOwnerOnly(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ownerToken := addPlatformSession(t, srv)
	ctx := context.Background()
	member := types.User{ID: "usr_activity_member", Email: "member@example.test", Role: "user", PasswordHash: "unused", CreatedAt: time.Now().UTC()}
	if err := srv.store.CreateUser(ctx, member); err != nil {
		t.Fatal(err)
	}
	memberToken := "activity-member-token"
	if err := srv.store.CreateSession(ctx, store.Session{ID: tokenHash(memberToken), UserID: member.ID, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/settings/pricing", memberToken, nil, http.StatusForbidden, nil)
	requestJSON(t, httpServer.Client(), http.MethodPut, httpServer.URL+"/api/v1/settings/pricing", memberToken, map[string]any{"currency": "USD", "hourlyBySize": map[string]float64{"small": 1}}, http.StatusForbidden, nil)
	var pricing map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/settings/pricing", ownerToken, nil, http.StatusOK, &pricing)
	if pricing["currency"] != "USD" || len(pricing["hourlyBySize"].(map[string]any)) != 0 {
		t.Fatalf("default pricing = %#v", pricing)
	}
	requestJSON(t, httpServer.Client(), http.MethodPut, httpServer.URL+"/api/v1/settings/pricing", ownerToken, map[string]any{"currency": "USD", "hourlyBySize": map[string]float64{"small": 0.5, "default": 0.25}}, http.StatusOK, &pricing)
	if pricing["currency"] != "USD" || pricing["hourlyBySize"].(map[string]any)["small"] != 0.5 {
		t.Fatalf("saved pricing = %#v", pricing)
	}
	requestJSON(t, httpServer.Client(), http.MethodPut, httpServer.URL+"/api/v1/settings/pricing", ownerToken, map[string]any{"hourlyBySize": map[string]float64{"small": -1}}, http.StatusBadRequest, nil)
}

func TestActivityRecordingThroughFakeAgentForEveryCategoryAndRedactsSecrets(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, boxID, nodeToken = "nd_activity", "bx_activitylog", "activity-node-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "198.51.100.7", "activity.example.test")
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	snapshotNumber := 0
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/health":
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"boxes":[{"boxId":%q,"state":"running"}]}`, nodeID, boxID)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/boxes/"+boxID+"/boxd/events":
			_, _ = io.WriteString(w, `{"events":[],"cursor":0}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+boxID+"/boxd/prompt":
			_, _ = io.WriteString(w, `{"runId":"run_activity","conversationId":"conv_activity"}`)
		case (r.Method == http.MethodPost && (r.URL.Path == "/v1/boxes/"+boxID+"/boxd/steer" || r.URL.Path == "/v1/boxes/"+boxID+"/boxd/interrupt")) ||
			(r.Method == http.MethodPut && r.URL.Path == "/v1/boxes/"+boxID+"/routes"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+boxID+"/exec":
			var command struct {
				Detached bool `json:"detached"`
			}
			_ = json.NewDecoder(r.Body).Decode(&command)
			if command.Detached {
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, `{"processId":"proc_activity"}`)
			} else {
				_, _ = io.WriteString(w, `{"exitCode":0}`)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/v1/boxes/"+boxID+"/exec/proc_activity":
			_, _ = io.WriteString(w, `{"running":false,"exitCode":7,"stdout":"not stored in activity"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/boxes/"+boxID+"/files":
			_, _ = io.WriteString(w, "FILE_BODY_MUST_NOT_BE_RECORDED")
		case r.Method == http.MethodPut && r.URL.Path == "/v1/boxes/"+boxID+"/files":
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+boxID+"/snapshot":
			snapshotNumber++
			_, _ = fmt.Fprintf(w, `{"snapshotId":"restic-%d","sizeBytes":%d,"createdAt":%q}`, snapshotNumber, snapshotNumber*4096, time.Now().UTC().Format(time.RFC3339Nano))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+boxID+"/stop":
			_, _ = io.WriteString(w, `{"snapshot":{"snapshotId":"restic-stop","sizeBytes":8192}}`)
		case r.Method == http.MethodDelete && (r.URL.Path == "/v1/boxes/"+boxID || strings.HasPrefix(r.URL.Path, "/v1/snapshots/")):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/boxes/"+boxID+"/tcp/3000":
			conn, err := websocket.Accept(w, r, nil)
			if err == nil {
				_ = conn.Close(websocket.StatusNormalClosure, "")
			}
		default:
			http.NotFound(w, r)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tunnel.Dial(ctx, websocketURL(httpServer.URL)+"/api/v1/agent/connect", nodeToken, agent)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("fake agent tunnel did not stop")
		}
	})
	waitFor(t, func() bool { _, ok := srv.hub.Client(nodeID); return ok })
	base := httpServer.URL + "/api/v1/sandboxes/" + boxID

	// Lifecycle and command entries.
	requestJSON(t, httpServer.Client(), http.MethodPatch, base, token, map[string]any{"ttlSeconds": 120}, http.StatusOK, nil)
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/commands", token, map[string]any{"cmd": "echo safe; TOKEN=inline-secret", "cwd": "/tmp", "env": map[string]string{"API_TOKEN": "ENV_VALUE_MUST_NOT_BE_RECORDED"}}, http.StatusOK, nil)
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/commands", token, map[string]any{"cmd": "sleep 1", "cwd": "/tmp", "detached": true}, http.StatusAccepted, nil)
	requestJSON(t, httpServer.Client(), http.MethodGet, base+"/commands/proc_activity", token, nil, http.StatusOK, nil)

	// Agent request and terminal completion entries.
	prompt := "password=prompt-secret " + strings.Repeat("x", 115) + " tail-that-must-be-truncated"
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/prompt", token, map[string]any{"prompt": prompt, "provider": "claude", "model": "test-model"}, http.StatusAccepted, nil)
	now := time.Now().UTC()
	srv.applyBoxdEvent(context.Background(), types.Event{BoxID: boxID, RunID: "run_activity", ConversationID: "conv_activity", Type: "turn.completed", Time: now, Data: map[string]any{}})
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/steer", token, map[string]any{"prompt": "steer-secret"}, http.StatusNoContent, nil)
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/interrupt", token, map[string]any{}, http.StatusNoContent, nil)

	// Access records, including file byte counts and the desktop reuse event.
	fileRequest, _ := http.NewRequest(http.MethodGet, base+"/files?path=%2Ftmp%2Freport.txt", nil)
	fileRequest.Header.Set("Authorization", "Bearer "+token)
	fileResponse, err := httpServer.Client().Do(fileRequest)
	if err != nil || fileResponse.StatusCode != http.StatusOK {
		t.Fatalf("read file: response=%v err=%v", fileResponse, err)
	}
	_ = fileResponse.Body.Close()
	upload, _ := http.NewRequest(http.MethodPut, base+"/files?path=%2Ftmp%2Fprivate.txt", strings.NewReader("FILE_SECRET_CONTENT"))
	upload.Header.Set("Authorization", "Bearer "+token)
	upload.Header.Set("Content-Type", "text/plain")
	fileResponse, err = httpServer.Client().Do(upload)
	if err != nil || fileResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("write file: response=%v err=%v", fileResponse, err)
	}
	_ = fileResponse.Body.Close()
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/hosts", token, map[string]any{"port": 8080, "private": true}, http.StatusCreated, nil)
	requestJSON(t, httpServer.Client(), http.MethodDelete, base+"/hosts/8080", token, nil, http.StatusNoContent, nil)
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/ssh-key", token, map[string]any{"publicKey": string(ssh.MarshalAuthorizedKey(sshKey))}, http.StatusNoContent, nil)
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/desktop", token, map[string]any{"browser": true, "public": false}, http.StatusOK, nil)
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/desktop", token, map[string]any{"browser": true, "public": false}, http.StatusOK, nil)
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/desktop", token, map[string]any{"browser": true, "public": false, "renewal": true}, http.StatusOK, nil)
	proxyCtx, proxyCancel := context.WithTimeout(context.Background(), 3*time.Second)
	proxyConn, _, proxyErr := websocket.Dial(proxyCtx, websocketURL(httpServer.URL)+"/api/v1/sandboxes/"+boxID+"/tcp/3000", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	proxyCancel()
	if proxyErr != nil {
		t.Fatalf("open TCP proxy: %v", proxyErr)
	}
	_ = proxyConn.Close(websocket.StatusNormalClosure, "")

	// Snapshot entries and lifecycle deletion. History must outlive the box row.
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/named-snapshots", token, map[string]any{"name": "checkpoint", "boxId": boxID}, http.StatusCreated, nil)
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/named-snapshots", token, map[string]any{"name": "checkpoint-2", "boxId": boxID}, http.StatusCreated, nil)
	var usage map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, base+"/usage", token, nil, http.StatusOK, &usage)
	if usage["commandsCount"] != float64(2) || usage["agentRunsCount"] != float64(1) || usage["desktopSessionsCount"] != float64(2) || usage["snapshotsCount"] != float64(2) || usage["snapshotsBytes"] != float64(8192) {
		t.Fatalf("usage counters = %#v", usage)
	}
	if _, ok := usage["lastSnapshotAt"].(string); !ok {
		t.Fatalf("usage lastSnapshotAt = %#v, want timestamp", usage["lastSnapshotAt"])
	}
	if len(usage["perDay"].([]any)) != 30 {
		t.Fatalf("usage perDay length = %d, want 30", len(usage["perDay"].([]any)))
	}
	requestJSON(t, httpServer.Client(), http.MethodPost, base+"/stop", token, map[string]any{}, http.StatusOK, nil)
	requestJSON(t, httpServer.Client(), http.MethodDelete, base, token, nil, http.StatusAccepted, nil)
	waitFor(t, func() bool {
		_, getErr := srv.store.GetBox(context.Background(), boxID)
		return errors.Is(getErr, sql.ErrNoRows)
	})
	activities, err := srv.store.ListActivity(context.Background(), boxID, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	desktopRows := 0
	for _, activity := range activities {
		seen[activity.Category] = true
		if strings.HasPrefix(activity.Action, "desktop session ") {
			desktopRows++
		}
		if activity.Category == "command" {
			var detail map[string]any
			if err := json.Unmarshal(activity.Detail, &detail); err == nil && detail["processId"] == "proc_activity" {
				if detail["exitCode"] != float64(7) || activity.DurationMS == nil || activity.Outcome == nil || *activity.Outcome != "error" {
					t.Errorf("detached command completion = detail %#v, duration %#v, outcome %#v", detail, activity.DurationMS, activity.Outcome)
				}
			}
		}
	}
	if desktopRows != 2 {
		t.Errorf("desktop activity rows = %d, want created + reused once despite renewal", desktopRows)
	}
	for _, category := range []string{"lifecycle", "command", "agent", "access", "snapshot"} {
		if !seen[category] {
			t.Errorf("activity category %q was not recorded: %#v", category, seen)
		}
	}
	encoded, err := json.Marshal(activities)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"ENV_VALUE_MUST_NOT_BE_RECORDED", "FILE_BODY_MUST_NOT_BE_RECORDED", "FILE_SECRET_CONTENT", "prompt-secret", "inline-secret", "steer-secret", "bx_live_activity-secret", "token="} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("recorded activity contains sensitive value %q: %s", secret, encoded)
		}
	}
	var response map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, base+"/activity?limit=100", token, nil, http.StatusOK, &response)
	if len(response["items"].([]any)) == 0 {
		t.Fatal("activity history disappeared when its sandbox was deleted")
	}
}

func stringPointer(value string) *string { return &value }

func TestSafeActivityTextRedactsOnlySecretEnvironmentAssignments(t *testing.T) {
	readable := map[string]string{
		"PORT=8080 npm start":      "PORT=8080 npm start",
		"NODE_ENV=production node": "NODE_ENV=production node",
	}
	for in, want := range readable {
		if got := safeActivityText(in, 0); got != want {
			t.Errorf("safeActivityText(%q) = %q, want %q", in, got, want)
		}
	}
	secret := map[string]string{
		"API_KEY=abc":         "API_KEY=[REDACTED]",
		"GITHUB_TOKEN=x":      "GITHUB_TOKEN=[REDACTED]",
		"DB_PASSWORD=y":       "DB_PASSWORD=[REDACTED]",
		"SESSION_ID=z run":    "SESSION_ID=[REDACTED] run",
		"AWS_SECRET=s deploy": "AWS_SECRET=[REDACTED] deploy",
	}
	for in, want := range secret {
		if got := safeActivityText(in, 0); got != want {
			t.Errorf("safeActivityText(%q) = %q, want %q", in, got, want)
		}
	}
}
