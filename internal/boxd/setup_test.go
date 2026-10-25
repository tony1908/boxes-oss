package boxd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// shaRejectingFake fails every "--branch" clone, as git does for a commit SHA.
type shaRejectingFake struct{ setupFake }

func (f *shaRejectingFake) Run(ctx context.Context, argv []string, dir string, env []string, stdout, stderr io.Writer) (int, error) {
	if hasItem(argv, "--branch") {
		f.calls = append(f.calls, append([]string(nil), argv...))
		f.envs = append(f.envs, append([]string(nil), env...))
		return 1, nil
	}
	return f.setupFake.Run(ctx, argv, dir, env, stdout, stderr)
}

func writeSetupDoc(t *testing.T, root, doc string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".boxd"), 0700); err != nil {
		t.Fatal(err)
	}
	setupFile := filepath.Join(root, "setup.json")
	if err := os.WriteFile(setupFile, []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	return setupFile
}

func TestSetupCloneDepthAndRevisionArgv(t *testing.T) {
	root := t.TempDir()
	fake := &setupFake{}
	setupFile := writeSetupDoc(t, root, `{"repos":[{"name":"project","url":"https://example.test/p.git","revision":"v1.2.3","depth":1}]}`)
	if err := RunSetup(context.Background(), SetupOptions{Root: root, SetupFile: setupFile, SecretsDir: filepath.Join(root, "secrets"), Executor: fake}); err != nil {
		t.Fatal(err)
	}
	want := []string{"git", "clone", "--depth", "1", "--branch", "v1.2.3", "--", "https://example.test/p.git", filepath.Join(root, "project")}
	if len(fake.calls) != 1 || !reflect.DeepEqual(fake.calls[0], want) {
		t.Fatalf("clone calls = %#v, want %#v", fake.calls, want)
	}
}

func TestSetupCloneFallsBackToDetachedRevisionForSHA(t *testing.T) {
	root := t.TempDir()
	fake := &shaRejectingFake{}
	sha := "0123456789abcdef0123456789abcdef01234567"
	setupFile := writeSetupDoc(t, root, `{"repos":[{"name":"project","url":"https://example.test/p.git","revision":"`+sha+`","depth":1}]}`)
	if err := RunSetup(context.Background(), SetupOptions{Root: root, SetupFile: setupFile, SecretsDir: filepath.Join(root, "secrets"), Executor: fake}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "project")
	want := [][]string{
		{"git", "clone", "--depth", "1", "--branch", sha, "--", "https://example.test/p.git", target},
		{"git", "clone", "--", "https://example.test/p.git", target},
		{"git", "-C", target, "checkout", "--detach", sha},
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("clone calls = %#v, want %#v", fake.calls, want)
	}
}

func TestSetupTarballRunsCurlWithArgvNotShellInterpolation(t *testing.T) {
	root := t.TempDir()
	fake := &setupFake{}
	url := "https://example.test/src.tar.gz?x=$(touch /pwned)"
	setupFile := writeSetupDoc(t, root, `{"tarballs":[{"name":"project","url":"`+url+`"}]}`)
	if err := RunSetup(context.Background(), SetupOptions{Root: root, SetupFile: setupFile, SecretsDir: filepath.Join(root, "secrets"), Executor: fake}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "project")
	if len(fake.calls) != 1 || len(fake.calls[0]) != 6 || fake.calls[0][0] != "/bin/sh" || fake.calls[0][3] != "sh" || fake.calls[0][4] != url || fake.calls[0][5] != target {
		t.Fatalf("tarball call = %#v", fake.calls)
	}
	if strings.Contains(fake.calls[0][2], "example.test") {
		t.Fatal("tarball URL interpolated into shell script")
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		t.Fatalf("tarball target not created: %v", err)
	}
}

func TestSetupFailedTarballLeavesNoTargetDir(t *testing.T) {
	root := t.TempDir()
	fake := &setupFake{exitCode: 1}
	setupFile := writeSetupDoc(t, root, `{"tarballs":[{"name":"project","url":"https://example.test/src.tar.gz"}]}`)
	if err := RunSetup(context.Background(), SetupOptions{Root: root, SetupFile: setupFile, SecretsDir: filepath.Join(root, "secrets"), Executor: fake}); err == nil {
		t.Fatal("RunSetup() succeeded for a failed tarball command")
	}
	if _, err := os.Stat(filepath.Join(root, "project")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed tarball left target dir behind: %v", err)
	}
}

func TestSetupSourceTokenIsUsedThenRemoved(t *testing.T) {
	root := t.TempDir()
	fake := &setupFake{}
	secrets := filepath.Join(root, "secrets")
	if err := os.MkdirAll(secrets, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "source-token"), []byte("src-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	setupFile := writeSetupDoc(t, root, `{"repos":[{"name":"project","url":"https://example.test/p.git","tokenSecret":"source-token"}]}`)
	if err := RunSetup(context.Background(), SetupOptions{Root: root, SetupFile: setupFile, SecretsDir: secrets, Executor: fake}); err != nil {
		t.Fatal(err)
	}
	wantAuth := "GIT_CONFIG_VALUE_0=Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:src-token"))
	if len(fake.envs) != 1 || !hasItem(fake.envs[0], wantAuth) {
		t.Fatalf("clone env missing source token header: %#v", fake.envs)
	}
	if _, err := os.Stat(filepath.Join(secrets, "source-token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source token file still exists: %v", err)
	}
}

func TestRunSetupUnreadableConfigPersistsFailedStatus(t *testing.T) {
	root := t.TempDir()
	// A directory where the config file should be fails ReadFile with a non-not-exist error.
	setupFile := filepath.Join(root, "setup.json")
	if err := os.Mkdir(setupFile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := RunSetup(context.Background(), SetupOptions{Root: root, SetupFile: setupFile, Executor: &setupFake{}}); err == nil {
		t.Fatal("RunSetup() succeeded for an unreadable config")
	}
	status, err := os.ReadFile(filepath.Join(root, ".boxd", "setup.status"))
	if err != nil {
		t.Fatalf("read setup status: %v", err)
	}
	if string(status) != "failed\n" {
		t.Fatalf("setup status = %q, want failed", status)
	}
}
