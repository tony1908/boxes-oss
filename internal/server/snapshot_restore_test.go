package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tony1908/boxes-oss/internal/tunnel"
	"github.com/tony1908/boxes-oss/internal/types"
)

type restoreAgentCall struct {
	method, path string
	body         map[string]any
}

func TestRestoreRollsBackStoppedAndRunningBoxesFromChosenSnapshot(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()
	const nodeID, boxID, nodeToken = "nd_restore", "bx_restore123456", "restore-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "", "")
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	base := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	if err := srv.store.CreateSnapshot(ctx, types.Snapshot{ID: "sn_old", BoxID: boxID, ResticID: "restic-old", CreatedAt: base}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreateSnapshot(ctx, types.Snapshot{ID: "sn_new", BoxID: boxID, ResticID: "restic-new", CreatedAt: base.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	record, err := srv.store.GetBox(ctx, boxID)
	if err != nil {
		t.Fatal(err)
	}
	record.State = types.StateStopped
	if err := srv.store.UpdateBox(ctx, record); err != nil {
		t.Fatal(err)
	}

	calls := make(chan restoreAgentCall, 32)
	serveAgent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.URL.Path == "/v1/health":
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"boxes":[]}`, nodeID)
			return
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+boxID+"/stop":
			calls <- restoreAgentCall{r.Method, r.URL.Path, body}
			_, _ = io.WriteString(w, `{"snapshot":{"snapshotId":"restic-final","sizeBytes":1,"createdAt":"2026-10-24T11:00:00Z"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/boxes/"+boxID:
			calls <- restoreAgentCall{r.Method, r.URL.Path, body}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes":
			calls <- restoreAgentCall{r.Method, r.URL.Path, body}
			_, _ = io.WriteString(w, `{}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tunnel.Dial(ctx, websocketURL(httpServer.URL)+"/api/v1/agent/connect", nodeToken, serveAgent)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			_ = err
		case <-time.After(5 * time.Second):
			t.Error("agent tunnel did not stop")
		}
	})
	waitFor(t, func() bool { return len(srv.hub.Connected()) == 1 })
	waitCall := func(method, path string) restoreAgentCall {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case call := <-calls:
				if call.method == method && call.path == path {
					return call
				}
			case <-deadline:
				t.Fatalf("agent was not sent %s %s", method, path)
			}
		}
	}

	// Stopped: the restore is recorded, the agent is not contacted, and the next resume uses the chosen snapshot.
	var restored map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/restore", token,
		map[string]any{"snapshotId": "sn_old"}, http.StatusOK, &restored)
	if state := restored["sandbox"].(map[string]any)["state"]; state != string(types.StateStopped) {
		t.Fatalf("restored stopped sandbox state = %v", state)
	}
	if stored, err := srv.store.GetBox(ctx, boxID); err != nil || stored.RestoreSnapshotID != "sn_old" {
		t.Fatalf("stored restore snapshot = %q (err %v), want sn_old", stored.RestoreSnapshotID, err)
	}
	var resumed map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/resume", token, map[string]any{}, http.StatusOK, &resumed)
	if call := waitCall(http.MethodPost, "/v1/boxes"); call.body["fromSnapshot"] != "restic-old" {
		t.Fatalf("resume fromSnapshot = %v, want restic-old", call.body["fromSnapshot"])
	}
	if stored, err := srv.store.GetBox(ctx, boxID); err != nil || stored.RestoreSnapshotID != "" {
		t.Fatalf("restore snapshot after resume = %q (err %v), want cleared", stored.RestoreSnapshotID, err)
	}

	// Running: the box is stopped (capturing a final snapshot), then resumed from the chosen snapshot.
	var rolledBack map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/restore", token,
		map[string]any{"snapshotId": "sn_old"}, http.StatusOK, &rolledBack)
	if state := rolledBack["sandbox"].(map[string]any)["state"]; state != string(types.StateReady) {
		t.Fatalf("rolled back running sandbox state = %v, want ready", state)
	}
	waitCall(http.MethodPost, "/v1/boxes/"+boxID+"/stop")
	if call := waitCall(http.MethodPost, "/v1/boxes"); call.body["fromSnapshot"] != "restic-old" {
		t.Fatalf("running rollback fromSnapshot = %v, want restic-old", call.body["fromSnapshot"])
	}

	// Unknown snapshots and busy sandboxes are refused.
	var missing map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/restore", token,
		map[string]any{"snapshotId": "sn_nope"}, http.StatusNotFound, &missing)
	if code := missing["error"].(map[string]any)["code"]; code != "snapshot_not_found" {
		t.Fatalf("unknown snapshot code = %v, want snapshot_not_found", code)
	}
	busy, err := srv.store.GetBox(ctx, boxID)
	if err != nil {
		t.Fatal(err)
	}
	busy.State = types.StateResuming
	if err := srv.store.UpdateBox(ctx, busy); err != nil {
		t.Fatal(err)
	}
	var conflict map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/restore", token,
		map[string]any{"snapshotId": "sn_new"}, http.StatusConflict, &conflict)
}

func TestKeepSnapshotsValidatesOnCreateAndPatch(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID, boxID = "nd_keepval", "bx_keepval12345"
	seedPlatformBox(t, srv, nodeID, boxID, "keepval-token", "", "")
	token := addPlatformSession(t, srv)

	for _, value := range []int{-1, 51} {
		var created map[string]any
		requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes", token,
			map[string]any{"keepSnapshots": value}, http.StatusBadRequest, &created)
		if code := created["error"].(map[string]any)["code"]; code != "invalid_keep_snapshots" {
			t.Fatalf("create keepSnapshots=%d code = %v", value, code)
		}
		var patched map[string]any
		requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/sandboxes/"+boxID, token,
			map[string]any{"keepSnapshots": value}, http.StatusBadRequest, &patched)
		if code := patched["error"].(map[string]any)["code"]; code != "invalid_keep_snapshots" {
			t.Fatalf("patch keepSnapshots=%d code = %v", value, code)
		}
	}
	var patched map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/sandboxes/"+boxID, token,
		map[string]any{"keepSnapshots": 2}, http.StatusOK, &patched)
	if got := patched["sandbox"].(map[string]any)["keepSnapshots"]; got != float64(2) {
		t.Fatalf("patched keepSnapshots = %v, want 2", got)
	}
	if stored, err := srv.store.GetBox(context.Background(), boxID); err != nil || stored.KeepSnapshots != 2 {
		t.Fatalf("stored keepSnapshots = %d (err %v), want 2", stored.KeepSnapshots, err)
	}
}
