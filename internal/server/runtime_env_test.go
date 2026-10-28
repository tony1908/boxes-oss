package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tony1908/paddock/internal/store"
	"github.com/tony1908/paddock/internal/tunnel"
	"github.com/tony1908/paddock/internal/types"
)

// TestResumeRedeliversRuntimeEnvironment covers the resume payloads for boxes with an environment and -e variables,
// with noSecrets, and with neither. Snapshots exclude /run, so the files must be sent again on every resume.
func TestResumeRedeliversRuntimeEnvironment(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	const nodeID = "nd_runtimeenv"
	const nodeToken = "runtime-env-token"
	if err := srv.store.CreateNode(context.Background(), store.NodeRecord{Node: types.Node{ID: nodeID, Name: "runtime", Status: "offline"}, TokenHash: tokenHash(nodeToken)}); err != nil {
		t.Fatalf("create node: %v", err)
	}
	token := addPlatformSession(t, srv)
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	var environmentResponse map[string]any
	requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/environments", token, map[string]any{
		"name": "runtime", "vars": map[string]string{"MODE": "test"}, "secrets": map[string]string{"API_KEY": "secret-value"},
		"secretFiles": map[string]string{"git-token": "private-token"}, "setupScript": "printf setup once",
	}, http.StatusCreated, &environmentResponse)
	environmentID, _ := environmentResponse["environment"].(map[string]any)["id"].(string)

	type agentCall struct {
		method, path string
		body         map[string]any
	}
	calls := make(chan agentCall, 64)
	agent := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.URL.Path == "/v1/health":
			_, _ = fmt.Fprintf(w, `{"nodeId":%q,"cpus":8,"memoryBytes":17179869184,"memAvailableBytes":6442450944,"memoryPressureSomeAvg10":0,"freeMemoryBytes":8589934592,"diskFreeBytes":100000,"boxes":[]}`, nodeID)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/boxes/") && strings.HasSuffix(r.URL.Path, "/stop"):
			calls <- agentCall{r.Method, r.URL.Path, body}
			_, _ = io.WriteString(w, `{"snapshot":{"snapshotId":"restic-final","sizeBytes":1,"createdAt":"2026-10-24T11:00:00Z"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/boxes":
			calls <- agentCall{r.Method, r.URL.Path, body}
			_, _ = io.WriteString(w, `{"containerId":"runtime-container"}`)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/boxes/"):
			w.WriteHeader(http.StatusNoContent)
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
		case err := <-done:
			_ = err
		case <-time.After(5 * time.Second):
			t.Error("agent tunnel did not stop after cancellation")
		}
	})
	waitFor(t, func() bool {
		node, err := srv.store.GetNode(context.Background(), nodeID)
		return err == nil && node.Node.Status == "healthy"
	})

	// stopAndResume stops a box, then returns the files of the resume payload that restores it from the snapshot.
	stopAndResume := func(t *testing.T, boxID string) map[string]any {
		t.Helper()
		var stopped map[string]any
		requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/stop", token, map[string]any{}, http.StatusOK, &stopped)
		var resumed map[string]any
		requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes/"+boxID+"/resume", token, map[string]any{}, http.StatusOK, &resumed)
		deadline := time.After(5 * time.Second)
		for {
			select {
			case call := <-calls:
				if call.method == http.MethodPost && call.path == "/v1/boxes" && call.body["fromSnapshot"] != nil && call.body["boxId"] == boxID {
					return call.body
				}
			case <-deadline:
				t.Fatalf("resume payload for %s was not sent", boxID)
			}
		}
	}
	// filesByPath decodes every file in a resume payload; it returns nil when the payload has no files.
	filesByPath := func(t *testing.T, body map[string]any) map[string]string {
		t.Helper()
		raw, present := body["files"].([]any)
		if !present {
			return nil
		}
		contents := make(map[string]string)
		for _, item := range raw {
			file := item.(map[string]any)
			name := file["path"].(string)
			decoded, err := base64.StdEncoding.DecodeString(file["content"].(string))
			if err != nil {
				t.Fatalf("decode %s: %v", name, err)
			}
			contents[name] = string(decoded)
		}
		return contents
	}
	createBox := func(t *testing.T, body map[string]any) string {
		t.Helper()
		var created map[string]any
		requestJSON(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/v1/sandboxes", token, body, http.StatusCreated, &created)
		id, _ := created["id"].(string)
		if id == "" {
			t.Fatalf("created sandbox = %#v", created)
		}
		return id
	}

	t.Run("environment with -e variables", func(t *testing.T) {
		boxID := createBox(t, map[string]any{"type": "small", "environment": environmentID, "env": map[string]string{"FOO": "bar"}})
		stored, err := srv.store.GetBox(context.Background(), boxID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(stored.RuntimeEnvEnc), "bar") {
			t.Fatal("runtime environment was stored as plaintext")
		}
		files := filesByPath(t, stopAndResume(t, boxID))
		env := files["/run/paddock/env"]
		for _, want := range []string{"MODE=test", "API_KEY=secret-value", "FOO=bar"} {
			if !strings.Contains(env, want+"\n") {
				t.Fatalf("resume /run/paddock/env = %q, missing %s", env, want)
			}
		}
		if files["/run/paddock/secrets/git-token"] != "private-token" {
			t.Fatalf("resume secret file = %q", files["/run/paddock/secrets/git-token"])
		}
		if _, present := files["/run/paddock/setup.json"]; present {
			t.Fatal("resume payload re-sends setup.json; setup must not run again")
		}
	})

	t.Run("noSecrets keeps secret values out", func(t *testing.T) {
		boxID := createBox(t, map[string]any{"type": "small", "environment": environmentID, "noSecrets": true, "env": map[string]string{"FOO": "bar"}})
		files := filesByPath(t, stopAndResume(t, boxID))
		env := files["/run/paddock/env"]
		if !strings.Contains(env, "FOO=bar\n") || !strings.Contains(env, "MODE=test\n") {
			t.Fatalf("resume /run/paddock/env = %q, missing plain variables", env)
		}
		for name, content := range files {
			if strings.Contains(name, "secrets") || strings.Contains(content, "secret-value") || strings.Contains(content, "private-token") {
				t.Fatalf("noSecrets resume payload contains secret material: %s", name)
			}
		}
	})

	t.Run("box without environment or variables sends no files", func(t *testing.T) {
		boxID := createBox(t, map[string]any{"type": "small"})
		if files, present := stopAndResume(t, boxID)["files"]; present {
			t.Fatalf("resume payload files = %#v, want none", files)
		}
	})
}
