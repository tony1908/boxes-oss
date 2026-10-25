package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestNewLifecycleOptionsSaveCurrentID(t *testing.T) {
	setupFile := filepath.Join(t.TempDir(), "setup.sh")
	if err := os.WriteFile(setupFile, []byte("echo setup\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sandboxes" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_, _ = io.WriteString(w, `{"sandbox":{"id":"bx_creatednew22","state":"ready"}}`)
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.json")
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: configPath, HTTPClient: server.Client(), BaseURL: server.URL}
	err := app.Run(context.Background(), []string{"new", "--no-auto-stop", "-e", "A=one", "-e", "B=two", "--setup-file", setupFile, "--from", "base", "--environment", "dev", "--no-env"})
	if err != nil {
		t.Fatalf("Run new: %v", err)
	}
	if body["ttlSeconds"] != nil || body["env"].(map[string]any)["A"] != "one" || body["env"].(map[string]any)["B"] != "two" || body["setupScript"] != "echo setup\n" || body["from"] != "base" || body["environment"] != "dev" || body["noEnv"] != true {
		t.Fatalf("new body = %#v", body)
	}
	var config map[string]any
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config["current"] != "bx_creatednew22" {
		t.Fatalf("current = %v, want created sandbox ID", config["current"])
	}
}

func TestCurrentPersistenceDoesNotSaveEnvironmentCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer transient-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"id":"bx_abcdefghijkl","state":"ready"}`)
	}))
	defer server.Close()
	t.Setenv("BOXES_SERVER", server.URL)
	t.Setenv("BOXES_TOKEN", "transient-token")
	configPath := filepath.Join(t.TempDir(), "config.json")
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: configPath, HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"info", "bx_abcdefghijkl"}); err != nil {
		t.Fatalf("info: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config["token"] == "transient-token" || config["server"] == server.URL {
		t.Fatalf("environment credentials were persisted: %#v", config)
	}
	if config["current"] != "bx_abcdefghijkl" {
		t.Fatalf("current = %v", config["current"])
	}
}

func TestListLifecycleFilters(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		_, _ = io.WriteString(w, `{"sandboxes":[]}`)
	}))
	defer server.Close()
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL}
	for _, args := range [][]string{{"list"}, {"list", "--filter", "se"}, {"list", "--all"}} {
		if err := app.Run(context.Background(), args); err != nil {
			t.Fatalf("Run %v: %v", args, err)
		}
	}
	want := []string{"filter=r", "filter=se", ""}
	for i := range want {
		if queries[i] != want[i] {
			t.Errorf("query[%d] = %q, want %q", i, queries[i], want[i])
		}
	}
}

func TestLifecycleCommandsResolveCurrentAndSendTTL(t *testing.T) {
	var requests []struct {
		method, path string
		body         map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := struct {
			method, path string
			body         map[string]any
		}{method: r.Method, path: r.URL.Path}
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&request.body); err != nil {
				t.Errorf("decode body: %v", err)
			}
		}
		requests = append(requests, request)
		_, _ = io.WriteString(w, `{"id":"bx_current","state":"ready"}`)
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server":"`+server.URL+`","token":"test","current":"bx_current"}`), 0600); err != nil {
		t.Fatal(err)
	}
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: configPath, HTTPClient: server.Client()}
	commands := [][]string{
		{"extend", "current", "--ttl", "7200"},
		{"resume", "current", "--ttl", "900", "--type", "large"},
		{"fork", "current", "--no-auto-stop"},
		{"info", "current"},
	}
	for _, args := range commands {
		if err := app.Run(context.Background(), args); err != nil {
			t.Fatalf("Run %v: %v", args, err)
		}
	}
	if len(requests) != 4 {
		t.Fatalf("requests = %#v", requests)
	}
	if requests[0].method != http.MethodPatch || requests[0].path != "/api/v1/sandboxes/bx_current" || requests[0].body["ttlSeconds"] != float64(7200) {
		t.Errorf("extend request = %#v", requests[0])
	}
	if requests[1].body["ttlSeconds"] != float64(900) || requests[1].body["type"] != "large" {
		t.Errorf("resume request = %#v", requests[1])
	}
	if _, ok := requests[2].body["ttlSeconds"]; !ok || requests[2].body["ttlSeconds"] != nil {
		t.Errorf("fork no-auto-stop body = %#v", requests[2].body)
	}
	if requests[3].method != http.MethodGet || requests[3].path != "/api/v1/sandboxes/bx_current" {
		t.Errorf("info request = %#v", requests[3])
	}
}

