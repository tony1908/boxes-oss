package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tony1908/boxes-oss/internal/store"
	"github.com/tony1908/boxes-oss/internal/tunnel"
	"github.com/tony1908/boxes-oss/internal/types"
)

func TestAgentTunnelIsReachableAtInstallPath(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	if err := srv.store.CreateNode(context.Background(), store.NodeRecord{Node: types.Node{ID: "nd_alias", Name: "alias", Status: "offline"}, TokenHash: tokenHash("join-token")}); err != nil {
		t.Fatalf("create alias test node: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tunnel.Dial(ctx, websocketURL(httpServer.URL)+"/api/v1/agent/connect", "join-token", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/health" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"nodeId":"nd_alias","cpus":2,"memoryBytes":100,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":100,"diskFreeBytes":100,"boxes":[]}`)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent tunnel did not stop after cancellation")
		}
	})
	waitFor(t, func() bool { return len(srv.hub.Connected()) == 1 })
}

func TestDeleteSandboxForgetsSnapshotsInBackground(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, boxID, nodeToken = "nd_asyncforget", "bx_asyncforget12", "async-forget-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "", "")
	if err := srv.store.CreateSnapshot(context.Background(), types.Snapshot{ID: "sn_asyncforget", BoxID: boxID, ResticID: "restic-blocked"}); err != nil {
		t.Fatal(err)
	}
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	forgetStarted := make(chan struct{}, 1)
	allowForget := make(chan struct{})
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/boxes/"+boxID:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/snapshots/restic-blocked":
			forgetStarted <- struct{}{}
			<-allowForget
			w.WriteHeader(http.StatusNoContent)
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
		close(allowForget)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent tunnel did not stop")
		}
	})
	waitFor(t, func() bool { return len(srv.hub.Connected()) == 1 })
	requestDone := make(chan *http.Response, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodDelete, httpServer.URL+"/api/v1/sandboxes/"+boxID, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := httpServer.Client().Do(request)
		if err != nil {
			requestDone <- nil
			return
		}
		requestDone <- response
	}()
	select {
	case response := <-requestDone:
		if response == nil {
			t.Fatal("delete request failed")
		}
		response.Body.Close()
		if response.StatusCode != http.StatusAccepted {
			t.Fatalf("delete status = %d, want 202", response.StatusCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DELETE /sandboxes/{id} waited for snapshot forgetting")
	}
	select {
	case <-forgetStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("background snapshot forgetting did not start")
	}
}

func TestSnapshotListingPaginationAndLatestNull(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()
	seedPlatformBox(t, srv, "nd_snap_page1", "bx_snap_page0001", "snap-page-token1", "", "")
	seedPlatformBox(t, srv, "nd_snap_page2", "bx_snap_page0002", "snap-page-token2", "", "")
	base := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	for i, item := range []struct{ id, box string }{{"sn_page_old", "bx_snap_page0001"}, {"sn_page_mid", "bx_snap_page0002"}, {"sn_page_new", "bx_snap_page0001"}} {
		if err := srv.store.CreateSnapshot(ctx, types.Snapshot{ID: item.id, BoxID: item.box, ResticID: item.id, CreatedAt: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	var first map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/snapshots?limit=2", token, nil, http.StatusOK, &first)
	firstItems := first["snapshots"].([]any)
	firstCursor := first["nextCursor"].(string)
	if len(firstItems) != 2 || firstItems[0].(map[string]any)["id"] != "sn_page_new" || firstItems[1].(map[string]any)["id"] != "sn_page_mid" || firstCursor == "" {
		t.Fatalf("first page = %#v", first)
	}
	var second map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/snapshots?limit=2&cursor="+url.QueryEscape(firstCursor), token, nil, http.StatusOK, &second)
	secondItems := second["snapshots"].([]any)
	if len(secondItems) != 1 || secondItems[0].(map[string]any)["id"] != "sn_page_old" || second["nextCursor"] != "" {
		t.Fatalf("second page = %#v", second)
	}
	var latest map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes/bx_snap_page0002/snapshots/latest", token, nil, http.StatusOK, &latest)
	if latest["snapshot"].(map[string]any)["id"] != "sn_page_mid" {
		t.Fatalf("latest = %#v", latest)
	}
	seedPlatformBox(t, srv, "nd_snap_empty1", "bx_snap_empty0001", "snap-empty-token1", "", "")
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes/bx_snap_empty0001/snapshots/latest", token, nil, http.StatusOK, &latest)
	if latest["snapshot"] != nil {
		t.Fatalf("empty latest = %#v", latest["snapshot"])
	}
}

func TestSnapshotDownloadStreamsFromFakeAgent(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, boxID, nodeToken = "nd_snap_download", "bx_snap_download1", "snapshot-download-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "", "")
	encryptedStorageEnv, err := srv.secrets.encrypt([]byte(`{"RESTIC_PASSWORD":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.CreateSnapshot(context.Background(), types.Snapshot{ID: "sn_download", BoxID: boxID, ResticID: "restic-download", Repository: "/repo", StorageEnvEnc: encryptedStorageEnv}); err != nil {
		t.Fatal(err)
	}
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	started := make(chan struct{}, 1)
	serveAgent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/v1/storage" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/health" {
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"boxes":[{"boxId":%q,"state":"stopped"}]}`, nodeID, boxID)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/snapshots/restic-download/download" {
			if r.Header.Get("X-Boxes-Snapshot-Storage") == "" {
				t.Error("snapshot storage header missing")
			}
			started <- struct{}{}
			w.Header().Set("Content-Type", "application/gzip")
			_, _ = io.WriteString(w, "streamed-gzip-bytes")
			return
		}
		if r.URL.Path == "/v1/health" {
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"boxes":[{"boxId":%q,"state":"stopped"}]}`, nodeID, boxID)
			return
		}
		http.NotFound(w, r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- tunnel.Dial(ctx, websocketURL(httpServer.URL)+"/api/v1/agent/connect", nodeToken, serveAgent)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent tunnel did not stop")
		}
	})
	waitFor(t, func() bool { return len(srv.hub.Connected()) == 1 })
	request, _ := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/snapshots/sn_download/download", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || string(body) != "streamed-gzip-bytes" {
		t.Fatalf("download = status %d body %q err %v", response.StatusCode, body, readErr)
	}
	select {
	case <-started:
	default:
		t.Fatal("fake agent was not called")
	}
}

func TestSandboxBoxdHealthCheckProxiesThroughAgentTunnel(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID = "nd_boxdproxy"
	const boxID = "bx_boxdproxy123"
	const nodeToken = "boxd-proxy-node-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "", "")
	token := addPlatformSession(t, srv)
	paths := make(chan string, 1)
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/health":
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"boxes":[{"boxId":%q,"state":"running"}]}`, nodeID, boxID)
		case "/v1/boxes/" + boxID + "/boxd/healthz":
			paths <- r.URL.Path
			_, _ = io.WriteString(w, `{"status":"ok"}`)
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
			t.Error("agent tunnel did not stop after cancellation")
		}
	})
	waitFor(t, func() bool { _, ok := srv.hub.Client(nodeID); return ok })

	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/boxd/healthz", nil)
	if err != nil {
		t.Fatalf("create boxd health request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatalf("request boxd health through server: %v", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read boxd health response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("boxd health status = %d, want HTTP 200: %s", response.StatusCode, responseBody)
	}
	var body map[string]string
	if err := json.Unmarshal(responseBody, &body); err != nil {
		t.Fatalf("decode boxd health response: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("boxd health response = %d %#v, want HTTP 200 status ok", response.StatusCode, body)
	}
	select {
	case path := <-paths:
		if path != "/v1/boxes/"+boxID+"/boxd/healthz" {
			t.Fatalf("agent received path %q", path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("boxd health request did not reach the agent")
	}
}

func TestForkStartsEventPumpForCreatedSandbox(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID = "nd_forkpump"
	const sourceID = "bx_forksource12"
	const nodeToken = "fork-pump-node-token"
	seedPlatformBox(t, srv, nodeID, sourceID, nodeToken, "", "")
	token := addPlatformSession(t, srv)
	pumpRequests := make(chan string, 64)
	createdBoxes := make(chan string, 1)
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/health":
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"runtime":"runc","boxes":[{"boxId":%q,"state":"running"}]}`, nodeID, sourceID)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+sourceID+"/snapshot":
			_, _ = io.WriteString(w, `{"snapshotId":"restic-fork-source","sizeBytes":42,"createdAt":"2026-09-24T12:00:00Z"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes":
			var request struct {
				BoxID string `json:"boxId"`
				Fork  bool   `json:"fork"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode fork create request: %v", err)
			}
			if !request.Fork {
				t.Error("fork create request did not include fork marker flag")
			}
			createdBoxes <- request.BoxID
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"containerId":"fork-container"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/boxd/events"):
			boxID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/boxes/"), "/boxd/events")
			select {
			case pumpRequests <- boxID:
			default:
			}
			_, _ = io.WriteString(w, `{"events":[],"cursor":0}`)
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
			t.Error("agent tunnel did not stop after cancellation")
		}
	})
	waitFor(t, func() bool { _, ok := srv.hub.Client(nodeID); return ok })

	var response map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+sourceID+"/fork", token, map[string]any{"ttlSeconds": 0}, http.StatusCreated, &response)
	forked, _ := response["sandbox"].(map[string]any)
	if forked["ttlSeconds"] != nil || forked["autoStop"] != false {
		t.Fatalf("fork with zero TTL = %#v", forked)
	}
	forkID, _ := forked["id"].(string)
	if forkID == "" {
		t.Fatalf("fork response = %#v, missing sandbox ID", response)
	}
	select {
	case got := <-createdBoxes:
		if got != forkID {
			t.Fatalf("agent created box %q, response returned %q", got, forkID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fork did not create a box on the agent")
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-pumpRequests:
			if got == forkID {
				return
			}
		case <-deadline:
			t.Fatalf("forked sandbox %s has no event pump", forkID)
		}
	}
}

func TestNodeReconnectRestartsEventPumpAfterTransientHealthFailure(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID = "nd_event_reconnect"
	const boxID = "bx_eventreconnect12"
	const nodeToken = "event-reconnect-node-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "", "")
	var healthCalls atomic.Int32
	var failNextHealth atomic.Bool
	eventRequests := make(chan struct{}, 128)
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/health":
			healthCalls.Add(1)
			if failNextHealth.Swap(false) {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"runtime":"runc","boxes":[{"boxId":%q,"state":"running"}]}`, nodeID, boxID)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/boxes/"+boxID+"/boxd/events":
			select {
			case eventRequests <- struct{}{}:
			default:
			}
			time.Sleep(20 * time.Millisecond)
			_, _ = io.WriteString(w, `{"events":[],"cursor":0}`)
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
			t.Error("agent tunnel did not stop after cancellation")
		}
	})
	waitFor(t, func() bool { _, ok := srv.hub.Client(nodeID); return ok })
	select {
	case <-eventRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("initial node connection did not start the event pump")
	}

	srv.nodeDisconnected(nodeID)
	waitFor(t, func() bool {
		srv.platformMu.Lock()
		defer srv.platformMu.Unlock()
		return len(srv.eventPumps) == 0
	})
	for {
		select {
		case <-eventRequests:
		default:
			goto drained
		}
	}
drained:
	beforeReconnect := healthCalls.Load()
	failNextHealth.Store(true)
	srv.nodeConnected(nodeID)
	select {
	case <-eventRequests:
	case <-time.After(5 * time.Second):
		t.Fatal("node reconnect did not restart the event pump after a transient health error")
	}
	if calls := healthCalls.Load() - beforeReconnect; calls < 2 {
		t.Fatalf("health attempts after reconnect = %d, want retry after the injected failure", calls)
	}
}

func TestBoxRoutesAreOnlySentWhenConfiguredOrExplicitlyCleared(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID = "nd_routes_empty"
	const boxID = "bx_routesempty12"
	seedPlatformBox(t, srv, nodeID, boxID, "routes-empty-token", "", "")

	var requests []map[string]any
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode routes request: %v", err)
		}
		requests = append(requests, body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer targetServer.Close()
	target, err := url.Parse(targetServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &agentClient{http: &http.Client{Transport: rewriteAgentTransport{target: target, base: http.DefaultTransport}}}

	if err := srv.pushCurrentBoxRoutes(context.Background(), client, types.Box{ID: boxID, NodeID: nodeID}); err != nil {
		t.Fatalf("push current empty routes: %v", err)
	}
	if len(requests) != 0 {
		t.Fatalf("route requests without configured routes = %#v, want none", requests)
	}

	if err := srv.reconcileCurrentBoxRoutes(context.Background(), client, types.Box{ID: boxID, NodeID: nodeID}); err != nil {
		t.Fatalf("reconcile empty routes: %v", err)
	}
	if len(requests) != 1 {
		t.Fatalf("route requests after explicit clear = %d, want 1", len(requests))
	}
	if _, hasDesktop := requests[0]["desktop"]; hasDesktop {
		t.Fatalf("clear request unexpectedly contains desktop route: %#v", requests[0])
	}
	hosts, ok := requests[0]["hosts"].([]any)
	if !ok || len(hosts) != 0 {
		t.Fatalf("clear request hosts = %#v, want empty list", requests[0]["hosts"])
	}
}

func TestDesktopSessionReloadedFromStoreKeepsURL(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir + "/server.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dataDir := dir + "/data"
	first, err := New(Config{Store: st, DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	const boxID, token = "bx_desktopreload1", "persisted-desktop-token"
	expires := time.Now().Add(time.Hour).UTC()
	enc, err := first.secrets.encrypt([]byte(token))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveDesktopSession(context.Background(), store.DesktopSessionRecord{BoxID: boxID, TokenEnc: enc, ExpiresAt: expires, Host: "desk.example.test", Browser: true, Profile: "work"}); err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, err := New(Config{Store: st, DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	loaded, ok := second.desktopSessions[boxID]
	if !ok || loaded.Token != token || loaded.Host != "desk.example.test" || !loaded.Browser || loaded.Profile != "work" {
		t.Fatalf("reloaded session = %#v, exists=%v", loaded, ok)
	}
	parsed := url.URL{Scheme: "https", Host: loaded.Host, Path: "/"}
	parsed.RawQuery = "app=browser&profile=work&token=" + url.QueryEscape(loaded.Token)
	want := url.URL{Scheme: "https", Host: "desk.example.test", Path: "/"}
	want.RawQuery = "app=browser&profile=work&token=" + url.QueryEscape(token)
	if parsed.String() != want.String() {
		t.Fatalf("reloaded URL = %q, want %q", parsed.String(), want.String())
	}
}

func TestDesktopAndHostURLsUseNodeHostFallbackAndReconcileRoutes(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID = "nd_platform"
	const boxID = "bx_platformbox12"
	const nodeToken = "node-platform-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "203.0.113.7", "")
	if err := srv.store.CreateHost(context.Background(), store.HostRecord{BoxID: boxID, Port: 3000}); err != nil {
		t.Fatalf("create host: %v", err)
	}
	token := addPlatformSession(t, srv)

	var mu sync.Mutex
	var routeBodies []map[string]any
	seenEventCursors := make(chan string, 8)
	serveAgent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/health":
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"runtime":"runc","boxes":[{"boxId":%q,"state":"running"}]}`, nodeID, boxID)
		case r.Method == http.MethodPut && r.URL.Path == "/v1/boxes/"+boxID+"/routes":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode routes: %v", err)
			}
			mu.Lock()
			routeBodies = append(routeBodies, body)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+boxID+"/snapshot":
			_, _ = io.WriteString(w, `{"snapshotId":"restic-platform","sizeBytes":42,"createdAt":"2026-09-24T12:00:00Z"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/snapshots/restic-platform/tree":
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/boxes/"+boxID+"/boxd/events":
			seenEventCursors <- r.URL.Query().Get("after")
			if r.URL.Query().Get("after") == "0" {
				_, _ = io.WriteString(w, `{"events":[{"cursor":1,"id":"ev_once","type":"message","time":"2026-09-24T12:00:00Z","data":{"text":"hello"}}],"cursor":1}`)
			} else {
				_, _ = io.WriteString(w, `{"events":[],"cursor":1}`)
			}
		default:
			http.NotFound(w, r)
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
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent tunnel did not stop after cancellation")
		}
	})
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(routeBodies) > 0
	})
	var namedResponse map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/named-snapshots", token,
		map[string]any{"boxId": boxID, "name": "daily"}, http.StatusCreated, &namedResponse)
	named, _ := namedResponse["namedSnapshot"].(map[string]any)
	snapshotID, _ := named["snapshotId"].(string)
	if snapshotID == "" {
		t.Fatalf("named snapshot response = %#v", namedResponse)
	}
	treeRequest, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/snapshots/"+snapshotID+"/tree", nil)
	if err != nil {
		t.Fatalf("create snapshot tree request: %v", err)
	}
	treeRequest.Header.Set("Authorization", "Bearer "+token)
	treeResponse, err := httpServer.Client().Do(treeRequest)
	if err != nil {
		t.Fatalf("get snapshot tree: %v", err)
	}
	defer treeResponse.Body.Close()
	if treeResponse.StatusCode != http.StatusOK {
		t.Fatalf("snapshot tree status = %d", treeResponse.StatusCode)
	}

	var desktop map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/desktop", token,
		map[string]any{"browser": true}, http.StatusOK, &desktop)
	desktopURL, _ := desktop["url"].(string)
	parsed, err := url.Parse(desktopURL)
	if err != nil {
		t.Fatalf("parse desktop URL %q: %v", desktopURL, err)
	}
	dnsBoxID := strings.ReplaceAll(boxID, "_", "-")
	wantHost := "desk-" + dnsBoxID + ".203.0.113.7.sslip.io"
	if parsed.Host != wantHost || parsed.Query().Get("app") != "browser" || parsed.Query().Get("token") == "" {
		t.Fatalf("desktop URL = %q, expected host %q and browser/token query", desktopURL, wantHost)
	}
	var reusedDesktop map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/desktop", token,
		map[string]any{"browser": true}, http.StatusOK, &reusedDesktop)
	if reusedDesktop["url"] != desktopURL {
		t.Fatalf("reused desktop URL = %v, want %v", reusedDesktop["url"], desktopURL)
	}
	var currentDesktop map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/desktop", token, nil, http.StatusOK, &currentDesktop)
	if currentDesktop["url"] != desktopURL {
		t.Fatalf("GET desktop URL = %v, want current URL %v", currentDesktop["url"], desktopURL)
	}
	srv.platformMu.Lock()
	session := srv.desktopSessions[boxID]
	session.ExpiresAt = srv.now().Add(20 * time.Minute)
	srv.desktopSessions[boxID] = session
	srv.platformMu.Unlock()
	var renewedDesktop map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/desktop", token,
		map[string]any{"browser": true}, http.StatusOK, &renewedDesktop)
	if renewedDesktop["url"] == desktopURL {
		t.Fatal("desktop token was not renewed inside the 30 minute window")
	}
	var absentDesktop map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes/bx_missing/desktop", token, nil, http.StatusNotFound, &absentDesktop)
	var publicDesktop map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/desktop", token,
		map[string]any{"public": true}, http.StatusOK, &publicDesktop)
	expiryText, _ := publicDesktop["expiresAt"].(string)
	publicExpiry, err := time.Parse(time.RFC3339Nano, expiryText)
	if err != nil || time.Until(publicExpiry) < 6*24*time.Hour {
		t.Fatalf("public desktop expires too soon: %#v, err %v", publicDesktop["expiresAt"], err)
	}
	var hostResponse map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/hosts", token,
		map[string]any{"port": 3001, "private": true}, http.StatusCreated, &hostResponse)
	host, _ := hostResponse["host"].(map[string]any)
	privateURL, _ := host["url"].(string)
	privateParsed, err := url.Parse(privateURL)
	if err != nil || privateParsed.Host != dnsBoxID+"-3001.203.0.113.7.sslip.io" || privateParsed.Query().Get("token") == "" {
		t.Fatalf("private host URL = %q; err %v", privateURL, err)
	}
	mu.Lock()
	if len(routeBodies) == 0 {
		mu.Unlock()
		t.Fatal("no routes pushed to agent")
	}
	lastRoutes := routeBodies[len(routeBodies)-1]
	mu.Unlock()
	desktopRoute, _ := lastRoutes["desktop"].(map[string]any)
	if desktopRoute == nil || desktopRoute["host"] != wantHost || desktopRoute["tokenHash"] == "" {
		t.Fatalf("desktop route not retained while updating host routes: %#v", lastRoutes)
	}
	routes, _ := lastRoutes["hosts"].([]any)
	if len(routes) != 2 {
		t.Fatalf("pushed hosts = %#v, want existing and new host", routes)
	}
	select {
	case cursor := <-seenEventCursors:
		if cursor != "0" {
			t.Fatalf("initial event cursor = %q, want 0", cursor)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event pump did not request events")
	}
	waitFor(t, func() bool { return srv.boxdCursor(boxID) == 1 })
	afterCursor := false
	deadline := time.After(5 * time.Second)
	for !afterCursor {
		select {
		case cursor := <-seenEventCursors:
			afterCursor = cursor == "1"
		case <-deadline:
			t.Fatal("event pump did not resume from the last boxd cursor")
		}
	}
	srv.platformMu.Lock()
	pumpCount := len(srv.eventPumps)
	srv.platformMu.Unlock()
	if pumpCount != 1 {
		t.Fatalf("event pump count = %d, want one goroutine for the ready box", pumpCount)
	}
	box, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil {
		t.Fatalf("load box for event pump state transition: %v", err)
	}
	box.Box.State = types.StateStopped
	srv.syncEventPump(box.Box)
	waitFor(t, func() bool {
		srv.platformMu.Lock()
		defer srv.platformMu.Unlock()
		return len(srv.eventPumps) == 0
	})
	box.Box.State = types.StateReady
	srv.syncEventPump(box.Box)
	srv.syncEventPump(box.Box)
	srv.platformMu.Lock()
	pumpCount = len(srv.eventPumps)
	srv.platformMu.Unlock()
	if pumpCount != 1 {
		t.Fatalf("resumed event pump count = %d, want one", pumpCount)
	}
	client, ok := newAgentClient(srv.hub, nodeID)
	if !ok {
		t.Fatal("agent disconnected before route reconcile test")
	}
	srv.repushNodeRoutes(context.Background(), nodeID, client)
	mu.Lock()
	lastRoutes = routeBodies[len(routeBodies)-1]
	mu.Unlock()
	desktopRoute, _ = lastRoutes["desktop"].(map[string]any)
	if desktopRoute == nil || desktopRoute["host"] != wantHost {
		t.Fatalf("desktop route was not restored after node reconcile: %#v", lastRoutes)
	}
	srv.platformMu.Lock()
	expired := srv.desktopRoutes[boxID]
	expired.ExpiresAt = time.Now().Add(-time.Second)
	srv.desktopRoutes[boxID] = expired
	srv.platformMu.Unlock()
	srv.repushNodeRoutes(context.Background(), nodeID, client)
	mu.Lock()
	lastRoutes = routeBodies[len(routeBodies)-1]
	mu.Unlock()
	if _, exists := lastRoutes["desktop"]; exists {
		t.Fatalf("expired desktop route was pushed after reconnect: %#v", lastRoutes)
	}
	waitFor(t, func() bool {
		events, err := srv.store.ListEvents(context.Background(), boxID, 0, 10)
		return err == nil && len(events) == 1
	})
	events, err := srv.store.ListEvents(context.Background(), boxID, 0, 10)
	if err != nil || len(events) != 1 || events[0].ID != "ev_once" {
		t.Fatalf("stored events = %#v, err %v; want exactly one event", events, err)
	}
	var eventResponse map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/events?after=0", token, nil, http.StatusOK, &eventResponse)
	apiEvents, _ := eventResponse["events"].([]any)
	if len(apiEvents) != 1 {
		t.Fatalf("events API returned %#v, want the stored event", eventResponse)
	}
}

