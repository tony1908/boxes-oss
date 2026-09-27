package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHelpListsSupportedBoxSizes(t *testing.T) {
	var output bytes.Buffer
	app := CLI{Out: &output}
	if err := app.Run(context.Background(), []string{"--help"}); err != nil {
		t.Fatal(err)
	}
	for _, size := range []string{"xsmall", "small", "default", "large", "xlarge"} {
		if !strings.Contains(output.String(), size) {
			t.Errorf("help does not list %q size: %s", size, output.String())
		}
	}
	if !strings.Contains(output.String(), "boxes push <id> [dir] [--to path]") {
		t.Fatalf("help omits push usage: %s", output.String())
	}
}

func TestLoginStoresCredentialsWithPrivatePermissions(t *testing.T) {
	var got struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/login" {
			t.Errorf("request = %s %s, want POST /api/v1/auth/login", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode login body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"session-secret"}`)
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.json")
	var out, stderr bytes.Buffer
	app := CLI{In: strings.NewReader("correct horse\n"), Out: &out, Err: &stderr, ConfigPath: configPath, HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"login", "--server", server.URL, "--email", "user@example.test"}); err != nil {
		t.Fatalf("Run login: %v (stderr: %s)", err, stderr.String())
	}
	if got.Email != "user@example.test" || got.Password != "correct horse" {
		t.Fatalf("login credentials = %#v", got)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if config.Server != server.URL || config.Token != "session-secret" {
		t.Fatalf("saved config = %#v", config)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %04o, want 0600", info.Mode().Perm())
	}
}

func TestLoginUsesPasswordEnvironmentWhenFlagIsMissing(t *testing.T) {
	t.Setenv("BOXES_PASSWORD", "environment horse")
	var got struct {
		Password string `json:"password"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode login body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"session-secret"}`)
	}))
	defer server.Close()

	var stderr bytes.Buffer
	app := CLI{In: strings.NewReader(""), Out: io.Discard, Err: &stderr, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"login", "--server", server.URL, "--email", "user@example.test"}); err != nil {
		t.Fatalf("Run login: %v (stderr: %s)", err, stderr.String())
	}
	if got.Password != "environment horse" {
		t.Fatalf("login password = %q, want password from BOXES_PASSWORD", got.Password)
	}
}

func TestLoginWithAPIKeyValidatesAndStoresCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/me" {
			t.Errorf("request = %s %s, want GET /api/v1/me", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer bx_live_test-key" {
			t.Errorf("Authorization = %q, want API key bearer", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"user":{"email":"agent@example.test"}}`)
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.json")
	var out bytes.Buffer
	app := CLI{Out: &out, Err: io.Discard, ConfigPath: configPath, HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"login", "--server", server.URL, "--api-key", "bx_live_test-key"}); err != nil {
		t.Fatalf("Run API-key login: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if config.Server != server.URL || config.Token != "bx_live_test-key" {
		t.Fatalf("saved config = %#v", config)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %04o, want 0600", info.Mode().Perm())
	}
	if !strings.Contains(out.String(), "agent@example.test") {
		t.Fatalf("login output = %q, want validated account email", out.String())
	}
}

func TestLoginUsesAPIKeyEnvironment(t *testing.T) {
	t.Setenv("BOXES_API_KEY", "bx_live_environment-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/me" || r.Header.Get("Authorization") != "Bearer bx_live_environment-key" {
			t.Errorf("request = %s %s Authorization %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"user":{"email":"ci@example.test"}}`)
	}))
	defer server.Close()

	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"login", "--server", server.URL}); err != nil {
		t.Fatalf("Run environment API-key login: %v", err)
	}
}

func TestWhoamiUsesEnvironmentOverridesAndPrintsEmail(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server":"http://config.example.test","token":"config-token"}`), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/me" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer environment-token" {
			t.Errorf("Authorization = %q, want environment token", got)
		}
		_, _ = io.WriteString(w, `{"user":{"email":"agent@example.test"}}`)
	}))
	defer server.Close()
	t.Setenv("BOXES_SERVER", server.URL)
	t.Setenv("BOXES_TOKEN", "environment-token")

	var out bytes.Buffer
	app := CLI{Out: &out, Err: io.Discard, ConfigPath: configPath, HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"whoami"}); err != nil {
		t.Fatalf("Run whoami: %v", err)
	}
	if want := server.URL + "\nagent@example.test\n"; out.String() != want {
		t.Fatalf("whoami output = %q, want %q", out.String(), want)
	}
}

