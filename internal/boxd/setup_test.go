package boxd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type setupFake struct {
	calls    [][]string
	envs     [][]string
	exitCode int
}

func (f *setupFake) Run(_ context.Context, argv []string, _ string, env []string, stdout, stderr io.Writer) (int, error) {
	f.calls = append(f.calls, append([]string(nil), argv...))
	f.envs = append(f.envs, append([]string(nil), env...))
	if stdout != nil {
		_, _ = stdout.Write([]byte("setup output\n"))
	}
	return f.exitCode, nil
}

func TestSetupRunsScriptOnceAndWritesHarnessMCPConfigs(t *testing.T) {
	root := t.TempDir()
	fake := &setupFake{}
	secrets := filepath.Join(root, "secrets")
	if err := os.MkdirAll(secrets, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "git-token"), []byte("private-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".boxd"), 0700); err != nil {
		t.Fatal(err)
	}
	setupFile := filepath.Join(root, "setup.json")
	if err := os.WriteFile(setupFile, []byte(`{"repos":[{"name":"sample","url":"https://example.test/private.git","branch":"main","tokenSecret":"git-token"}],"setupScript":"printf done"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RunSetup(context.Background(), SetupOptions{Root: root, SetupFile: setupFile, SecretsDir: secrets, Executor: fake}); err != nil {
		t.Fatal(err)
	}
	if err := RunSetup(context.Background(), SetupOptions{Root: root, SetupFile: setupFile, SecretsDir: secrets, Executor: fake}); err != nil {
		t.Fatal(err)
	}
	wantClone := []string{"git", "clone", "--branch", "main", "--", "https://example.test/private.git", filepath.Join(root, "sample")}
	if len(fake.calls) != 2 || !reflect.DeepEqual(fake.calls[0], wantClone) || !reflect.DeepEqual(fake.calls[1], []string{"/bin/sh", "-lc", "printf done"}) {
		t.Fatalf("setup calls = %#v", fake.calls)
	}
	if strings.Contains(strings.Join(fake.calls[0], " "), "private-token") {
		t.Fatal("repository token leaked into command arguments")
	}
	wantAuth := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:private-token"))
	if !hasItem(fake.envs[0], "GIT_CONFIG_VALUE_0="+wantAuth) {
		t.Fatalf("git clone authorization not passed through process environment")
	}
	for path, contains := range map[string]string{
		filepath.Join(root, ".boxd", "mcp.json"):                    "\"command\": \"boxd\"",
		filepath.Join(root, ".codex", "config.toml"):                "[mcp_servers.computer]",
		filepath.Join(root, ".config", "opencode", "opencode.json"): "\"computer\"",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), contains) {
			t.Errorf("%s missing %q: %s", path, contains, data)
		}
	}
}

func TestRunSetupPersistsSuccessfulSetupStatus(t *testing.T) {
	root := t.TempDir()
	setupFile := filepath.Join(root, "setup.json")
	if err := os.WriteFile(setupFile, []byte(`{"setupScript":"printf done"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RunSetup(context.Background(), SetupOptions{Root: root, SetupFile: setupFile, Executor: &setupFake{}}); err != nil {
		t.Fatalf("RunSetup(): %v", err)
	}
	status, err := os.ReadFile(filepath.Join(root, ".boxd", "setup.status"))
	if err != nil {
		t.Fatalf("read setup status: %v", err)
	}
	if string(status) != "succeeded\n" {
		t.Fatalf("setup status = %q, want succeeded", status)
	}
}

func TestRunSetupPersistsFailedSetupStatus(t *testing.T) {
	root := t.TempDir()
	setupFile := filepath.Join(root, "setup.json")
	if err := os.WriteFile(setupFile, []byte(`{"setupScript":"exit 1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RunSetup(context.Background(), SetupOptions{Root: root, SetupFile: setupFile, Executor: &setupFake{exitCode: 1}}); err == nil {
		t.Fatal("RunSetup() succeeded for a failed script")
	}
	status, err := os.ReadFile(filepath.Join(root, ".boxd", "setup.status"))
	if err != nil {
		t.Fatalf("read setup status: %v", err)
	}
	if string(status) != "failed\n" {
		t.Fatalf("setup status = %q, want failed", status)
	}
}

func TestWriteMCPConfigsMergesExistingProviderConfigs(t *testing.T) {
	root := t.TempDir()
	for path, contents := range map[string]string{
		filepath.Join(root, ".boxd", "mcp.json"):                    `{"mcpServers":{"existing":{"command":"keep-me"}}}`,
		filepath.Join(root, ".codex", "config.toml"):                "model = \"keep-me\"\n",
		filepath.Join(root, ".config", "opencode", "opencode.json"): `{"model":"keep-me","mcp":{"existing":{"enabled":true}}}`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}

	if err := writeMCPConfigs(root); err != nil {
		t.Fatal(err)
	}
	if err := writeMCPConfigs(root); err != nil {
		t.Fatal(err)
	}

	var claude map[string]any
	if data, err := os.ReadFile(filepath.Join(root, ".boxd", "mcp.json")); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(data, &claude); err != nil {
		t.Fatalf("decode Claude config: %v", err)
	}
	claudeServers := claude["mcpServers"].(map[string]any)
	if claudeServers["existing"].(map[string]any)["command"] != "keep-me" || claudeServers["computer"] == nil {
		t.Fatalf("Claude MCP servers = %#v", claudeServers)
	}

	codex, err := os.ReadFile(filepath.Join(root, ".codex", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(codex), `model = "keep-me"`) || strings.Count(string(codex), "[mcp_servers.computer]") != 1 {
		t.Fatalf("Codex config = %q", codex)
	}

	var opencode map[string]any
	if data, err := os.ReadFile(filepath.Join(root, ".config", "opencode", "opencode.json")); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal(data, &opencode); err != nil {
		t.Fatalf("decode OpenCode config: %v", err)
	}
	opencodeMCP := opencode["mcp"].(map[string]any)
	if opencode["model"] != "keep-me" || opencodeMCP["existing"].(map[string]any)["enabled"] != true || opencodeMCP["computer"] == nil {
		t.Fatalf("OpenCode config = %#v", opencode)
	}
}

func hasItem(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
