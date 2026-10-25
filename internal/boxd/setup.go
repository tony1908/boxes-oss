package boxd

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tony1908/boxes-oss/internal/types"
)

// SetupOptions configures first-start repository and script setup.
type SetupOptions struct {
	Root       string
	SetupFile  string
	SecretsDir string
	Executor   SetupExecutor
}

// SetupExecutor runs setup commands without shell interpolation.
type SetupExecutor interface {
	Run(ctx context.Context, argv []string, dir string, env []string, stdout, stderr io.Writer) (int, error)
}

type osSetupExecutor struct{}

func (osSetupExecutor) Run(ctx context.Context, argv []string, dir string, env []string, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return -1, err
}

// sourceTokenSecret is the secret file carrying a source repository token; it is removed after cloning.
const sourceTokenSecret = "source-token"

type setupDocument struct {
	Repos []struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		Branch      string `json:"branch"`
		Revision    string `json:"revision"`
		Depth       int    `json:"depth"`
		TokenSecret string `json:"tokenSecret"`
	} `json:"repos"`
	Tarballs []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"tarballs"`
	SetupScript string `json:"setupScript"`
}

// RunSetup applies the configured setup once and records output events in the outbox.
func RunSetup(ctx context.Context, options SetupOptions) (returnErr error) {
	root := options.Root
	if root == "" {
		root = "/config"
	}
	setupFile := options.SetupFile
	if setupFile == "" {
		setupFile = "/run/boxes/setup.json"
	}
	data, err := os.ReadFile(setupFile)
	if errors.Is(err, os.ErrNotExist) {
		return writeMCPConfigs(root)
	}
	if err != nil {
		// The config exists but cannot be read, so setup cannot run; record the failure rather than leave no status.
		return errors.Join(err, writeSetupStatus(root, "failed"))
	}
	var doc setupDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("decode setup config: %w", err)
	}
	if err := writeMCPConfigs(root); err != nil {
		return err
	}
	marker := filepath.Join(root, ".boxd", "setup.done")
	configured := len(doc.Repos) != 0 || len(doc.Tarballs) != 0 || doc.SetupScript != ""
	if _, err := os.Stat(marker); err == nil {
		if configured {
			return writeSetupStatus(root, "succeeded")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if configured {
		if err := writeSetupStatus(root, "running"); err != nil {
			return err
		}
		defer func() {
			status := "succeeded"
			if returnErr != nil {
				status = "failed"
			}
			if err := writeSetupStatus(root, status); err != nil && returnErr == nil {
				returnErr = err
			}
		}()
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		return err
	}
	store, err := OpenOutbox(filepath.Join(root, ".boxd", "boxd.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	executor := options.Executor
	if executor == nil {
		executor = osSetupExecutor{}
	}
	env, err := loadHarnessEnv("/run/boxes/env")
	if err != nil {
		return err
	}
	secretsDir := options.SecretsDir
	if secretsDir == "" {
		secretsDir = "/run/boxes/secrets"
	}
	if err := cloneSetupRepos(ctx, executor, store, doc.Repos, root, secretsDir, env); err != nil {
		return err
	}
	for _, tarball := range doc.Tarballs {
		if strings.TrimSpace(tarball.Name) == "" || strings.TrimSpace(tarball.URL) == "" {
			return errors.New("setup tarball requires name and url")
		}
		target := filepath.Join(root, filepath.Clean(tarball.Name))
		if !inside(root, target) {
			return fmt.Errorf("setup tarball path %q is outside config directory", tarball.Name)
		}
		if _, err := os.Stat(target); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(target, 0755); err != nil {
			return err
		}
		// ponytail: .tar.gz only; add zip support when a caller needs it.
		// The URL and target are passed as argv, never interpolated into the shell string.
		args := []string{"/bin/sh", "-c", `curl -fsSL "$1" | tar -xz -C "$2" --strip-components=0`, "sh", tarball.URL, target}
		if err := runSetupCommand(ctx, executor, store, args, root, env); err != nil {
			// Remove the partial extraction so a later run does not treat it as already set up.
			_ = os.RemoveAll(target)
			return err
		}
	}
	if doc.SetupScript != "" {
		if err := runSetupCommand(ctx, executor, store, []string{"/bin/sh", "-lc", doc.SetupScript}, root, env); err != nil {
			return err
		}
	}
	return os.WriteFile(marker, []byte("done\n"), 0600)
}

// cloneSetupRepos clones each repository into root once. The source token secret file is removed when this returns.
func cloneSetupRepos(ctx context.Context, executor SetupExecutor, store *Outbox, repos []struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Branch      string `json:"branch"`
	Revision    string `json:"revision"`
	Depth       int    `json:"depth"`
	TokenSecret string `json:"tokenSecret"`
}, root, secretsDir string, env []string) error {
	defer os.Remove(filepath.Join(secretsDir, sourceTokenSecret))
	for _, repo := range repos {
		if strings.TrimSpace(repo.Name) == "" || strings.TrimSpace(repo.URL) == "" {
			return errors.New("setup repository requires name and url")
		}
		target := filepath.Join(root, filepath.Clean(repo.Name))
		if !inside(root, target) {
			return fmt.Errorf("setup repository path %q is outside config directory", repo.Name)
		}
		if _, err := os.Stat(target); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		revision := repo.Revision
		if revision == "" {
			revision = repo.Branch
		}
		repoEnv := append([]string(nil), env...)
		if repo.TokenSecret != "" {
			secretPath := filepath.Join(secretsDir, filepath.Clean(repo.TokenSecret))
			if !inside(secretsDir, secretPath) {
				return fmt.Errorf("repository token secret path %q is invalid", repo.TokenSecret)
			}
			secret, err := os.ReadFile(secretPath)
			if err != nil {
				return fmt.Errorf("read repository token secret %q: %w", repo.TokenSecret, err)
			}
			token := strings.TrimSpace(string(secret))
			if token == "" {
				return fmt.Errorf("repository token secret %q is empty", repo.TokenSecret)
			}
			authorization := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
			repoEnv = append(repoEnv, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraheader", "GIT_CONFIG_VALUE_0="+authorization)
		}
		args := []string{"git", "clone"}
		if repo.Depth > 0 {
			args = append(args, "--depth", strconv.Itoa(repo.Depth))
		}
		if revision == "" {
			if err := runSetupCommand(ctx, executor, store, append(args, "--", repo.URL, target), root, repoEnv); err != nil {
				_ = os.RemoveAll(target)
				return err
			}
			continue
		}
		// A branch or tag clones directly; a commit SHA fails that form, so fall back to a full clone and detach.
		err := runSetupCommand(ctx, executor, store, append(args, "--branch", revision, "--", repo.URL, target), root, repoEnv)
		if err == nil {
			continue
		}
		_ = os.RemoveAll(target)
		if err := runSetupCommand(ctx, executor, store, []string{"git", "clone", "--", repo.URL, target}, root, repoEnv); err != nil {
			_ = os.RemoveAll(target)
			return err
		}
		if err := runSetupCommand(ctx, executor, store, []string{"git", "-C", target, "checkout", "--detach", revision}, root, repoEnv); err != nil {
			_ = os.RemoveAll(target)
			return err
		}
	}
	return nil
}

func writeSetupStatus(root, status string) error {
	statusPath := filepath.Join(root, ".boxd", "setup.status")
	if err := os.MkdirAll(filepath.Dir(statusPath), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(statusPath, []byte(status+"\n"), 0600); err != nil {
		return err
	}
	store, err := OpenOutbox(filepath.Join(root, ".boxd", "boxd.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	_, err = store.AddEvent(Event{ID: types.NewID("ev_"), Type: "setup.status", Time: time.Now().UTC(), Data: map[string]any{"status": status}})
	return err
}

func runSetupCommand(ctx context.Context, executor SetupExecutor, store *Outbox, args []string, dir string, env []string) error {
	stdout := &setupEventWriter{store: store, stream: "stdout"}
	stderr := &setupEventWriter{store: store, stream: "stderr"}
	code, err := executor.Run(ctx, args, dir, env, stdout, stderr)
	stdout.flush()
	stderr.flush()
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("setup command %q exited with code %d", args[0], code)
	}
	return nil
}

type setupEventWriter struct {
	mu      sync.Mutex
	store   *Outbox
	stream  string
	pending string
}

func (w *setupEventWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(data)
	w.pending += string(data)
	for {
		line, rest, ok := strings.Cut(w.pending, "\n")
		if !ok {
			break
		}
		w.pending = rest
		w.emit(line)
	}
	return n, nil
}
func (w *setupEventWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending != "" {
		w.emit(w.pending)
		w.pending = ""
	}
}
func (w *setupEventWriter) emit(line string) {
	_, _ = w.store.AddEvent(Event{ID: types.NewID("ev_"), Type: "setup.output", Data: map[string]any{"stream": w.stream, "line": line}})
}

func writeMCPConfigs(root string) error {
	claudePath := filepath.Join(root, ".boxd", "mcp.json")
	if err := mergeJSONConfig(claudePath, func(config map[string]any) {
		servers := nestedObject(config, "mcpServers")
		servers["computer"] = map[string]any{"command": "boxd", "args": []string{"mcp"}}
	}); err != nil {
		return err
	}
	codexPath := filepath.Join(root, ".codex", "config.toml")
	if err := appendCodexMCP(codexPath); err != nil {
		return err
	}
	opencodePath := filepath.Join(root, ".config", "opencode", "opencode.json")
	if err := mergeJSONConfig(opencodePath, func(config map[string]any) {
		mcp := nestedObject(config, "mcp")
		mcp["computer"] = map[string]any{"type": "local", "command": []string{"boxd", "mcp"}, "enabled": true}
	}); err != nil {
		return err
	}
	return mergeJSONConfig(filepath.Join(root, ".pi", "mcp.json"), func(config map[string]any) {
		servers := nestedObject(config, "mcpServers")
		servers["computer"] = map[string]any{"command": "boxd", "args": []string{"mcp"}}
	})
}

func mergeJSONConfig(path string, merge func(map[string]any)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	config := make(map[string]any)
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("decode MCP config %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if config == nil {
		config = make(map[string]any)
	}
	merge(config)
	data, err = json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}

func nestedObject(parent map[string]any, key string) map[string]any {
	if child, ok := parent[key].(map[string]any); ok {
		return child
	}
	child := make(map[string]any)
	parent[key] = child
	return child
}

func appendCodexMCP(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "[mcp_servers.computer]" {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	var config strings.Builder
	config.Write(data)
	if config.Len() > 0 && !strings.HasSuffix(config.String(), "\n") {
		config.WriteByte('\n')
	}
	if config.Len() > 0 {
		config.WriteByte('\n')
	}
	config.WriteString("[mcp_servers.computer]\ncommand = \"boxd\"\nargs = [\"mcp\"]\n")
	return os.WriteFile(path, []byte(config.String()), 0600)
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