func TestPromptStopWhenDoneSendsAPIOption(t *testing.T) {
	var promptBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt"):
			_ = json.NewDecoder(r.Body).Decode(&promptBody)
			_, _ = io.WriteString(w, `{"runId":"run_1"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			_, _ = io.WriteString(w, `{"events":[],"cursor":0}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/runs/run_1"):
			_, _ = io.WriteString(w, `{"status":"completed"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL}
	if err := app.Run(context.Background(), []string{"prompt", "bx_abcdefghijkl", "do work", "--stop-when-done"}); err != nil {
		t.Fatalf("Run prompt: %v", err)
	}
	if promptBody["stopWhenDone"] != true {
		t.Fatalf("prompt body = %#v", promptBody)
	}
}

func TestPromptUsesCurrentWhenIDIsOmittedWithFlags(t *testing.T) {
	var promptBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sandboxes/bx_current/prompt":
			_ = json.NewDecoder(r.Body).Decode(&promptBody)
			_, _ = io.WriteString(w, `{"runId":"run_current"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			_, _ = io.WriteString(w, `{"events":[],"cursor":0}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/runs/run_current"):
			_, _ = io.WriteString(w, `{"status":"completed"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server":"`+server.URL+`","current":"bx_current"}`), 0600); err != nil {
		t.Fatal(err)
	}
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: configPath, HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"prompt", "do this", "--provider", "codex"}); err != nil {
		t.Fatalf("prompt using current: %v", err)
	}
	if promptBody["prompt"] != "do this" || promptBody["provider"] != "codex" {
		t.Fatalf("prompt body = %#v", promptBody)
	}
}

func TestExecDetachAndProcessStatus(t *testing.T) {
	var detached bool
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			detached = body["detached"] == true
			_, _ = io.WriteString(w, `{"processId":"pid_1"}`)
			return
		}
		_, _ = io.WriteString(w, `{"running":false,"exitCode":0}`)
	}))
	defer server.Close()
	var out bytes.Buffer
	app := CLI{Out: &out, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL}
	if err := app.Run(context.Background(), []string{"exec", "bx_abcdefghijkl", "--detach", "--", "sleep", "10"}); err != nil {
		t.Fatalf("detached exec: %v", err)
	}
	if err := app.Run(context.Background(), []string{"exec", "current", "--status", "pid_1"}); err != nil {
		t.Fatalf("exec status: %v", err)
	}
	if !detached || strings.Join(requests, "\n") != "POST /api/v1/sandboxes/bx_abcdefghijkl/commands\nGET /api/v1/sandboxes/bx_abcdefghijkl/commands/pid_1" {
		t.Fatalf("detached=%v requests=%#v", detached, requests)
	}
	if !strings.Contains(out.String(), "pid_1") {
		t.Fatalf("detached output = %q, want process ID", out.String())
	}
}

func TestInfoDisplaysLifecycleDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"bx_abcdefghijkl","state":"ready","autoStop":true,"ttlSeconds":3600,"ttlRemainingSeconds":1200,"setupStatus":"succeeded"}`)
	}))
	defer server.Close()
	var out bytes.Buffer
	app := CLI{Out: &out, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL}
	if err := app.Run(context.Background(), []string{"info", "bx_abcdefghijkl"}); err != nil {
		t.Fatalf("info: %v", err)
	}
	for _, field := range []string{"autoStop", "ttlSeconds", "ttlRemainingSeconds", "setupStatus"} {
		if !strings.Contains(out.String(), field) {
			t.Errorf("info output %q does not include %q", out.String(), field)
		}
	}
}

func TestForwardBridgesLocalTCPOverSandboxWebSocket(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sandboxes/bx_abcdefghijkl/tcp/9000" {
			t.Errorf("websocket path = %q", r.URL.Path)
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close(websocket.StatusNormalClosure, "done") }()
		conn := websocket.NetConn(r.Context(), ws, websocket.MessageBinary)
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := make(chan string, 1)
	app := CLI{Out: channelWriter{output}, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL}
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, []string{"forward", "bx_abcdefghijkl", "9000", "--local-port", "0"}) }()
	var localAddr string
	select {
	case line := <-output:
		_, err := fmt.Sscanf(line, "Forwarding %s to", &localAddr)
		if err != nil || localAddr == "" {
			t.Fatalf("forward output = %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forward listener did not start")
	}
	conn, err := net.DialTimeout("tcp", localAddr, time.Second)
	if err != nil {
		t.Fatalf("connect local forward: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 5)
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatalf("read echoed bytes: %v", err)
	}
	_ = conn.Close()
	if string(response) != "hello" {
		t.Fatalf("forward response = %q", response)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("forward returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forward did not stop after cancellation")
	}
}

type channelWriter struct{ out chan<- string }

func (w channelWriter) Write(data []byte) (int, error) {
	w.out <- string(data)
	return len(data), nil
}

func TestLoginReadsAPIKeyFromStdin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me" || r.Header.Get("Authorization") != "Bearer stdin-key" {
			t.Errorf("request = %s %s Authorization %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"user":{"email":"stdin@example.test"}}`)
	}))
	defer server.Close()
	app := CLI{In: strings.NewReader("stdin-key\n"), Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"login", "--server", server.URL, "--key-stdin"}); err != nil {
		t.Fatalf("Run login --key-stdin: %v", err)
	}
}