func TestCommandsUseBOXESAPIKEYWithoutConfig(t *testing.T) {
	t.Setenv("BOXES_SERVER", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sandboxes" || r.Header.Get("Authorization") != "Bearer bx_live_ci-key" {
			t.Errorf("request = %s %s Authorization %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"sandboxes":[]}`)
	}))
	defer server.Close()
	t.Setenv("BOXES_SERVER", server.URL)
	t.Setenv("BOXES_API_KEY", "bx_live_ci-key")

	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "missing.json"), HTTPClient: server.Client()}
	if err := app.Run(context.Background(), []string{"list"}); err != nil {
		t.Fatalf("Run list with environment API key: %v", err)
	}
}

func TestNewSendsDocumentedSandboxRequestAndPrintsJSON(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sandboxes" {
			t.Errorf("request = %s %s, want POST /api/v1/sandboxes", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer api-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Idempotency-Key") == "" {
			t.Error("Idempotency-Key header is empty")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"bx_abcdefghijkl","type":"large","state":"ready"}`)
	}))
	defer server.Close()

	var out, stderr bytes.Buffer
	app := CLI{Out: &out, Err: &stderr, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token"}
	err := app.Run(context.Background(), []string{"--json", "new", "--type", "large", "--ttl", "900", "--env", "env_demo"})
	if err != nil {
		t.Fatalf("Run new: %v (stderr: %s)", err, stderr.String())
	}
	if got["type"] != "large" || got["ttlSeconds"] != float64(900) || got["environment"] != "env_demo" {
		t.Fatalf("new request = %#v", got)
	}
	var result map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &result); err != nil {
		t.Fatalf("output is not one JSON value: %q: %v", out.String(), err)
	}
	if result["id"] != "bx_abcdefghijkl" {
		t.Fatalf("output = %#v", result)
	}
}

func TestListAcceptsJSONFlagAfterCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/sandboxes" {
			t.Errorf("request = %s %s, want GET /api/v1/sandboxes", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sandboxes":[{"id":"bx_abcdefghijkl","state":"ready","type":"small"}]}`)
	}))
	defer server.Close()

	var out bytes.Buffer
	app := CLI{Out: &out, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token"}
	if err := app.Run(context.Background(), []string{"list", "--json"}); err != nil {
		t.Fatalf("Run list --json: %v", err)
	}
	var boxes []map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &boxes); err != nil {
		t.Fatalf("list output is not JSON: %q: %v", out.String(), err)
	}
	if len(boxes) != 1 || boxes[0]["id"] != "bx_abcdefghijkl" {
		t.Fatalf("list output = %#v", boxes)
	}
}

func TestExecPreservesArgumentBoundaries(t *testing.T) {
	var got struct {
		Cmd []string `json:"cmd"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sandboxes/bx_abcdefghijkl/commands" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode command: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"exitCode":0,"stdout":"hello world\n","stderr":""}`)
	}))
	defer server.Close()

	var out, stderr bytes.Buffer
	app := CLI{Out: &out, Err: &stderr, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token"}
	if err := app.Run(context.Background(), []string{"exec", "bx_abcdefghijkl", "--", "printf", "%s", "hello world"}); err != nil {
		t.Fatalf("Run exec: %v (stderr: %s)", err, stderr.String())
	}
	if len(got.Cmd) != 3 || got.Cmd[0] != "printf" || got.Cmd[1] != "%s" || got.Cmd[2] != "hello world" {
		t.Fatalf("command argv = %#v", got.Cmd)
	}
	if out.String() != "hello world\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestExternalCommandUsesWindowsOpenSSHExecutableNames(t *testing.T) {
	for _, test := range []struct {
		goos string
		name string
		want string
	}{
		{goos: "windows", name: "ssh", want: "ssh.exe"},
		{goos: "windows", name: "scp", want: "scp.exe"},
		{goos: "linux", name: "ssh", want: "ssh"},
		{goos: "darwin", name: "scp", want: "scp"},
	} {
		if got := externalCommand(test.name, test.goos); got != test.want {
			t.Errorf("externalCommand(%q, %q) = %q, want %q", test.name, test.goos, got, test.want)
		}
	}
}

