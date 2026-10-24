package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/tony1908/boxes-oss/internal/store"
	"github.com/tony1908/boxes-oss/internal/types"
)

func TestPeriodicCapturePersistsAndPrunesOnlyUnpinnedSnapshots(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()
	const nodeID = "nd_snapshot"
	const boxID = "bx_snapshots123"
	if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: types.Node{ID: nodeID, Name: "snapshot node", Status: "healthy"}, TokenHash: "snapshot-token"}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: types.Box{ID: boxID, NodeID: nodeID, Type: "small", State: types.StateReady, CreatedAt: time.Now().UTC()}}); err != nil {
		t.Fatalf("create box: %v", err)
	}
	baseTime := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for index := 0; index < 7; index++ {
		if err := srv.store.CreateSnapshot(ctx, types.Snapshot{
			ID: types.NewID("sn_"), BoxID: boxID, ResticID: "restic-" + string(rune('0'+index)), CreatedAt: baseTime.Add(time.Duration(index) * time.Minute),
		}); err != nil {
			t.Fatalf("create snapshot %d: %v", index, err)
		}
	}
	oldSnapshots, err := srv.store.ListSnapshots(ctx, boxID)
	if err != nil {
		t.Fatalf("list seeded snapshots: %v", err)
	}
	if err := srv.store.CreateNamedSnapshot(ctx, types.NamedSnapshot{Name: "baseline", SnapshotID: oldSnapshots[0].ID, CreatedAt: baseTime}); err != nil {
		t.Fatalf("pin baseline snapshot: %v", err)
	}

	var forgotten [][]string
	var pruneCalls int
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+boxID+"/snapshot":
			_, _ = io.WriteString(w, `{"snapshotId":"restic-new","sizeBytes":42,"createdAt":"2026-09-24T13:00:00Z"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/snapshots/forget":
			var body struct {
				IDs []string `json:"ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode forget request: %v", err)
			}
			forgotten = append(forgotten, body.IDs)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/snapshots/prune":
			pruneCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer agentServer.Close()
	parsedAgentURL, err := url.Parse(agentServer.URL)
	if err != nil {
		t.Fatalf("parse test agent URL: %v", err)
	}
	client := &agentClient{http: &http.Client{Transport: rewriteAgentTransport{target: parsedAgentURL, base: http.DefaultTransport}}}
	if err := srv.recordPeriodicSnapshot(ctx, boxID, client); err != nil {
		t.Fatalf("record periodic snapshot: %v", err)
	}

	snapshots, err := srv.store.ListSnapshots(ctx, boxID)
	if err != nil {
		t.Fatalf("list snapshots after capture: %v", err)
	}
	if len(snapshots) != 6 {
		t.Fatalf("snapshot count = %d, want five recent plus named snapshot", len(snapshots))
	}
	if snapshots[len(snapshots)-1].ResticID != "restic-new" {
		t.Fatalf("latest stored snapshot = %q, want restic-new", snapshots[len(snapshots)-1].ResticID)
	}
	if len(forgotten) != 1 || !reflect.DeepEqual(forgotten[0], []string{"restic-1", "restic-2"}) {
		t.Fatalf("forget requests = %v, want one request for restic-1 and restic-2", forgotten)
	}
	if pruneCalls != 1 {
		t.Fatalf("prune requests = %d, want one", pruneCalls)
	}
	for i := 0; i < 2; i++ {
		if err := srv.store.CreateSnapshot(ctx, types.Snapshot{ID: types.NewID("sn_"), BoxID: boxID, ResticID: "extra-" + string(rune('0'+i)), CreatedAt: baseTime.Add(time.Duration(8+i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.pruneSnapshots(ctx, boxID, client); err != nil {
		t.Fatal(err)
	}
	if len(forgotten) != 2 || pruneCalls != 1 {
		t.Fatalf("after second retention pass: forget requests=%d prune requests=%d, want two and one", len(forgotten), pruneCalls)
	}
	for _, saved := range snapshots {
		if saved.ResticID == "restic-0" {
			return
		}
	}
	t.Fatal("pruning deleted the named snapshot target restic-0")
}

func TestPruneRemovesExpiredRowsWhenRepositoryIsMissing(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()
	base := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	const nodeID = "nd_missingrepo"
	const boxID = "bx_missingrepo123"
	if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: types.Node{ID: nodeID, Name: "snapshot node", Status: "healthy"}, TokenHash: "missingrepo-token"}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: types.Box{ID: boxID, NodeID: nodeID, Type: "small", State: types.StateReady, CreatedAt: base}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < snapshotsPerBox+2; i++ {
		if err := srv.store.CreateSnapshot(ctx, types.Snapshot{ID: types.NewID("sn_"), BoxID: boxID, ResticID: "gone-" + string(rune('0'+i)), CreatedAt: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	snapshots, err := srv.store.ListSnapshots(ctx, boxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreateNamedSnapshot(ctx, types.NamedSnapshot{Name: "pinned", SnapshotID: snapshots[0].ID, CreatedAt: base}); err != nil {
		t.Fatal(err)
	}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"repository_unreachable","message":"snapshot repository is unreachable"}}`)
	}))
	defer agent.Close()
	target, _ := url.Parse(agent.URL)
	client := &agentClient{http: &http.Client{Transport: rewriteAgentTransport{target: target, base: http.DefaultTransport}}}
	if err := srv.pruneSnapshots(ctx, boxID, client); err != nil {
		t.Fatalf("prune snapshots: %v", err)
	}
	remaining, err := srv.store.ListSnapshots(ctx, boxID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != snapshotsPerBox+1 || remaining[len(remaining)-1].ResticID != "gone-6" {
		t.Fatalf("remaining snapshots = %#v, want pinned row, five retained recent rows, and latest successful snapshot", remaining)
	}
}

func TestPeriodicCaptureSkipsDeletingAndDeletedBoxes(t *testing.T) {
	for _, state := range []types.BoxState{types.StateDeleting, "deleted"} {
		t.Run(string(state), func(t *testing.T) {
			srv, cleanup := newTestServer(t)
			defer cleanup()
			ctx := context.Background()
			const boxID = "bx_skipcapture123"
			if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: types.Node{ID: "nd_skipcapture", Name: "node", Status: "healthy"}}); err != nil {
				t.Fatal(err)
			}
			if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: types.Box{ID: boxID, NodeID: "nd_skipcapture", Type: "small", State: state}}); err != nil {
				t.Fatal(err)
			}
			var captures int
			agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+boxID+"/snapshot" {
					captures++
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer agent.Close()
			target, _ := url.Parse(agent.URL)
			client := &agentClient{http: &http.Client{Transport: rewriteAgentTransport{target: target, base: http.DefaultTransport}}}
			if err := srv.recordPeriodicSnapshot(ctx, boxID, client); err != nil {
				t.Fatalf("record snapshot: %v", err)
			}
			if captures != 0 {
				t.Fatalf("capture requests = %d, want 0", captures)
			}
		})
	}
}

func TestPeriodicCaptureTreatsAgentSkipAsUnchanged(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()
	const boxID = "bx_skippedcapture1"
	if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: types.Node{ID: "nd_skippedcapture", Name: "node", Status: "healthy"}}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: types.Box{ID: boxID, NodeID: "nd_skippedcapture", Type: "small", State: types.StateReady}}); err != nil {
		t.Fatal(err)
	}
	var captures int
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captures++
		var body map[string]bool
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["ifChanged"] != true {
			t.Errorf("request body = %#v, want ifChanged", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"skipped": true})
	}))
	defer agent.Close()
	target, _ := url.Parse(agent.URL)
	client := &agentClient{http: &http.Client{Transport: rewriteAgentTransport{target: target, base: http.DefaultTransport}}}
	skipped, err := srv.recordPeriodicSnapshotResult(ctx, boxID, client)
	if err != nil || !skipped {
		t.Fatalf("result = (%v, %v), want skipped", skipped, err)
	}
	snapshots, err := srv.store.ListSnapshots(ctx, boxID)
	if err != nil {
		t.Fatal(err)
	}
	if captures != 1 || len(snapshots) != 0 {
		t.Fatalf("captures=%d snapshots=%d, want one request and no records", captures, len(snapshots))
	}
}

func TestPeriodicCaptureContainerGoneIsNotRetriedOrRecorded(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()
	const boxID = "bx_gonecature123"
	if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: types.Node{ID: "nd_gonecapture", Name: "node", Status: "healthy"}}); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: types.Box{ID: boxID, NodeID: "nd_gonecapture", Type: "small", State: types.StateReady}}); err != nil {
		t.Fatal(err)
	}
	var captures int
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+boxID+"/snapshot" {
			captures++
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"code":"container_not_found","message":"No such container"}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer agent.Close()
	target, _ := url.Parse(agent.URL)
	client := &agentClient{http: &http.Client{Transport: rewriteAgentTransport{target: target, base: http.DefaultTransport}}}
	if err := srv.recordPeriodicSnapshot(ctx, boxID, client); err == nil {
		t.Fatal("capture error = nil, want missing-container error")
	}
	if captures != 1 {
		t.Fatalf("capture requests = %d, want one (no retry)", captures)
	}
	snapshots, err := srv.store.ListSnapshots(ctx, boxID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("stored snapshots = %#v, want none after failed capture", snapshots)
	}
}

func TestPeriodicPruneFailureKeepsCaptureAndRetriesAfterBackoff(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()
	const nodeID = "nd_prune_retry"
	const boxID = "bx_pruneretry123"
	if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: types.Node{ID: nodeID, Name: "snapshot node", Status: "healthy"}}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: types.Box{ID: boxID, NodeID: nodeID, Type: "small", State: types.StateReady, CreatedAt: time.Now().UTC()}}); err != nil {
		t.Fatalf("create box: %v", err)
	}
	baseTime := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for index := 0; index < snapshotsPerBox; index++ {
		if err := srv.store.CreateSnapshot(ctx, types.Snapshot{
			ID: types.NewID("sn_"), BoxID: boxID, ResticID: "restic-old-" + string(rune('0'+index)), CreatedAt: baseTime.Add(time.Duration(index) * time.Minute),
		}); err != nil {
			t.Fatalf("create old snapshot %d: %v", index, err)
		}
	}
	var captures, forgetAttempts int
	var forgotten [][]string
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+boxID+"/snapshot":
			captures++
			_, _ = io.WriteString(w, `{"snapshotId":"restic-new-`+string(rune('0'+captures))+`","sizeBytes":42,"createdAt":"2026-09-24T13:00:00Z"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/snapshots/forget":
			forgetAttempts++
			var body struct {
				IDs []string `json:"ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			forgotten = append(forgotten, body.IDs)
			if forgetAttempts == 1 {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"error":{"code":"snapshot_delete_failed"}}`)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/snapshots/prune":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer agentServer.Close()
	target, err := url.Parse(agentServer.URL)
	if err != nil {
		t.Fatalf("parse test agent URL: %v", err)
	}
	client := &agentClient{http: &http.Client{Transport: rewriteAgentTransport{target: target, base: http.DefaultTransport}}}

	if err := srv.recordPeriodicSnapshot(ctx, boxID, client); err != nil {
		t.Fatalf("record capture despite prune failure: %v", err)
	}
	snapshots, err := srv.store.ListSnapshots(ctx, boxID)
	if err != nil {
		t.Fatalf("list snapshots after failed prune: %v", err)
	}
	if len(snapshots) != snapshotsPerBox+1 || snapshots[len(snapshots)-1].ResticID != "restic-new-1" {
		t.Fatalf("snapshots after failed prune = %#v, want newly captured snapshot retained", snapshots)
	}

	if err := srv.recordPeriodicSnapshot(ctx, boxID, client); err != nil {
		t.Fatalf("record next capture during prune backoff: %v", err)
	}
	if forgetAttempts != 1 {
		t.Fatalf("forget attempts during backoff = %d, want 1", forgetAttempts)
	}

	srv.pruneMu.Lock()
	srv.pruneRetryAt[srv.storageConfig().Repository] = time.Now().Add(-time.Second)
	srv.pruneMu.Unlock()
	if err := srv.recordPeriodicSnapshot(ctx, boxID, client); err != nil {
		t.Fatalf("record capture and retry prune after backoff: %v", err)
	}
	if captures != 3 || forgetAttempts < 2 || !slices.Contains(forgotten[0], "restic-old-0") || !slices.Contains(forgotten[1], "restic-old-0") {
		t.Fatalf("capture/forget attempts = %d/%d, forgotten = %v; want failed batch retried after backoff", captures, forgetAttempts, forgotten)
	}
}

func TestPeriodicPruneBacksOffForUnavailableRepository(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()
	const nodeID = "nd_prune_backoff"
	const boxID = "bx_prunebackoff12"
	if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: types.Node{ID: nodeID, Name: "snapshot node", Status: "healthy"}}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: types.Box{ID: boxID, NodeID: nodeID, Type: "small", State: types.StateReady, CreatedAt: time.Now().UTC()}}); err != nil {
		t.Fatalf("create box: %v", err)
	}
	baseTime := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for index := 0; index < snapshotsPerBox; index++ {
		if _, err := srv.recordSnapshot(ctx, boxID, "restic-old-"+string(rune('0'+index)), 1,
			baseTime.Add(time.Duration(index)*time.Minute), storageConfig{Repository: "/deleted/repository"}); err != nil {
			t.Fatalf("create old snapshot %d: %v", index, err)
		}
	}

	var captures, forgetAttempts int
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+boxID+"/snapshot":
			captures++
			_, _ = io.WriteString(w, `{"snapshotId":"restic-new-`+string(rune('0'+captures))+`","sizeBytes":42,"createdAt":"2026-09-24T13:00:00Z"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/snapshots/forget":
			forgetAttempts++
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"code":"repository_unreachable"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer agentServer.Close()
	target, err := url.Parse(agentServer.URL)
	if err != nil {
		t.Fatalf("parse test agent URL: %v", err)
	}
	client := &agentClient{http: &http.Client{Transport: rewriteAgentTransport{target: target, base: http.DefaultTransport}}}

	for tick := 0; tick < 3; tick++ {
		if err := srv.recordPeriodicSnapshot(ctx, boxID, client); err != nil {
			t.Fatalf("record periodic snapshot on tick %d: %v", tick+1, err)
		}
	}
	if captures != 3 {
		t.Fatalf("capture attempts = %d, want 3 while prune is backed off", captures)
	}
	if forgetAttempts != 3 {
		t.Fatalf("forget attempts = %d, want one grouped attempt on each tick for unreachable-repository rows", forgetAttempts)
	}
}

type rewriteAgentTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t rewriteAgentTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.URL.Scheme = t.target.Scheme
	copy.URL.Host = t.target.Host
	return t.base.RoundTrip(copy)
}