func TestEnvironmentSecretsAreEncryptedAndDeliveredAsFiles(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	var response map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/environments", token, map[string]any{
		"name": "dev", "vars": map[string]string{"MODE": "test"}, "secrets": map[string]string{"API_KEY": "secret-value"},
		"secretFiles": map[string]string{"git-token": "private-token"},
		"repos":       []map[string]any{{"name": "repo", "url": "https://example.test/repo", "tokenSecret": "git-token"}},
		"setupScript": "printf ready",
	}, http.StatusCreated, &response)
	envJSON, _ := response["environment"].(map[string]any)
	envID, _ := envJSON["id"].(string)
	if envID == "" {
		t.Fatalf("environment response = %#v", response)
	}
	stored, err := srv.store.GetEnvironmentVersion(context.Background(), envID, 1)
	if err != nil {
		t.Fatalf("get stored environment: %v", err)
	}
	if strings.Contains(string(stored.SecretsEnc), "secret-value") || strings.Contains(string(stored.SecretsEnc), "private-token") {
		t.Fatal("environment secrets were stored as plaintext")
	}
	files, version, err := srv.environmentFiles(context.Background(), envID, 1, false)
	if err != nil || version != 1 {
		t.Fatalf("prepare environment files: version %d, err %v", version, err)
	}
	contents := make(map[string]string)
	for _, file := range files {
		name, _ := file["path"].(string)
		encoded, _ := file["content"].(string)
		decoded, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil {
			t.Fatalf("decode %s: %v", name, decodeErr)
		}
		contents[name] = string(decoded)
	}
	if !strings.Contains(contents["/run/boxes/env"], "API_KEY=secret-value") || contents["/run/boxes/secrets/git-token"] != "private-token" {
		t.Fatalf("environment file set omitted configured values: %#v", contents)
	}
	withoutSecrets, _, err := srv.environmentFiles(context.Background(), envID, 1, true)
	if err != nil {
		t.Fatalf("prepare no-secrets environment: %v", err)
	}
	for _, file := range withoutSecrets {
		name, _ := file["path"].(string)
		encoded, _ := file["content"].(string)
		decoded, _ := base64.StdEncoding.DecodeString(encoded)
		if name == "/run/boxes/secrets/git-token" || strings.Contains(string(decoded), "secret-value") || strings.Contains(string(decoded), "git-token") {
			t.Fatalf("noSecrets payload contains secret material: %s=%q", name, decoded)
		}
	}
}