func TestSSHForwardsOptionalRemoteCommand(t *testing.T) {
	keyDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(keyDir, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, ".ssh", "id_ed25519.pub"), []byte("ssh-ed25519 AAAATEST key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", keyDir)
	keyInstalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sandboxes/bx_abcdefghijkl/ssh-key" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		keyInstalled = true
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	var commandName string
	var commandArgs []string
	configDir := t.TempDir()
	app := CLI{
		Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(configDir, "config.json"),
		HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token",
		RunCommand: func(_ context.Context, name string, args ...string) error {
			commandName, commandArgs = name, append([]string(nil), args...)
			return nil
		},
	}
	if err := app.Run(context.Background(), []string{"ssh", "bx_abcdefghijkl", "--", "printf", "%s", "hello world"}); err != nil {
		t.Fatalf("Run ssh: %v", err)
	}
	if !keyInstalled {
		t.Fatal("SSH public key was not installed")
	}
	knownHosts := filepath.Join(configDir, "known_hosts")
	want := []string{
		"-o", "ProxyCommand=boxes proxy bx_abcdefghijkl",
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "HostKeyAlias=bx_abcdefghijkl",
		"abc@bx_abcdefghijkl", "printf", "%s", "hello world",
	}
	if commandName != "ssh" || !reflect.DeepEqual(commandArgs, want) {
		t.Fatalf("ssh invocation = %q %#v, want ssh %#v", commandName, commandArgs, want)
	}
	info, err := os.Stat(knownHosts)
	if err != nil {
		t.Fatalf("known_hosts file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("known_hosts permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestSCPInstallsSSHKeyForReferencedBox(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519.pub"), []byte("ssh-ed25519 AAAATEST key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	installed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sandboxes/bx_abcdefghijkl/ssh-key" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		installed = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	app := CLI{Out: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), BaseURL: server.URL, HTTPClient: server.Client(), RunCommand: func(context.Context, string, ...string) error { return nil }}
	if err := app.Run(context.Background(), []string{"scp", "./repo", "bx_abcdefghijkl:/config/repo"}); err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Fatal("scp did not install SSH public key")
	}
}

func TestSSHWithoutRemoteCommandStaysInteractive(t *testing.T) {
	keyDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(keyDir, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, ".ssh", "id_ed25519.pub"), []byte("ssh-ed25519 AAAATEST key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", keyDir)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	defer server.Close()
	var commandArgs []string
	app := CLI{Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token",
		RunCommand: func(_ context.Context, _ string, args ...string) error { commandArgs = args; return nil }}
	if err := app.Run(context.Background(), []string{"ssh", "bx_abcdefghijkl"}); err != nil {
		t.Fatalf("Run ssh: %v", err)
	}
	want := []string{
		"-o", "ProxyCommand=boxes proxy bx_abcdefghijkl",
		"-o", "UserKnownHostsFile=" + filepath.Join(filepath.Dir(app.ConfigPath), "known_hosts"),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "HostKeyAlias=bx_abcdefghijkl",
		"abc@bx_abcdefghijkl",
	}
	if !reflect.DeepEqual(commandArgs, want) {
		t.Fatalf("ssh args = %#v, want %#v", commandArgs, want)
	}
}

func TestSCPUsesDedicatedKnownHostsAndBoxAlias(t *testing.T) {
	configDir := t.TempDir()
	keyDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(keyDir, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, ".ssh", "id_ed25519.pub"), []byte("ssh-ed25519 AAAATEST key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", keyDir)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	var commandName string
	var commandArgs []string
	app := CLI{
		Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(configDir, "config.json"),
		BaseURL: server.URL, HTTPClient: server.Client(), Token: "api-token",
		RunCommand: func(_ context.Context, name string, args ...string) error {
			commandName, commandArgs = name, append([]string(nil), args...)
			return nil
		},
	}
	if err := app.Run(context.Background(), []string{"scp", "./local.txt", "bx_abcdefghijkl:/tmp/remote.txt"}); err != nil {
		t.Fatalf("Run scp: %v", err)
	}
	knownHosts := filepath.Join(configDir, "known_hosts")
	want := []string{
		"-o", "ProxyCommand=boxes proxy %h",
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "HostKeyAlias=bx_abcdefghijkl",
		"./local.txt", "abc@bx_abcdefghijkl:/tmp/remote.txt",
	}
	if commandName != "scp" || !reflect.DeepEqual(commandArgs, want) {
		t.Fatalf("scp invocation = %q %#v, want scp %#v", commandName, commandArgs, want)
	}
	info, err := os.Stat(knownHosts)
	if err != nil {
		t.Fatalf("known_hosts file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("known_hosts permissions = %o, want 600", info.Mode().Perm())
	}
}

func TestClientReturnsStructuredServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"sandbox not found"}}`)
	}))
	defer server.Close()

	client := NewClient(server.URL, "api-token", server.Client())
	_, err := client.Do(context.Background(), http.MethodGet, "/sandboxes/missing", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "sandbox not found") {
		t.Fatalf("Do error = %v, want structured server message", err)
	}
}

func TestPromptSendsOptionsAndFollowsRunEvents(t *testing.T) {
	var promptBody map[string]any
	var eventRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sandboxes/bx_abcdefghijkl/prompt":
			if err := json.NewDecoder(r.Body).Decode(&promptBody); err != nil {
				t.Errorf("decode prompt: %v", err)
			}
			_, _ = io.WriteString(w, `{"runId":"run_123","conversationId":"cv_123"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sandboxes/bx_abcdefghijkl/events":
			eventRequests++
			wantAfter := "0"
			if eventRequests > 1 {
				wantAfter = "1"
			}
			if r.URL.Query().Get("conversation") != "cv_123" || r.URL.Query().Get("after") != wantAfter {
				t.Errorf("event query = %v", r.URL.Query())
			}
			if eventRequests == 1 {
				_, _ = io.WriteString(w, `{"events":[{"cursor":1,"id":"ev_1","conversationId":"cv_123","runId":"run_123","type":"message","data":{"text":"hello"}}],"cursor":1}`)
			} else {
				_, _ = io.WriteString(w, `{"events":[],"cursor":1}`)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sandboxes/bx_abcdefghijkl/runs/run_123":
			_, _ = io.WriteString(w, `{"id":"run_123","conversationId":"cv_123","status":"completed"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var out bytes.Buffer
	app := CLI{In: strings.NewReader(""), Out: &out, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token"}
	if err := app.Run(context.Background(), []string{"--json", "prompt", "bx_abcdefghijkl", "explain this", "--provider", "codex", "--model", "gpt-test", "--new"}); err != nil {
		t.Fatalf("Run prompt: %v", err)
	}
	if promptBody["prompt"] != "explain this" || promptBody["provider"] != "codex" || promptBody["model"] != "gpt-test" || promptBody["new"] != true {
		t.Fatalf("prompt body = %#v", promptBody)
	}
	if eventRequests != 2 || !strings.Contains(out.String(), `"runId":"run_123"`) {
		t.Fatalf("event requests = %d; output = %q", eventRequests, out.String())
	}
}

func TestPromptFetchesFinalEventsAfterRunBecomesTerminal(t *testing.T) {
	var eventRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sandboxes/bx_abcdefghijkl/prompt":
			_, _ = io.WriteString(w, `{"runId":"run_final","conversationId":"cv_final"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sandboxes/bx_abcdefghijkl/events":
			eventRequests++
			if eventRequests == 1 {
				_, _ = io.WriteString(w, `{"events":[],"cursor":0}`)
				return
			}
			_, _ = io.WriteString(w, `{"events":[{"cursor":1,"id":"ev_message","conversationId":"cv_final","runId":"run_final","type":"message","data":{"text":"boxes-ok"}},{"cursor":2,"id":"ev_complete","conversationId":"cv_final","runId":"run_final","type":"turn.completed","data":{}}],"cursor":2}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sandboxes/bx_abcdefghijkl/runs/run_final":
			_, _ = io.WriteString(w, `{"id":"run_final","conversationId":"cv_final","status":"completed"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var out bytes.Buffer
	app := CLI{In: strings.NewReader(""), Out: &out, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token"}
	if err := app.Run(context.Background(), []string{"--json", "prompt", "bx_abcdefghijkl", "say boxes-ok"}); err != nil {
		t.Fatalf("Run prompt: %v", err)
	}
	if eventRequests != 2 || !strings.Contains(out.String(), "boxes-ok") {
		t.Fatalf("event requests = %d; output = %q, want final message", eventRequests, out.String())
	}
}

func TestStopSendsNoSnapshotFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sandboxes/bx_abcdefghijkl/stop" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.ContentLength != 0 {
			body, _ := io.ReadAll(r.Body)
			t.Errorf("stop body = %q, want empty", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"snapshot":{"id":"sn_123"}}`)
	}))
	defer server.Close()

	app := CLI{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token"}
	if err := app.Run(context.Background(), []string{"stop", "bx_abcdefghijkl"}); err != nil {
		t.Fatalf("Run stop: %v", err)
	}
}

func TestSnapshotPullStreamsArchiveToOutputFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/snapshots/sn_123/download" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = io.WriteString(w, "archive-stream")
	}))
	defer server.Close()
	output := filepath.Join(t.TempDir(), "snapshot.tar.gz")
	var stderr bytes.Buffer
	app := CLI{In: strings.NewReader(""), Out: io.Discard, Err: &stderr, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token"}
	if err := app.Run(context.Background(), []string{"snapshot", "pull", "sn_123", "-o", output}); err != nil {
		t.Fatalf("Run snapshot pull: %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "archive-stream" || !strings.Contains(stderr.String(), "(14 bytes)") || strings.Contains(stderr.String(), "Downloaded") {
		t.Fatalf("file=%q progress=%q", got, stderr.String())
	}
}

func TestDesktopPrintsAndOpensReturnedURL(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sandboxes/bx_abcdefghijkl/desktop" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode desktop body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"url":"https://desk.example.test/?token=secret"}`)
	}))
	defer server.Close()

	var out bytes.Buffer
	opened := ""
	app := CLI{In: strings.NewReader(""), Out: &out, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token", OpenBrowser: func(_ context.Context, target string) error { opened = target; return nil }}
	if err := app.Run(context.Background(), []string{"desktop", "bx_abcdefghijkl", "--public"}); err != nil {
		t.Fatalf("Run desktop: %v", err)
	}
	if requestBody["public"] != true || opened != "https://desk.example.test/?token=secret" || !strings.Contains(out.String(), opened) {
		t.Fatalf("body=%#v opened=%q output=%q", requestBody, opened, out.String())
	}
}

func TestHostAcceptsPrivateFlagAfterPort(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sandboxes/bx_abcdefghijkl/hosts" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Errorf("decode host body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"host":"service.example.test","port":8080,"private":true}`)
	}))
	defer server.Close()

	app := CLI{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard, ConfigPath: filepath.Join(t.TempDir(), "config.json"), HTTPClient: server.Client(), BaseURL: server.URL, Token: "api-token"}
	if err := app.Run(context.Background(), []string{"host", "bx_abcdefghijkl", "8080", "--private"}); err != nil {
		t.Fatalf("Run host: %v", err)
	}
	if requestBody["port"] != float64(8080) || requestBody["private"] != true {
		t.Fatalf("host body = %#v", requestBody)
	}
}

func TestDefaultConfigPathUsesDocumentedConfigDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
	app := CLI{}
	app.defaults()
	if want := filepath.Join(home, ".config", "boxes", "config.json"); app.ConfigPath != want {
		t.Fatalf("ConfigPath = %q, want %q", app.ConfigPath, want)
	}
}
