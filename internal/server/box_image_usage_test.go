package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tony1908/boxes-oss/internal/tunnel"
	"github.com/tony1908/boxes-oss/internal/types"
)

// fakeAgentCall records one request the server sent to a fake agent.
type fakeAgentCall struct {
	method, path string
	body         map[string]any
}

// serveFakeAgent connects a fake agent for nodeID and returns the channel of calls it received.
// statsStatus is the HTTP status returned for GET .../stats; stats is the body used when it is 200.
func serveFakeAgent(t *testing.T, srv *Server, httpServer *httptest.Server, nodeID, boxID, nodeToken string, stats string, statsStatus int) <-chan fakeAgentCall {
	t.Helper()
	calls := make(chan fakeAgentCall, 64)
	serveAgent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls <- fakeAgentCall{r.Method, r.URL.Path, body}
		switch {
		case r.URL.Path == "/v1/health":
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"runtime":"runc","boxes":[{"boxId":%q,"state":"running"}]}`, nodeID, boxID)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/stats"):
			w.WriteHeader(statsStatus)
			_, _ = io.WriteString(w, stats)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			_, _ = io.WriteString(w, `{"snapshot":{"snapshotId":"restic-stop","sizeBytes":1,"createdAt":"2026-10-24T11:00:00Z"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes":
			_, _ = io.WriteString(w, `{"containerId":"container-id"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/snapshot"):
			_, _ = io.WriteString(w, `{"snapshotId":"restic-fork","sizeBytes":1,"createdAt":"2026-10-24T11:00:00Z"}`)
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
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent tunnel did not stop")
		}
	})
	waitFor(t, func() bool { return len(srv.hub.Connected()) == 1 })
	return calls
}

func waitForCall(t *testing.T, calls <-chan fakeAgentCall, method, path string) fakeAgentCall {
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

func TestCreateSandboxSendsCustomImageAndRejectsInvalidImage(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, boxID, nodeToken = "nd_image", "bx_imageseed1234", "image-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "", "")
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	calls := serveFakeAgent(t, srv, httpServer, nodeID, boxID, nodeToken, "", http.StatusOK)

	var invalid map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes", token,
		map[string]any{"image": "bad image!"}, http.StatusBadRequest, &invalid)
	if code := invalid["error"].(map[string]any)["code"]; code != "invalid_image" {
		t.Fatalf("invalid image code = %v, want invalid_image", code)
	}

	var created map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes", token,
		map[string]any{"image": "ghcr.io/example/custom-box:v2"}, http.StatusCreated, &created)
	call := waitForCall(t, calls, http.MethodPost, "/v1/boxes")
	if call.body["image"] != "ghcr.io/example/custom-box:v2" {
		t.Fatalf("agent image = %v, want the custom image", call.body["image"])
	}
	if image := created["image"]; image != "ghcr.io/example/custom-box:v2" {
		t.Fatalf("created sandbox image = %v", image)
	}
}

func TestForkKeepsSourceImage(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, sourceID, nodeToken = "nd_forkimage", "bx_forkimage123", "fork-image-token"
	seedPlatformBox(t, srv, nodeID, sourceID, nodeToken, "", "")
	source, err := srv.store.GetBox(context.Background(), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	source.Box.Image = "ghcr.io/example/source-box:1"
	if err := srv.store.UpdateBox(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	calls := serveFakeAgent(t, srv, httpServer, nodeID, sourceID, nodeToken, "", http.StatusOK)

	var forked map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+sourceID+"/fork", token,
		map[string]any{"image": "ghcr.io/example/other:9"}, http.StatusCreated, &forked)
	call := waitForCall(t, calls, http.MethodPost, "/v1/boxes")
	if call.body["image"] != "ghcr.io/example/source-box:1" || call.body["fork"] != true {
		t.Fatalf("fork agent create = image %v fork %v, want the source image", call.body["image"], call.body["fork"])
	}
}

func TestSandboxUsageLiveAndTotalsAndStopAccumulates(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, boxID, nodeToken = "nd_usage", "bx_usagecounters1", "usage-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "", "")
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const stats = `{"cpuPercent":1.5,"desktopConnections":0,"cpuSeconds":10.5,"rxBytes":1000,"txBytes":400}`
	calls := serveFakeAgent(t, srv, httpServer, nodeID, boxID, nodeToken, stats, http.StatusOK)

	// Running: usage reports the live counters, and totals are the stored totals plus live.
	var usage map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/usage", token, nil, http.StatusOK, &usage)
	wantLive := map[string]any{"cpuSeconds": 10.5, "rxBytes": float64(1000), "txBytes": float64(400)}
	if live, _ := usage["live"].(map[string]any); fmt.Sprint(live) != fmt.Sprint(wantLive) {
		t.Fatalf("running usage live = %#v, want %#v", usage["live"], wantLive)
	}
	if totals, _ := usage["totals"].(map[string]any); fmt.Sprint(totals) != fmt.Sprint(wantLive) {
		t.Fatalf("running usage totals = %#v, want %#v", usage["totals"], wantLive)
	}

	// Stop: the counters are captured before the container stops and stored as totals.
	var stopped map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/stop", token, nil, http.StatusOK, &stopped)
	waitForCall(t, calls, http.MethodPost, "/v1/boxes/"+boxID+"/stop")
	stored, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil || stored.TotalCPUSeconds != 10.5 || stored.TotalRxBytes != 1000 || stored.TotalTxBytes != 400 {
		t.Fatalf("stored totals after stop = %v %d %d (err %v)", stored.TotalCPUSeconds, stored.TotalRxBytes, stored.TotalTxBytes, err)
	}
	var stoppedUsage map[string]any
	requestJSON(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/usage", token, nil, http.StatusOK, &stoppedUsage)
	if _, ok := stoppedUsage["live"]; ok {
		t.Fatalf("stopped usage includes live: %#v", stoppedUsage["live"])
	}
	if totals, _ := stoppedUsage["totals"].(map[string]any); fmt.Sprint(totals) != fmt.Sprint(wantLive) {
		t.Fatalf("stopped usage totals = %#v, want %#v", stoppedUsage["totals"], wantLive)
	}
	if _, ok := stopped["sandbox"]; !ok {
		t.Fatalf("stop response = %#v", stopped)
	}
}

func TestStopSucceedsWhenUsageStatsFail(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, boxID, nodeToken = "nd_statsfail", "bx_statsfail1234", "statsfail-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "", "")
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	calls := serveFakeAgent(t, srv, httpServer, nodeID, boxID, nodeToken, `{"error":{"code":"stats_failed"}}`, http.StatusBadGateway)

	var stopped map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/stop", token, nil, http.StatusOK, &stopped)
	waitForCall(t, calls, http.MethodPost, "/v1/boxes/"+boxID+"/stop")
	stored, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil || stored.Box.State != types.StateStopped || stored.TotalCPUSeconds != 0 {
		t.Fatalf("stored box after failed stats = state %v totals %v (err %v)", stored.Box.State, stored.TotalCPUSeconds, err)
	}
}