func TestProvisionSandboxSendsEnvironmentFilesToAgent(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID = "nd_envprovision"
	const nodeToken = "env-node-token"
	if err := srv.store.CreateNode(context.Background(), store.NodeRecord{Node: types.Node{ID: nodeID, Name: "environment", Status: "offline"}, TokenHash: tokenHash(nodeToken)}); err != nil {
		t.Fatalf("create environment node: %v", err)
	}
	token := addPlatformSession(t, srv)
	var environmentResponse map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/environments", token, map[string]any{
		"name": "provisioned", "vars": map[string]string{"MODE": "test"}, "secrets": map[string]string{"API_KEY": "value-from-secret"},
		"secretFiles": map[string]string{"token": "file-secret"},
	}, http.StatusCreated, &environmentResponse)
	environment, _ := environmentResponse["environment"].(map[string]any)
	environmentID, _ := environment["id"].(string)

	filesReceived := make(chan []map[string]any, 1)
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/health":
			_, _ = io.WriteString(w, `{"nodeId":"`+nodeID+`","cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"runtime":"runc","boxes":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes":
			var request struct {
				Files []map[string]any `json:"files"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode create box payload: %v", err)
			}
			filesReceived <- request.Files
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"containerId":"env-container"}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/routes"):
			w.WriteHeader(http.StatusNoContent)
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
			t.Error("agent tunnel did not stop after cancellation")
		}
	})
	waitFor(t, func() bool {
		node, err := srv.store.GetNode(context.Background(), nodeID)
		return err == nil && node.Node.Status == "healthy"
	})
	var created map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes", token, map[string]any{
		"type": "small", "environment": environmentID, "env": map[string]string{"REQUEST_ONLY": "present"},
		"setupScript": "printf setup once", "ttlSeconds": nil,
	}, http.StatusCreated, &created)
	if created["setupStatus"] != "running" || created["autoStop"] != false || created["ttlSeconds"] != nil {
		t.Fatalf("new sandbox lifecycle fields = %#v", created)
	}
	select {
	case files := <-filesReceived:
		contentByPath := make(map[string]string)
		for _, file := range files {
			name, _ := file["path"].(string)
			encoded, _ := file["content"].(string)
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("decode agent file %s: %v", name, err)
			}
			contentByPath[name] = string(decoded)
		}
		if !strings.Contains(contentByPath["/run/boxes/env"], "API_KEY=value-from-secret") || !strings.Contains(contentByPath["/run/boxes/env"], "REQUEST_ONLY=present") || contentByPath["/run/boxes/secrets/token"] != "file-secret" {
			t.Fatalf("agent did not receive environment files: %#v", contentByPath)
		}
		var setup struct {
			SetupScript string `json:"setupScript"`
		}
		if err := json.Unmarshal([]byte(contentByPath["/run/boxes/setup.json"]), &setup); err != nil || setup.SetupScript != "printf setup once" {
			t.Fatalf("agent setup config = %#v, err %v", setup, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sandbox provisioning did not reach the agent")
	}
}

