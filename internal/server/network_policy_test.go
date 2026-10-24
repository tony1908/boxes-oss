package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/tony1908/boxes-oss/internal/tunnel"
	"github.com/tony1908/boxes-oss/internal/types"
)

func TestSandboxNetworkPolicyValidatesOnCreateAndPatch(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID, boxID = "nd_netvalid", "bx_netvalid1234"
	seedPlatformBox(t, srv, nodeID, boxID, "netvalid-token", "", "")
	token := addPlatformSession(t, srv)
	invalid := map[string]any{"mode": types.NetworkAllowlist, "allowDomains": []string{"bad_domain!"}}

	var created map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes", token,
		map[string]any{"networkPolicy": invalid}, http.StatusBadRequest, &created)
	if code := created["error"].(map[string]any)["code"]; code != "invalid_network_policy" {
		t.Fatalf("create error code = %v, want invalid_network_policy", code)
	}

	var patched map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/sandboxes/"+boxID, token,
		map[string]any{"networkPolicy": map[string]any{"mode": "nope"}}, http.StatusBadRequest, &patched)
	if code := patched["error"].(map[string]any)["code"]; code != "invalid_network_policy" {
		t.Fatalf("patch error code = %v, want invalid_network_policy", code)
	}
}

func TestSandboxNetworkPolicyPatchStoresWhenStoppedAndSendsToAgentWhenRunning(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID, boxID, nodeToken = "nd_netpatch", "bx_netpatch1234", "netpatch-token"
	seedPlatformBox(t, srv, nodeID, boxID, nodeToken, "", "")
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	// Stopped: the policy is stored and no agent is contacted.
	record, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil {
		t.Fatal(err)
	}
	record.Box.State = types.StateStopped
	if err := srv.store.UpdateBox(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/sandboxes/"+boxID, token,
		map[string]any{"networkPolicy": map[string]any{"mode": "deny-all"}}, http.StatusOK, &stored)
	if policy := stored["sandbox"].(map[string]any)["networkPolicy"]; !reflect.DeepEqual(policy, map[string]any{"mode": "deny-all"}) {
		t.Fatalf("stopped patch policy = %#v", policy)
	}

	// Running: the agent receives PUT /v1/boxes/{id}/network with the policy, then null clears it.
	record, err = srv.store.GetBox(context.Background(), boxID)
	if err != nil {
		t.Fatal(err)
	}
	record.Box.State = types.StateReady
	if err := srv.store.UpdateBox(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	bodies := make(chan string, 4)
	serveAgent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/v1/boxes/"+boxID+"/network" {
			data, _ := io.ReadAll(r.Body)
			bodies <- string(data)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/v1/health" {
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"boxes":[{"boxId":%q,"state":"ready"}]}`, nodeID, boxID)
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

	var running map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/sandboxes/"+boxID, token,
		map[string]any{"networkPolicy": map[string]any{"mode": "allowlist", "allowDomains": []string{"api.github.com"}}}, http.StatusOK, &running)
	select {
	case body := <-bodies:
		var sent types.NetworkPolicy
		if err := json.Unmarshal([]byte(body), &sent); err != nil || sent.Mode != types.NetworkAllowlist || !reflect.DeepEqual(sent.AllowDomains, []string{"api.github.com"}) {
			t.Fatalf("agent received %q (err %v), want allowlist policy", body, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent was not sent the network policy")
	}

	var cleared map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPatch, httpServer.URL+"/api/v1/sandboxes/"+boxID, token,
		map[string]any{"networkPolicy": nil}, http.StatusOK, &cleared)
	if body := <-bodies; body != "null" {
		t.Fatalf("clear body = %q, want null", body)
	}
	if policy := cleared["sandbox"].(map[string]any)["networkPolicy"]; policy != nil {
		t.Fatalf("cleared policy = %#v, want omitted", policy)
	}
	check, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil || check.Box.NetworkPolicy != nil {
		t.Fatalf("stored policy after clear = %#v (err %v)", check.Box.NetworkPolicy, err)
	}
}
