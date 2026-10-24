package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tony1908/boxes-oss/internal/store"
	"github.com/tony1908/boxes-oss/internal/tunnel"
	"github.com/tony1908/boxes-oss/internal/types"
)

func TestValidateSandboxSource(t *testing.T) {
	depth := func(n int) *int { return &n }
	valid := []sandboxSource{
		{Type: "git", URL: "https://example.test/repo.git"},
		{Type: "git", URL: "https://example.test/repo.git", Revision: "v1.2.3", Depth: depth(1), Token: "t", Path: "app/src"},
		{Type: "git", URL: "https://example.test/repo.git", Revision: "0123456789abcdef0123456789abcdef01234567"},
		{Type: "tarball", URL: "https://example.test/src.tar.gz"},
	}
	for _, source := range valid {
		if _, err := validateSandboxSource(source); err != nil {
			t.Errorf("validateSandboxSource(%#v) = %v, want valid", source, err)
		}
	}
	invalid := map[string]sandboxSource{
		"unknown type":          {Type: "zip", URL: "https://example.test/a.zip"},
		"ssh url":               {Type: "git", URL: "git@example.test:repo.git"},
		"http url":              {Type: "git", URL: "http://example.test/repo.git"},
		"url with credentials":  {Type: "git", URL: "https://user:pw@example.test/repo.git"},
		"option-like revision":  {Type: "git", URL: "https://example.test/repo.git", Revision: "--upload-pack=x"},
		"depth too large":       {Type: "git", URL: "https://example.test/repo.git", Depth: depth(1001)},
		"depth zero":            {Type: "git", URL: "https://example.test/repo.git", Depth: depth(0)},
		"oversized token":       {Type: "git", URL: "https://example.test/repo.git", Token: strings.Repeat("t", 4097)},
		"token with newline":    {Type: "git", URL: "https://example.test/repo.git", Token: "a\nb"},
		"absolute path":         {Type: "git", URL: "https://example.test/repo.git", Path: "/etc"},
		"parent segment":        {Type: "git", URL: "https://example.test/repo.git", Path: "a/../../etc"},
		"dot-prefixed path":     {Type: "tarball", URL: "https://example.test/a.tar.gz", Path: ".boxd"},
		"trailing slash":        {Type: "git", URL: "https://example.test/repo.git", Path: "app/"},
		"tarball with revision": {Type: "tarball", URL: "https://example.test/a.tar.gz", Revision: "main"},
		"tarball with token":    {Type: "tarball", URL: "https://example.test/a.tar.gz", Token: "t"},
	}
	for name, source := range invalid {
		if _, err := validateSandboxSource(source); err == nil {
			t.Errorf("%s: validateSandboxSource(%#v) accepted an invalid source", name, source)
		}
	}
	defaulted, err := validateSandboxSource(sandboxSource{Type: "tarball", URL: "https://example.test/a.tar.gz"})
	if err != nil || defaulted.Path != "project" {
		t.Fatalf("default path = %q, err %v; want project", defaulted.Path, err)
	}
}

func TestCreateSandboxSourceSendsSetupWithoutPersistingToken(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()
	const nodeID = "nd_source"
	const nodeToken = "source-node-token"
	const sourceToken = "ghp_source_secret_value"
	if err := srv.store.CreateNode(context.Background(), store.NodeRecord{Node: types.Node{ID: nodeID, Name: "source", Status: "offline"}, TokenHash: tokenHash(nodeToken)}); err != nil {
		t.Fatalf("create source node: %v", err)
	}
	token := addPlatformSession(t, srv)

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
			_, _ = io.WriteString(w, `{"containerId":"source-container"}`)
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
		"type": "small", "ttlSeconds": nil,
		"source": map[string]any{"type": "git", "url": "https://example.test/private.git", "revision": "v2", "depth": 1, "token": sourceToken},
	}, http.StatusCreated, &created)
	if created["setupStatus"] != "running" {
		t.Fatalf("setupStatus = %#v, want running", created["setupStatus"])
	}
	encodedResponse, _ := json.Marshal(created)
	if strings.Contains(string(encodedResponse), sourceToken) {
		t.Fatal("sandbox response contains the source token")
	}
	if created["source"] == nil {
		t.Fatalf("sandbox response omits source: %#v", created)
	}

	select {
	case files := <-filesReceived:
		contents := make(map[string]string)
		var tokenMode any
		for _, file := range files {
			name, _ := file["path"].(string)
			encoded, _ := file["content"].(string)
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("decode agent file %s: %v", name, err)
			}
			contents[name] = string(decoded)
			if name == sourceTokenPath {
				tokenMode = file["mode"]
			}
		}
		if contents[sourceTokenPath] != sourceToken || tokenMode != float64(0600) {
			t.Fatalf("source token file = %q mode %#v", contents[sourceTokenPath], tokenMode)
		}
		var setup struct {
			Repos []struct {
				Name        string `json:"name"`
				URL         string `json:"url"`
				Revision    string `json:"revision"`
				Depth       int    `json:"depth"`
				TokenSecret string `json:"tokenSecret"`
			} `json:"repos"`
		}
		if err := json.Unmarshal([]byte(contents["/run/boxes/setup.json"]), &setup); err != nil {
			t.Fatalf("decode setup.json: %v", err)
		}
		if len(setup.Repos) != 1 || setup.Repos[0].Name != "project" || setup.Repos[0].URL != "https://example.test/private.git" || setup.Repos[0].Revision != "v2" || setup.Repos[0].Depth != 1 || setup.Repos[0].TokenSecret != sourceTokenSecret {
			t.Fatalf("setup repos = %#v", setup.Repos)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sandbox provisioning did not reach the agent")
	}

	boxID, _ := created["id"].(string)
	record, err := srv.store.GetBox(context.Background(), boxID)
	if err != nil {
		t.Fatalf("get stored box: %v", err)
	}
	if record.Box.Source == nil || record.Box.Source.URL != "https://example.test/private.git" {
		t.Fatalf("stored source = %#v", record.Box.Source)
	}
	activity, err := srv.store.ListActivity(context.Background(), boxID, "", "", 100)
	if err != nil || len(activity) == 0 {
		t.Fatalf("list activity: %d entries, err %v", len(activity), err)
	}
	for _, entry := range activity {
		encoded, _ := json.Marshal(entry)
		if strings.Contains(string(encoded), sourceToken) {
			t.Fatalf("activity %s contains the source token", entry.Action)
		}
	}
}