func TestPromptPersistsConversationAndRunAndRunReadsCurrentBoxdState(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID = "nd_prompt"
	const boxID = "bx_promptbox123"
	const nodeToken = "prompt-node-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "198.51.100.9", "prompt.example.test")
	token := addPlatformSession(t, srv)
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/health":
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"runtime":"runc","boxes":[{"boxId":%q,"state":"running"}]}`, nodeID, boxID)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes/"+boxID+"/boxd/prompt":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode boxd prompt: %v", err)
			}
			if _, forwarded := body["stopWhenDone"]; forwarded {
				t.Error("server forwarded stopWhenDone to boxd instead of owning the lifecycle policy")
			}
			_, _ = io.WriteString(w, `{"runId":"run_prompt1","conversationId":"cv_prompt1"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/boxes/"+boxID+"/boxd/runs/run_prompt1":
			_, _ = io.WriteString(w, `{"id":"run_prompt1","conversationId":"cv_prompt1","status":"running"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/boxes/"+boxID+"/boxd/conversations":
			_, _ = io.WriteString(w, `[{"id":"cv_prompt1","provider":"claude","prompts":1,"running":true,"current":true}]`)
		default:
			w.WriteHeader(http.StatusNoContent)
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
			t.Error("agent tunnel did not stop after cancellation")
		}
	})
	waitFor(t, func() bool {
		client, ok := srv.hub.Client(nodeID)
		return ok && client != nil
	})
	var promptResponse map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/prompt", token,
		map[string]any{"prompt": "hello", "provider": "claude", "stopWhenDone": true}, http.StatusAccepted, &promptResponse)
	if promptResponse["runId"] != "run_prompt1" || promptResponse["conversationId"] != "cv_prompt1" {
		t.Fatalf("prompt response = %#v", promptResponse)
	}
	storedConversation, err := srv.store.GetConversation(context.Background(), "cv_prompt1")
	if err != nil || storedConversation.BoxID != boxID || storedConversation.LastPrompt != "hello" {
		t.Fatalf("stored conversation = %#v, err %v", storedConversation, err)
	}
	var runResponse map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/runs/run_prompt1", token, nil, http.StatusOK, &runResponse)
	if runResponse["status"] != "running" {
		t.Fatalf("run status = %#v, want latest boxd state", runResponse)
	}
	storedRun, err := srv.store.GetRun(context.Background(), "run_prompt1")
	if err != nil || storedRun.Status != "running" {
		t.Fatalf("stored run = %#v, err %v", storedRun, err)
	}
	if stopWhenDone, err := srv.store.RunStopWhenDone(context.Background(), storedRun.ID); err != nil || !stopWhenDone {
		t.Fatalf("stored stopWhenDone policy = %v, err %v; want true", stopWhenDone, err)
	}
	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/conversations", nil)
	if err != nil {
		t.Fatalf("create conversations request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatalf("get conversations: %v", err)
	}
	defer response.Body.Close()
	var conversations []types.Conversation
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&conversations) != nil || len(conversations) != 1 {
		t.Fatalf("conversations response status=%d entries=%#v", response.StatusCode, conversations)
	}
}

func TestRunStatusWaitsUntilTerminalEventIsStored(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID = "nd_runterminal"
	const boxID = "bx_runterminal123"
	const nodeToken = "run-terminal-node-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "", "")
	if err := srv.store.CreateConversation(context.Background(), types.Conversation{ID: "cv_runterminal", BoxID: boxID, Provider: "claude", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	if err := srv.store.CreateRunWithStopWhenDone(context.Background(), types.Run{ID: "run_terminal", ConversationID: "cv_runterminal", Status: "running", CreatedAt: time.Now().UTC()}, true); err != nil {
		t.Fatalf("create run: %v", err)
	}
	token := addPlatformSession(t, srv)
	var eventRequests atomic.Int32
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/health":
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"runtime":"runc","boxes":[{"boxId":%q,"state":"running"}]}`, nodeID, boxID)
		case "/v1/boxes/" + boxID + "/boxd/runs/run_terminal":
			_, _ = io.WriteString(w, `{"id":"run_terminal","conversationId":"cv_runterminal","status":"completed"}`)
		case "/v1/boxes/" + boxID + "/boxd/events":
			eventRequests.Add(1)
			// Model boxd having completed before the event becomes available to the server pump.
			time.Sleep(100 * time.Millisecond)
			_, _ = io.WriteString(w, `{"events":[{"cursor":1,"id":"ev_runterminal","conversationId":"cv_runterminal","runId":"run_terminal","type":"turn.completed","time":"2026-09-24T12:00:00Z","data":{}}],"cursor":1}`)
		case "/v1/boxes/" + boxID + "/stop":
			_, _ = io.WriteString(w, `{"snapshot":{"snapshotId":"stop-when-done-snapshot","sizeBytes":12,"createdAt":"2026-09-24T12:01:00Z"}}`)
		default:
			w.WriteHeader(http.StatusNoContent)
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
			t.Error("agent tunnel did not stop after cancellation")
		}
	})
	waitFor(t, func() bool { client, ok := srv.hub.Client(nodeID); return ok && client != nil })

	var run map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/runs/run_terminal", token, nil, http.StatusOK, &run)
	if run["status"] != "completed" {
		t.Fatalf("run status = %#v, want completed after terminal event is stored", run["status"])
	}
	events, err := srv.store.ListEvents(context.Background(), boxID, 0, 10)
	if err != nil || len(events) != 1 || events[0].Type != "turn.completed" {
		t.Fatalf("stored events = %#v, err %v; want terminal event before response", events, err)
	}
	if eventRequests.Load() == 0 {
		t.Fatal("run status request did not fetch boxd events")
	}
	waitFor(t, func() bool {
		box, err := srv.store.GetBox(context.Background(), boxID)
		return err == nil && box.Box.State == types.StateStopped
	})
	snapshots, err := srv.store.ListSnapshots(context.Background(), boxID)
	if err != nil || len(snapshots) != 1 || snapshots[0].ResticID != "stop-when-done-snapshot" {
		t.Fatalf("stopWhenDone snapshots = %#v, err %v", snapshots, err)
	}
}