func TestDeleteRequiresYesWithoutInteractiveTerminal(t *testing.T) {
	app := CLI{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), BaseURL: "http://example.test"}
	err := app.Run(context.Background(), []string{"delete", "bx_abcdefghijkl"})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("delete error = %v, want --yes requirement", err)
	}
}

func TestDeleteYesAndCurrentCommands(t *testing.T) {
	var deleted string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deleted = r.Method + " " + r.URL.Path
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server":"`+server.URL+`","token":"secret","current":"bx_current"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	app := CLI{Out: &out, Err: io.Discard, ConfigPath: configPath, HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"current"}); err != nil {
		t.Fatalf("current: %v", err)
	}
	if !strings.Contains(out.String(), "bx_current") {
		t.Fatalf("current output = %q", out.String())
	}
	if err := app.Run(context.Background(), []string{"delete", "current", "--yes"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != "DELETE /api/v1/sandboxes/bx_current" {
		t.Fatalf("delete request = %q", deleted)
	}
}

func TestDeletePollsAcceptedOperation(t *testing.T) {
	var out bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			_, _ = io.WriteString(w, `{"operation":{"id":"op_delete","status":"pending"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"operation":{"id":"op_delete","status":"done"}}`)
	}))
	defer server.Close()
	app := CLI{Out: &out, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), BaseURL: server.URL, Token: "token", HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"delete", "bx_abcdefghijkl", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Deleted bx_abcdefghijkl") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestConversationSnapshotUsageAndLimitsEndpoints(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		_, _ = io.WriteString(w, `{"conversations":[],"snapshots":[],"runningSeconds":12,"limits":{}}`)
	}))
	defer server.Close()
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL}
	for _, args := range [][]string{{"conversations", "bx_abcdefghijkl"}, {"snapshots", "bx_abcdefghijkl"}, {"usage", "bx_abcdefghijkl"}, {"limits"}} {
		if err := app.Run(context.Background(), args); err != nil {
			t.Fatalf("Run %v: %v", args, err)
		}
	}
	want := []string{
		"GET /api/v1/sandboxes/bx_abcdefghijkl/conversations",
		"GET /api/v1/sandboxes/bx_abcdefghijkl/snapshots",
		"GET /api/v1/sandboxes/bx_abcdefghijkl/usage",
		"GET /api/v1/limits",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
}

func TestDashboardOpensCurrentBoxURL(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server":"https://boxes.example.test/","current":"bx_current"}`), 0600); err != nil {
		t.Fatal(err)
	}
	opened := ""
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: configPath, OpenBrowser: func(_ context.Context, target string) error { opened = target; return nil }}
	if err := app.Run(context.Background(), []string{"dashboard", "current"}); err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	if opened != "https://boxes.example.test/sandbox/?id=bx_current" {
		t.Fatalf("dashboard URL = %q", opened)
	}
}

func TestStatusLogoutAndForkTTL(t *testing.T) {
	var requests []string
	var forkBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&forkBody)
			_, _ = io.WriteString(w, `{"sandbox":{"id":"bx_forkedbox234","state":"ready"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"sandbox":{"id":"bx_current","state":"ready"}}`)
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server":"`+server.URL+`","token":"token","current":"bx_current"}`), 0600); err != nil {
		t.Fatal(err)
	}
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: configPath, HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"status"}); err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := app.Run(context.Background(), []string{"fork", "current", "--ttl", "600"}); err != nil {
		t.Fatalf("fork: %v", err)
	}
	var config map[string]any
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config["current"] != "bx_forkedbox234" {
		t.Fatalf("current after fork = %v", config["current"])
	}
	if forkBody["ttlSeconds"] != float64(600) {
		t.Fatalf("fork body = %#v", forkBody)
	}
	if err := app.Run(context.Background(), []string{"logout"}); err != nil {
		t.Fatalf("logout: %v", err)
	}
	data, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config = nil
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if config["server"] != "" || config["token"] != "" || config["current"] != nil {
		t.Fatalf("config after logout = %#v", config)
	}
	want := []string{
		"GET /api/v1/sandboxes/bx_current",
		"POST /api/v1/sandboxes/bx_current/fork",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
}