func TestWebhookDeliveryRunsOffRequestPathAndSignsBody(t *testing.T) {
	t.Setenv("BOXES_WEBHOOK_ALLOW_PRIVATE", "true")
	delivered := make(chan *http.Request, 1)
	release := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		delivered <- r
		select {
		case <-release:
		case <-time.After(time.Second):
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	srv, cleanup := newTestServer(t)
	defer cleanup()
	webhook := types.Webhook{ID: "wh_platform", URL: target.URL, Events: []string{"sandbox.ready"}, Secret: "hook-secret", CreatedAt: time.Now().UTC()}
	if err := srv.store.CreateWebhook(context.Background(), webhook); err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	start := time.Now()
	srv.emitWebhookEvent(context.Background(), "sandbox.ready", map[string]any{"boxId": "bx_platformbox12"})
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("enqueue blocked for %s while webhook destination was slow", elapsed)
	}
	var request *http.Request
	select {
	case request = <-delivered:
	case <-time.After(time.Second):
		t.Fatal("webhook was not delivered asynchronously")
	}
	body, _ := io.ReadAll(request.Body)
	mac := hmac.New(sha256.New, []byte("hook-secret"))
	_, _ = mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := request.Header.Get("X-Boxes-Signature"); got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
	close(release)
}