func TestSkillInstallProtectsModifiedFilesAndCanForce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeFile := filepath.Join(home, ".claude", "skills", "boxes", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(claudeFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeFile, []byte("user-owned\n"), 0600); err != nil {
		t.Fatal(err)
	}
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(home, ".config", "boxes", "config.json")}
	if err := app.Run(context.Background(), []string{"skill", "install", "--claude"}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("install modified skill error = %v, want force guidance", err)
	}
	if err := app.Run(context.Background(), []string{"skill", "install", "--claude", "--force"}); err != nil {
		t.Fatalf("force install: %v", err)
	}
	installed, err := os.ReadFile(claudeFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(installed, []byte("boxes-skill-managed")) || !bytes.Contains(installed, []byte("--stop-when-done")) || !bytes.Contains(installed, []byte("boxes extend")) {
		t.Fatalf("installed skill lacks marker/lifecycle guidance: %s", installed)
	}
}

func TestParseIdleStopFlag(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int64
		fail  bool
	}{{"30m", 1800, false}, {"off", 0, false}, {"4m", 0, true}, {"721h", 0, true}, {"bad", 0, true}} {
		got, err := parseIdleStopFlag(test.value)
		if (err != nil) != test.fail || (err == nil && got != test.want) {
			t.Errorf("parseIdleStopFlag(%q) = %d, %v", test.value, got, err)
		}
	}
}

func TestTagFlagsOnNewListAndUpdate(t *testing.T) {
	var methods, queries []string
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		queries = append(queries, r.URL.RawQuery)
		var body map[string]any
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
		}
		bodies = append(bodies, body)
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"sandbox":{"id":"bx_taggedbox234","tags":{"team":"agents","tier":"old"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"sandbox":{"id":"bx_taggedbox234","state":"ready"}}`)
	}))
	defer server.Close()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server":"`+server.URL+`","token":"test","current":"bx_taggedbox234"}`), 0600); err != nil {
		t.Fatal(err)
	}
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: configPath, HTTPClient: server.Client()}
	for _, args := range [][]string{
		{"new", "--tag", "team=agents", "--tag", "tier=gold"},
		{"list", "--tag", "team=agents", "--tag", "owner"},
		{"update", "current", "--tag", "tier=gold", "--tag", "owner=ci"},
		{"update", "current", "--clear-tags"},
	} {
		if err := app.Run(context.Background(), args); err != nil {
			t.Fatalf("Run %v: %v", args, err)
		}
	}
	if bodies[0]["tags"].(map[string]any)["tier"] != "gold" {
		t.Errorf("new tags body = %#v", bodies[0])
	}
	if queries[1] != "filter=r&tag=team%3Dagents&tag=owner" {
		t.Errorf("list query = %q", queries[1])
	}
	// update --tag: GET current tags, then PATCH the merged map.
	if methods[2] != "GET /api/v1/sandboxes/bx_taggedbox234" || methods[3] != "PATCH /api/v1/sandboxes/bx_taggedbox234" {
		t.Fatalf("update requests = %v", methods[2:4])
	}
	merged := bodies[3]["tags"].(map[string]any)
	if merged["team"] != "agents" || merged["tier"] != "gold" || merged["owner"] != "ci" || len(merged) != 3 {
		t.Errorf("merged tags = %#v", merged)
	}
	if clear := bodies[4]["tags"].(map[string]any); len(clear) != 0 {
		t.Errorf("clear-tags body = %#v, want empty map", bodies[4])
	}
	if err := app.Run(context.Background(), []string{"new", "--tag", "=bad"}); err == nil {
		t.Error("new accepted a tag without a key")
	}
	if err := app.Run(context.Background(), []string{"update", "current", "--tag", "a=b", "--clear-tags"}); err == nil {
		t.Error("update accepted --tag with --clear-tags")
	}
}