func TestWebhookSecretRotationReplacesSecretAndReturnsItOnce(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	now := time.Now().UTC()
	webhook := types.Webhook{ID: "wh_rotate", URL: "https://example.test/hook", Events: []string{"sandbox.ready"}, Secret: "old-secret", CreatedAt: now, UpdatedAt: now}
	if err := srv.store.CreateWebhook(context.Background(), webhook); err != nil {
		t.Fatal(err)
	}
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	var result map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/webhooks/wh_rotate/rotate-secret", token, map[string]any{}, http.StatusOK, &result)
	secret, ok := result["secret"].(string)
	if !ok || secret == "" || secret == "old-secret" {
		t.Fatalf("rotated secret = %#v", result["secret"])
	}
	stored, err := srv.store.GetWebhook(context.Background(), webhook.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Secret != secret {
		t.Fatalf("stored secret = %q, response = %q", stored.Secret, secret)
	}
	var listed map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/webhooks", token, nil, http.StatusOK, &listed)
	encoded, _ := json.Marshal(listed)
	if strings.Contains(string(encoded), secret) {
		t.Fatal("webhook listing exposed its signing secret")
	}
}

func TestGetOperationReturnsStatusAndRejectsUnknownID(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	srv.operations["op_test"] = operationStatus{ID: "op_test", Status: "failed", Error: "agent unavailable"}
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	var result map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/operations/op_test", token, nil, http.StatusOK, &result)
	op := result["operation"].(map[string]any)
	if op["status"] != "failed" || op["error"] != "agent unavailable" {
		t.Fatalf("operation = %#v", op)
	}
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/operations/op_missing", token, nil, http.StatusNotFound, nil)
}

func TestWebhookDeliveryRetriesThreeTimesAfterInitialFailure(t *testing.T) {
	t.Setenv("BOXES_WEBHOOK_ALLOW_PRIVATE", "true")
	var attempts int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 4 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	srv, cleanup := newTestServer(t)
	defer cleanup()
	srv.deliverWebhook(webhookRequest{URL: target.URL, Secret: "retry-secret", Body: []byte(`{"event":"sandbox.ready"}`)})
	if attempts != 4 {
		t.Fatalf("webhook attempts = %d, want initial delivery plus three retries", attempts)
	}
}

func seedPlatformBox(t *testing.T, srv *Server, nodeID, boxID, nodeToken, host, hostBase string) {
	t.Helper()
	ctx := context.Background()
	if err := srv.store.CreateNode(ctx, store.NodeRecord{Node: types.Node{ID: nodeID, Name: "platform", Host: host, HostBase: hostBase, Status: "healthy", CPUs: 8, FreeMemoryBytes: 16 << 30}, TokenHash: tokenHash(nodeToken)}); err != nil {
		t.Fatalf("create platform node: %v", err)
	}
	password, err := srv.secrets.encrypt([]byte("desktop-password"))
	if err != nil {
		t.Fatalf("encrypt desktop password: %v", err)
	}
	boxd, err := srv.secrets.encrypt([]byte("boxd-token"))
	if err != nil {
		t.Fatalf("encrypt boxd token: %v", err)
	}
	if err := srv.store.CreateBox(ctx, store.BoxRecord{Box: types.Box{ID: boxID, NodeID: nodeID, Type: "small", State: types.StateReady, Image: "boxes/test", CreatedAt: time.Now().UTC()}, DesktopPasswordEnc: password, BoxdTokenEnc: boxd}); err != nil {
		t.Fatalf("create platform box: %v", err)
	}
}

func addPlatformSession(t *testing.T, srv *Server) string {
	t.Helper()
	ctx := context.Background()
	user := types.User{ID: "usr_platform", Email: "platform@example.test", Role: "owner", PasswordHash: "unused", CreatedAt: time.Now().UTC()}
	if err := srv.store.CreateUser(ctx, user); err != nil {
		t.Fatalf("create platform user: %v", err)
	}
	token := "platform-session-token"
	if err := srv.store.CreateSession(ctx, store.Session{ID: tokenHash(token), UserID: user.ID, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("create platform session: %v", err)
	}
	return token
}

func TestPlatformRoutesAreRegistered(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	for _, endpoint := range []string{
		"/sandboxes/bx_x/snapshots", "/snapshots/sn_x/tree", "/named-snapshots",
		"/sandboxes/bx_x/events", "/sandboxes/bx_x/conversations", "/environments",
		"/webhooks", "/limits", "/sandboxes/bx_x/hosts",
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, endpoint, nil)
		srv.APIHandler().ServeHTTP(recorder, request)
		if recorder.Code == http.StatusNotFound || strings.Contains(recorder.Body.String(), `"code":"not_found"`) {
			t.Errorf("platform endpoint %s is not registered: status %d", endpoint, recorder.Code)
		}
	}
}

func TestForkWithSourceReturnsInvalidSource(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, sourceID, nodeToken = "nd_forksrc", "bx_forksrc1234", "fork-source-token"
	seedPlatformBox(t, srv, nodeID, sourceID, nodeToken, "", "")
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	var rejected map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+sourceID+"/fork", token,
		map[string]any{"source": map[string]any{"type": "git", "url": "https://example.test/repo.git"}}, http.StatusBadRequest, &rejected)
	if code := rejected["error"].(map[string]any)["code"]; code != "invalid_source" {
		t.Fatalf("fork with source error code = %v, want invalid_source", code)
	}
}
