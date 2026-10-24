package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Config is the persisted per-user server connection.
type Config struct {
	Server  string `json:"server"`
	Token   string `json:"token"`
	Current string `json:"current,omitempty"`
}

//go:embed skill.md
var embeddedSkill []byte

// CLI contains the interactive and external dependencies of the boxes command.
// HTTPClient and command/browser hooks are injectable so the CLI can be tested
// against httptest servers without a running boxes server.
type CLI struct {
	In                  io.Reader
	Out                 io.Writer
	Err                 io.Writer
	ConfigPath          string
	HTTPClient          *http.Client
	BaseURL             string
	Token               string
	OpenBrowser         func(context.Context, string) error
	RunCommand          func(context.Context, string, ...string) error
	RunCommandWithInput func(context.Context, io.Reader, string, ...string) error
	Interactive         bool

	jsonOutput bool
	client     *Client
	config     Config
}

// Run executes one boxes command.
func (c *CLI) Run(ctx context.Context, args []string) error {
	c.defaults()
	c.jsonOutput = false
	if len(args) == 0 {
		return errors.New("usage: boxes [--json] <command> [arguments]")
	}
	if args[0] == "--json" {
		c.jsonOutput = true
		args = args[1:]
	}
	if len(args) == 0 {
		return errors.New("usage: boxes [--json] <command> [arguments]")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		c.printUsage()
		return nil
	}
	if args[0] == "skill" {
		return c.skill(ctx, args[1:])
	}
	if args[0] == "current" {
		if err := c.loadConfig(); err != nil {
			return err
		}
		return c.current(args[1:])
	}
	if args[0] == "logout" {
		if err := c.loadConfig(); err != nil {
			return err
		}
		return c.logout(args[1:])
	}
	if args[0] == "login" {
		if err := c.loadConfig(); err != nil {
			return err
		}
		return c.login(ctx, args[1:])
	}
	if err := c.loadConfig(); err != nil {
		return err
	}
	baseURL, token := c.BaseURL, c.Token
	if baseURL == "" {
		return errors.New("server URL is not configured; run boxes login --server URL --api-key KEY")
	}
	c.client = NewClient(baseURL, token, c.HTTPClient)
	switch args[0] {
	case "dashboard":
		return c.dashboard(ctx, args[1:])
	case "forward":
		return c.forward(ctx, args[1:])
	case "whoami":
		return c.whoami(ctx, args[1:])
	case "new":
		return c.newSandbox(ctx, args[1:])
	case "list":
		return c.list(ctx, args[1:])
	case "get":
		return c.get(ctx, args[1:])
	case "info":
		return c.info(ctx, args[1:])
	case "status":
		return c.status(ctx, args[1:])
	case "exec":
		return c.exec(ctx, args[1:])
	case "ps":
		return c.ps(ctx, args[1:])
	case "logs":
		return c.logs(ctx, args[1:])
	case "kill":
		return c.kill(ctx, args[1:])
	case "ssh":
		return c.ssh(ctx, args[1:])
	case "proxy":
		return c.proxy(ctx, args[1:])
	case "scp":
		return c.scp(ctx, args[1:])
	case "push":
		return c.push(ctx, args[1:])
	case "pull":
		return c.pull(ctx, args[1:])
	case "prompt":
		return c.prompt(ctx, args[1:])
	case "events":
		return c.events(ctx, args[1:])
	case "steer":
		return c.steer(ctx, args[1:])
	case "interrupt":
		return c.interrupt(ctx, args[1:])
	case "desktop":
		return c.desktop(ctx, args[1:], false)
	case "browser":
		return c.desktop(ctx, args[1:], true)
	case "host":
		return c.host(ctx, args[1:])
	case "stop":
		return c.stop(ctx, args[1:])
	case "resume":
		return c.resume(ctx, args[1:])
	case "fork":
		return c.fork(ctx, args[1:])
	case "snapshot":
		return c.snapshot(ctx, args[1:])
	case "delete":
		return c.delete(ctx, args[1:])
	case "extend":
		return c.extend(ctx, args[1:])
	case "update":
		return c.update(ctx, args[1:])
	case "restore":
		return c.restore(ctx, args[1:])
	case "conversations":
		return c.boxSubresource(ctx, args[1:], "conversations")
	case "snapshots":
		if len(args) > 1 && args[1] == "rm" {
			return c.deleteSnapshot(ctx, args[2:])
		}
		return c.boxSubresource(ctx, args[1:], "snapshots")
	case "usage":
		return c.boxSubresource(ctx, args[1:], "usage")
	case "limits":
		return c.limits(ctx, args[1:])
	case "env":
		return c.environment(ctx, args[1:])
	default:
		return fmt.Errorf("unknown command %q; run boxes --help", args[0])
	}
}

func (c *CLI) defaults() {
	if c.In == nil {
		c.In = os.Stdin
	}
	if c.Out == nil {
		c.Out = os.Stdout
	}
	if c.Err == nil {
		c.Err = os.Stderr
	}
	if c.HTTPClient == nil {
		c.HTTPClient = http.DefaultClient
	}
	if c.ConfigPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		c.ConfigPath = filepath.Join(home, ".config", "boxes", "config.json")
	}
	if c.OpenBrowser == nil {
		c.OpenBrowser = func(ctx context.Context, target string) error {
			program := "xdg-open"
			if runtime.GOOS == "darwin" {
				program = "open"
			} else if runtime.GOOS == "windows" {
				program = "rundll32"
				return c.run(ctx, program, "url.dll,FileProtocolHandler", target)
			}
			return c.run(ctx, program, target)
		}
	}
	if c.RunCommand == nil {
		c.RunCommand = func(ctx context.Context, name string, args ...string) error {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = c.In, c.Out, c.Err
			return cmd.Run()
		}
	}
	if c.RunCommandWithInput == nil {
		c.RunCommandWithInput = func(ctx context.Context, input io.Reader, name string, args ...string) error {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = input, c.Out, c.Err
			return cmd.Run()
		}
	}
}

func (c *CLI) run(ctx context.Context, name string, args ...string) error {
	return c.RunCommand(ctx, name, args...)
}

func (c *CLI) loadConfig() error {
	data, err := os.ReadFile(c.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read config: %w", err)
	}
	var config Config
	if len(data) != 0 {
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("decode config: %w", err)
		}
	}
	c.config = config
	if c.BaseURL == "" {
		c.BaseURL = config.Server
	}
	if c.Token == "" {
		c.Token = config.Token
	}
	if server := os.Getenv("BOXES_SERVER"); server != "" {
		c.BaseURL = server
	}
	if token := os.Getenv("BOXES_TOKEN"); token != "" {
		c.Token = token
	} else if apiKey := os.Getenv("BOXES_API_KEY"); apiKey != "" {
		c.Token = apiKey
	}
	return nil
}

func (c *CLI) saveConfig(config Config) error {
	if err := os.MkdirAll(filepath.Dir(c.ConfigPath), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.ConfigPath), ".config-*")
	if err != nil {
		return fmt.Errorf("create config file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure config file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmpName, c.ConfigPath); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	c.config = config
	return nil
}

func (c *CLI) login(ctx context.Context, args []string) error {
	fs := c.flagSet("login")
	defaultServer := c.BaseURL
	if envServer := os.Getenv("BOXES_SERVER"); envServer != "" {
		defaultServer = envServer
	}
	server := fs.String("server", defaultServer, "boxes server URL")
	apiKey := fs.String("api-key", "", "API key")
	email := fs.String("email", "", "account email")
	password := fs.String("password", "", "account password (prompted if omitted)")
	keyStdin := fs.Bool("key-stdin", false, "read API key from stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *server == "" || fs.NArg() != 0 {
		return errors.New("usage: boxes login --server URL --api-key KEY")
	}
	key := *apiKey
	if *keyStdin {
		if key != "" {
			return errors.New("--key-stdin cannot be combined with --api-key")
		}
		line, err := bufio.NewReader(c.In).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read API key: %w", err)
		}
		key = strings.TrimSpace(line)
	}
	if key == "" {
		key = os.Getenv("BOXES_API_KEY")
	}
	if key == "" {
		key = os.Getenv("BOXES_TOKEN")
	}
	if key != "" {
		client := NewClient(*server, key, c.HTTPClient)
		var response struct {
			User struct {
				Email string `json:"email"`
			} `json:"user"`
		}
		if err := client.request(ctx, http.MethodGet, "/me", nil, &response, nil); err != nil {
			return err
		}
		if response.User.Email == "" {
			return errors.New("whoami response did not contain a user email")
		}
		serverURL := strings.TrimRight(*server, "/")
		if err := c.saveConfig(Config{Server: serverURL, Token: key, Current: c.config.Current}); err != nil {
			return err
		}
		if c.jsonOutput {
			return c.printJSON(map[string]string{"server": serverURL, "email": response.User.Email})
		}
		_, err := fmt.Fprintf(c.Out, "Logged in to %s as %s\n", serverURL, response.User.Email)
		return err
	}
	if *email == "" {
		return errors.New("usage: boxes login --server URL --api-key KEY")
	}
	secret := *password
	if secret == "" {
		secret = os.Getenv("BOXES_PASSWORD")
	}
	if secret == "" {
		if _, err := fmt.Fprint(c.Err, "Password: "); err != nil {
			return err
		}
		line, err := bufio.NewReader(c.In).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read password: %w", err)
		}
		secret = strings.TrimRight(line, "\r\n")
	}
	client := NewClient(*server, "", c.HTTPClient)
	var response map[string]json.RawMessage
	if err := client.request(ctx, http.MethodPost, "/auth/login", map[string]string{"email": *email, "password": secret}, &response, nil); err != nil {
		return err
	}
	token := rawString(response["token"])
	if token == "" {
		token = rawString(response["sessionToken"])
	}
	if token == "" {
		return errors.New("login response did not contain a session token")
	}
	if err := c.saveConfig(Config{Server: strings.TrimRight(*server, "/"), Token: token, Current: c.config.Current}); err != nil {
		return err
	}
	if c.jsonOutput {
		return c.printJSON(map[string]string{"server": strings.TrimRight(*server, "/"), "email": *email})
	}
	_, err := fmt.Fprintf(c.Out, "Logged in to %s as %s\n", strings.TrimRight(*server, "/"), *email)
	return err
}

func (c *CLI) whoami(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: boxes whoami")
	}
	var response struct {
		User struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := c.client.request(ctx, http.MethodGet, "/me", nil, &response, nil); err != nil {
		return err
	}
	if response.User.Email == "" {
		return errors.New("whoami response did not contain a user email")
	}
	if c.jsonOutput {
		return c.printJSON(map[string]string{"server": c.client.BaseURL, "email": response.User.Email})
	}
	_, err := fmt.Fprintf(c.Out, "%s\n%s\n", c.client.BaseURL, response.User.Email)
	return err
}

func (c *CLI) current(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: boxes current")
	}
	if c.config.Current == "" {
		return errors.New("no current box; create or use a box first")
	}
	if c.jsonOutput {
		return c.printJSON(map[string]string{"id": c.config.Current})
	}
	_, err := fmt.Fprintln(c.Out, c.config.Current)
	return err
}

func (c *CLI) logout(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: boxes logout")
	}
	c.config.Server, c.config.Token, c.config.Current = "", "", ""
	if err := c.saveConfig(c.config); err != nil {
		return err
	}
	if c.jsonOutput {
		return c.printJSON(map[string]bool{"ok": true})
	}
	_, err := fmt.Fprintln(c.Out, "Logged out")
	return err
}

func (c *CLI) skill(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "install" {
		return errors.New("usage: boxes skill install [--claude] [--codex] [--force]")
	}
	fs := c.flagSet("skill install")
	claude := fs.Bool("claude", false, "install for Claude Code")
	codex := fs.Bool("codex", false, "install for Codex")
	force := fs.Bool("force", false, "overwrite existing skill files")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: boxes skill install [--claude] [--codex] [--force]")
	}
	if !*claude && !*codex {
		*claude, *codex = true, true
	}
	content := managedSkillContent()
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var targets []string
	if *claude {
		targets = append(targets, filepath.Join(home, ".claude", "skills", "boxes", "SKILL.md"))
	}
	if *codex {
		targets = append(targets, filepath.Join(home, ".codex", "skills", "boxes", "SKILL.md"))
	}
	for _, target := range targets {
		if existing, readErr := os.ReadFile(target); readErr == nil {
			if !bytes.Equal(existing, content) && !managedSkillIsUnmodified(existing) && !*force {
				return fmt.Errorf("refusing to overwrite modified skill %s; use --force", target)
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return fmt.Errorf("read skill %s: %w", target, readErr)
		}
	}
	for _, target := range targets {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create skill directory: %w", err)
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			return fmt.Errorf("write skill %s: %w", target, err)
		}
	}
	if c.jsonOutput {
		return c.printJSON(map[string]any{"installed": targets})
	}
	for _, target := range targets {
		if _, err := fmt.Fprintln(c.Out, target); err != nil {
			return err
		}
	}
	return nil
}

func (c *CLI) dashboard(ctx context.Context, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: boxes dashboard [id|current]")
	}
	base, err := url.Parse(c.client.BaseURL)
	if err != nil {
		return err
	}
	target := strings.TrimRight(base.String(), "/") + "/"
	if len(args) == 1 {
		id, err := c.boxID(args[0])
		if err != nil {
			return err
		}
		target = strings.TrimRight(base.String(), "/") + "/sandbox/?id=" + url.QueryEscape(id)
	}
	if c.jsonOutput {
		if err := c.printJSON(map[string]string{"url": target}); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(c.Out, target); err != nil {
		return err
	}
	return c.OpenBrowser(ctx, target)
}

func (c *CLI) forward(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: boxes forward <id|current> <port> [--local-port N]")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	remotePort, err := strconv.Atoi(args[1])
	if err != nil || remotePort < 1 || remotePort > 65535 {
		return fmt.Errorf("invalid remote port %q", args[1])
	}
	fs := c.flagSet("forward")
	localPort := fs.Int("local-port", remotePort, "local TCP port")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *localPort < 0 || *localPort > 65535 {
		return errors.New("invalid local port; usage: boxes forward <id|current> <port> [--local-port N]")
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", *localPort))
	if err != nil {
		return fmt.Errorf("listen for forwarded port: %w", err)
	}
	defer listener.Close()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	if _, err := fmt.Fprintf(c.Out, "Forwarding %s to %s:%d\n", listener.Addr(), id, remotePort); err != nil {
		return err
	}
	for {
		local, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept forwarded connection: %w", err)
		}
		go c.bridgeForward(ctx, local, id, remotePort)
	}
}

func (c *CLI) bridgeForward(ctx context.Context, local net.Conn, id string, port int) {
	defer local.Close()
	u, err := url.Parse(c.client.BaseURL)
	if err != nil {
		return
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/sandboxes/" + url.PathEscape(id) + "/tcp/" + strconv.Itoa(port)
	header := http.Header{}
	if c.client.Token != "" {
		header.Set("Authorization", "Bearer "+c.client.Token)
	}
	ws, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: c.HTTPClient, HTTPHeader: header})
	if err != nil {
		_, _ = fmt.Fprintf(c.Err, "forward connection: %v\n", err)
		return
	}
	defer ws.Close(websocket.StatusNormalClosure, "forward closed")
	remote := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer remote.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(remote, local)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(local, remote)
		done <- struct{}{}
	}()
	<-done
}

func (c *CLI) newSandbox(ctx context.Context, args []string) error {
	fs := c.flagSet("new")
	boxType := fs.String("type", "default", "box size")
	image := fs.String("image", "", "container image for the box (must be built FROM ghcr.io/tony1908/boxes-box)")
	ttl := fs.Int64("ttl", 3600, "time to live in seconds")
	idleStop := fs.String("idle-stop", "", "stop after inactivity (Go duration, e.g. 30m; off disables)")
	noAutoStop := fs.Bool("no-auto-stop", false, "disable the auto-stop timer")
	var envVars stringList
	fs.Var(&envVars, "e", "environment variable KEY=VALUE (repeatable)")
	fs.Var(&envVars, "env", "environment variable KEY=VALUE (repeatable; legacy environment ID also accepted)")
	environment := fs.String("environment", "", "environment ID")
	noEnv := fs.Bool("no-env", false, "disable the default environment")
	from := fs.String("from", "", "named snapshot to restore")
	setupFile := fs.String("setup-file", "", "setup script file")
	gitURL := fs.String("git", "", "clone this https git repository into the sandbox on first boot")
	revision := fs.String("revision", "", "git branch, tag, or commit SHA to check out (with --git)")
	depth := fs.Int("depth", 0, "shallow clone depth, 1 to 1000 (with --git)")
	gitTokenEnv := fs.String("git-token-env", "", "environment variable holding a git token for a private repository (with --git)")
	sourcePath := fs.String("path", "", "directory under /config for the source (default project)")
	tarball := fs.String("tarball", "", "https URL of a .tar.gz to extract into the sandbox on first boot")
	var tagFlags stringList
	fs.Var(&tagFlags, "tag", "tag KEY=VALUE (repeatable)")
	keepSnapshots := fs.Int("keep-snapshots", 0, "snapshots to retain, 1 to 50 (default 5)")
	var network networkFlags
	network.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: boxes new [--type SIZE] [--image REF] [--ttl SECONDS] [--no-auto-stop] [-e KEY=VALUE]... [--setup-file FILE] [--git URL [--revision X] [--depth N] [--git-token-env VAR] [--path P] | --tarball URL [--path P]] [--from NAME] [--environment NAME] [--no-env] [--tag KEY=VALUE]... [--keep-snapshots N] [--network MODE] [--allow-domain HOST]... [--allow-cidr CIDR]... [--deny-cidr CIDR]...")
	}
	if *noAutoStop && flagWasSet(fs, "ttl") {
		return errors.New("--ttl and --no-auto-stop are mutually exclusive")
	}
	body := map[string]any{"type": *boxType}
	if *image != "" {
		body["image"] = *image
	}
	if flagWasSet(fs, "keep-snapshots") {
		body["keepSnapshots"] = *keepSnapshots
	}
	if flagWasSet(fs, "idle-stop") {
		seconds, err := parseIdleStopFlag(*idleStop)
		if err != nil {
			return err
		}
		body["idleStopSeconds"] = seconds
	}
	if *noAutoStop {
		body["ttlSeconds"] = nil
	} else {
		body["ttlSeconds"] = *ttl
	}
	env := make(map[string]string)
	for _, entry := range envVars {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			if *environment == "" {
				*environment = entry // Preserve the previous --env ID spelling.
				continue
			}
			return fmt.Errorf("environment variable %q must be KEY=VALUE", entry)
		}
		if key == "" {
			return errors.New("environment variable name cannot be empty")
		}
		env[key] = value
	}
	if len(env) != 0 {
		body["env"] = env
	}
	if *environment != "" {
		body["environment"] = *environment
	}
	if *noEnv {
		body["noEnv"] = true
	}
	if *from != "" {
		body["from"] = *from
	}
	source, err := sourceBody(*gitURL, *revision, *depth, *gitTokenEnv, *sourcePath, *tarball)
	if err != nil {
		return err
	}
	if source != nil {
		body["source"] = source
	}
	tags, err := parseTagFlags(tagFlags)
	if err != nil {
		return err
	}
	if len(tags) != 0 {
		body["tags"] = tags
	}
	policy, err := network.policy()
	if err != nil {
		return err
	}
	if policy != nil {
		body["networkPolicy"] = policy
	}
	if *setupFile != "" {
		script, err := os.ReadFile(*setupFile)
		if err != nil {
			return fmt.Errorf("read setup file: %w", err)
		}
		body["setupScript"] = string(script)
	}
	key, err := idempotencyKey()
	if err != nil {
		return err
	}
	headers := make(http.Header)
	headers.Set("Idempotency-Key", key)
	result, err := c.client.doJSON(ctx, http.MethodPost, "/sandboxes", body, headers)
	if err != nil {
		return err
	}
	box := unwrap(result, "sandbox")
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(box, &created); err != nil {
		return fmt.Errorf("decode created sandbox: %w", err)
	}
	if created.ID != "" {
		if err := c.setCurrent(created.ID); err != nil {
			return err
		}
	}
	return c.printValue(box)
}

// sourceBody builds the create-request source from --git or --tarball flags. The git token is read
// from the environment variable named by tokenEnv, never from a flag value. It returns nil when no source is requested.
func sourceBody(gitURL, revision string, depth int, tokenEnv, path, tarball string) (map[string]any, error) {
	if gitURL != "" && tarball != "" {
		return nil, errors.New("--git and --tarball are mutually exclusive")
	}
	if gitURL == "" && tarball == "" {
		if revision != "" || depth != 0 || tokenEnv != "" {
			return nil, errors.New("--revision, --depth, and --git-token-env require --git")
		}
		if path != "" {
			return nil, errors.New("--path requires --git or --tarball")
		}
		return nil, nil
	}
	if tarball != "" {
		if revision != "" || depth != 0 || tokenEnv != "" {
			return nil, errors.New("--revision, --depth, and --git-token-env are not valid with --tarball")
		}
		source := map[string]any{"type": "tarball", "url": tarball}
		if path != "" {
			source["path"] = path
		}
		return source, nil
	}
	source := map[string]any{"type": "git", "url": gitURL}
	if revision != "" {
		source["revision"] = revision
	}
	if depth != 0 {
		source["depth"] = depth
	}
	if path != "" {
		source["path"] = path
	}
	if tokenEnv != "" {
		token := os.Getenv(tokenEnv)
		if token == "" {
			return nil, fmt.Errorf("environment variable %s is empty or unset", tokenEnv)
		}
		source["token"] = token
	}
	return source, nil
}

func (c *CLI) list(ctx context.Context, args []string) error {
	fs := c.flagSet("list")
	jsonOutput := fs.Bool("json", false, "print JSON output")
	filter := fs.String("filter", "r", "state filter letters: r (running), s (stopped), e (error)")
	all := fs.Bool("all", false, "show all boxes")
	var tagFilters stringList
	fs.Var(&tagFilters, "tag", "only boxes with tag KEY=VALUE, or KEY alone for any value (repeatable; all must match)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: boxes list [--filter r|s|e...] [--all] [--tag KEY=VALUE]... [--json]")
	}
	c.jsonOutput = c.jsonOutput || *jsonOutput
	query := url.Values{}
	for _, tag := range tagFilters {
		query.Add("tag", tag)
	}
	if !*all {
		if *filter == "" || strings.Trim(*filter, "rse") != "" {
			return errors.New("--filter must contain only r, s, and e")
		}
		query.Set("filter", *filter)
	}
	endpoint := "/sandboxes"
	if len(query) != 0 {
		endpoint += "?" + query.Encode()
	}
	result, err := c.client.doJSON(ctx, http.MethodGet, endpoint, nil, nil)
	if err != nil {
		return err
	}
	value := unwrap(result, "sandboxes")
	if c.jsonOutput {
		return c.printValue(value)
	}
	var boxes []map[string]any
	if err := json.Unmarshal(value, &boxes); err != nil {
		return c.printValue(value)
	}
	for _, box := range boxes {
		_, err := fmt.Fprintf(c.Out, "%s\t%s\t%s\n", box["id"], box["state"], box["type"])
		if err != nil {
			return err
		}
	}
	return nil
}

// update changes sandbox tags (--tag merges into the existing tags, --clear-tags removes them all)
// and the outbound network policy (--network and friends).
func (c *CLI) update(ctx context.Context, args []string) error {
	usage := errors.New("usage: boxes update <id|current> [--tag KEY=VALUE]... [--clear-tags] [--keep-snapshots N] [--network MODE] [--allow-domain HOST]... [--allow-cidr CIDR]... [--deny-cidr CIDR]...")
	if len(args) == 0 {
		return usage
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	fs := c.flagSet("update")
	var tagFlags stringList
	fs.Var(&tagFlags, "tag", "tag KEY=VALUE to set, merged into existing tags (repeatable)")
	clearTags := fs.Bool("clear-tags", false, "remove all tags")
	keepSnapshots := fs.Int("keep-snapshots", 0, "snapshots to retain, 1 to 50 (0 restores the default of 5)")
	var network networkFlags
	network.register(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return usage
	}
	if *clearTags && len(tagFlags) != 0 {
		return errors.New("--tag and --clear-tags are mutually exclusive")
	}
	policy, err := network.policy()
	if err != nil {
		return err
	}
	keepSet := flagWasSet(fs, "keep-snapshots")
	if !*clearTags && len(tagFlags) == 0 && policy == nil && !keepSet {
		return errors.New("choose --tag KEY=VALUE, --clear-tags, --keep-snapshots N, or a network flag (--network, --allow-domain, --allow-cidr, --deny-cidr)")
	}
	body := map[string]any{}
	if policy != nil {
		body["networkPolicy"] = policy
	}
	if keepSet {
		body["keepSnapshots"] = *keepSnapshots
	}
	if *clearTags || len(tagFlags) != 0 {
		tags := map[string]string{}
		if !*clearTags {
			parsed, err := parseTagFlags(tagFlags)
			if err != nil {
				return err
			}
			current, err := c.client.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, nil)
			if err != nil {
				return err
			}
			var box struct {
				Tags map[string]string `json:"tags"`
			}
			if err := json.Unmarshal(unwrap(current, "sandbox"), &box); err != nil {
				return fmt.Errorf("decode sandbox tags: %w", err)
			}
			for key, value := range box.Tags {
				tags[key] = value
			}
			for key, value := range parsed {
				tags[key] = value
			}
		}
		body["tags"] = tags
	}
	result, err := c.client.doJSON(ctx, http.MethodPatch, "/sandboxes/"+url.PathEscape(id), body, nil)
	if err != nil {
		return err
	}
	return c.printValue(unwrap(result, "sandbox"))
}

// restore rolls a box back to one of its own snapshots; a running box is stopped and resumed from it.
func (c *CLI) restore(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: boxes restore <id|current> <snapshotId>")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	result, err := c.client.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/restore", map[string]any{"snapshotId": args[1]}, nil)
	if err != nil {
		return err
	}
	return c.printValue(unwrap(result, "sandbox"))
}

// networkFlags are the outbound network policy flags shared by new and update.
type networkFlags struct {
	mode         string
	allowDomains stringList
	allowCIDRs   stringList
	denyCIDRs    stringList
}

func (n *networkFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&n.mode, "network", "", "outbound network mode: allow-all, deny-all, or allowlist")
	fs.Var(&n.allowDomains, "allow-domain", "domain or *.example.com allowed when --network allowlist (repeatable)")
	fs.Var(&n.allowCIDRs, "allow-cidr", "IPv4 CIDR allowed when --network allowlist (repeatable)")
	fs.Var(&n.denyCIDRs, "deny-cidr", "IPv4 CIDR always blocked, in any mode (repeatable)")
}

// policy returns the networkPolicy request body, or nil when no network flag was given.
func (n networkFlags) policy() (map[string]any, error) {
	if n.mode == "" {
		if len(n.allowDomains) != 0 || len(n.allowCIDRs) != 0 {
			return nil, errors.New("--allow-domain and --allow-cidr require --network allowlist")
		}
		if len(n.denyCIDRs) == 0 {
			return nil, nil
		}
		return nil, errors.New("--deny-cidr requires --network (allow-all, deny-all, or allowlist)")
	}
	switch n.mode {
	case "allow-all", "deny-all", "allowlist":
	default:
		return nil, fmt.Errorf("--network must be allow-all, deny-all, or allowlist, not %q", n.mode)
	}
	if n.mode != "allowlist" && (len(n.allowDomains) != 0 || len(n.allowCIDRs) != 0) {
		return nil, errors.New("--allow-domain and --allow-cidr require --network allowlist")
	}
	policy := map[string]any{"mode": n.mode}
	if len(n.allowDomains) != 0 {
		policy["allowDomains"] = []string(n.allowDomains)
	}
	if len(n.allowCIDRs) != 0 {
		policy["allowCidrs"] = []string(n.allowCIDRs)
	}
	if len(n.denyCIDRs) != 0 {
		policy["denyCidrs"] = []string(n.denyCIDRs)
	}
	return policy, nil
}

// parseTagFlags turns repeated KEY=VALUE flags into a tag map.
func parseTagFlags(values []string) (map[string]string, error) {
	tags := make(map[string]string, len(values))
	for _, entry := range values {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("tag %q must be KEY=VALUE", entry)
		}
		tags[key] = value
	}
	return tags, nil
}

func (c *CLI) get(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: boxes get <id>")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	result, err := c.client.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return err
	}
	return c.printValue(unwrap(result, "sandbox"))
}

func (c *CLI) info(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: boxes info <id|current>")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	result, err := c.client.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return err
	}
	value := unwrap(result, "sandbox")
	if c.jsonOutput {
		return c.printValue(value)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, value, "", "  "); err != nil {
		return fmt.Errorf("format sandbox info: %w", err)
	}
	_, err = fmt.Fprintln(c.Out, pretty.String())
	return err
}

func (c *CLI) exec(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: boxes exec <id|current> [--detach] [--status PID] -- <command> [arguments]")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	fs := c.flagSet("exec")
	detach := fs.Bool("detach", false, "run command in the background")
	statusPID := fs.String("status", "", "show detached process status")
	remaining := args[1:]
	var command []string
	if separator := indexOf(remaining, "--"); separator >= 0 {
		if err := fs.Parse(flagsBeforePositionals(fs, remaining[:separator])); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("exec options must appear before --")
		}
		command = remaining[separator+1:]
	} else if len(remaining) > 0 && (remaining[0] == "--detach" || remaining[0] == "--status" || strings.HasPrefix(remaining[0], "--status=")) {
		if err := fs.Parse(remaining); err != nil {
			return err
		}
		command = fs.Args()
	} else {
		command = remaining
	}
	if *statusPID != "" {
		if len(command) != 0 {
			return errors.New("exec --status cannot be combined with a command")
		}
		result, err := c.client.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id)+"/commands/"+url.PathEscape(*statusPID), nil, nil)
		if err != nil {
			return err
		}
		return c.printValue(result)
	}
	if len(command) == 0 {
		return errors.New("exec command is required")
	}
	var value any
	if len(command) == 1 {
		value = map[string]any{"cmd": command[0]}
	} else {
		value = map[string]any{"cmd": command}
	}
	if *detach {
		value.(map[string]any)["detached"] = true
	}
	result, err := c.client.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/commands", value, nil)
	if err != nil {
		return err
	}
	var commandResult struct {
		ExitCode  int    `json:"exitCode"`
		Stdout    string `json:"stdout"`
		Stderr    string `json:"stderr"`
		ProcessID string `json:"processId"`
	}
	if err := json.Unmarshal(result, &commandResult); err != nil {
		return fmt.Errorf("decode command result: %w", err)
	}
	if c.jsonOutput {
		return c.printValue(result)
	}
	if *detach {
		if commandResult.ProcessID != "" {
			_, err := fmt.Fprintln(c.Out, commandResult.ProcessID)
			return err
		}
		return c.printValue(result)
	}
	if commandResult.Stdout != "" {
		if _, err := io.WriteString(c.Out, commandResult.Stdout); err != nil {
			return err
		}
	}
	if commandResult.Stderr != "" {
		if _, err := io.WriteString(c.Err, commandResult.Stderr); err != nil {
			return err
		}
	}
	if commandResult.ExitCode != 0 {
		return fmt.Errorf("remote command exited with status %d", commandResult.ExitCode)
	}
	return nil
}

// ExitError reports a remote command's exit status. The boxes binary exits with Code.
type ExitError struct{ Code int }

func (e *ExitError) Error() string {
	return fmt.Sprintf("remote command exited with status %d", e.Code)
}

// ps lists the detached commands the agent still remembers for a box.
func (c *CLI) ps(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: boxes ps <id|current>")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	result, err := c.client.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id)+"/commands", nil, nil)
	if err != nil {
		return err
	}
	if c.jsonOutput {
		return c.printValue(result)
	}
	var listing struct {
		Commands []struct {
			ProcessID string    `json:"processId"`
			Cmd       string    `json:"cmd"`
			Running   bool      `json:"running"`
			ExitCode  *int      `json:"exitCode"`
			StartedAt time.Time `json:"startedAt"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(result, &listing); err != nil {
		return fmt.Errorf("decode command list: %w", err)
	}
	for _, command := range listing.Commands {
		state := "running"
		if !command.Running && command.ExitCode != nil {
			state = "exited " + strconv.Itoa(*command.ExitCode)
		} else if !command.Running {
			state = "exited"
		}
		if _, err := fmt.Fprintf(c.Out, "%s\t%s\t%s\t%s\n", command.ProcessID, state, command.StartedAt.Format(time.RFC3339), command.Cmd); err != nil {
			return err
		}
	}
	return nil
}

// logs prints a detached command's output. With -f it follows until the command exits and returns its status.
func (c *CLI) logs(ctx context.Context, args []string) error {
	usage := errors.New("usage: boxes logs <id|current> <processId> [-f]")
	if len(args) < 2 {
		return usage
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	fs := c.flagSet("logs")
	follow := fs.Bool("f", false, "follow output until the command exits")
	if err := fs.Parse(flagsBeforePositionals(fs, args[2:])); err != nil || fs.NArg() != 0 {
		return usage
	}
	endpoint := "/sandboxes/" + url.PathEscape(id) + "/commands/" + url.PathEscape(args[1]) + "/logs"
	if *follow {
		endpoint += "?follow=1"
	}
	lines := &logLines{out: c.Out, errOut: c.Err}
	if err := c.client.Download(ctx, endpoint, lines, nil); err != nil {
		return err
	}
	if lines.exited && lines.exitCode != 0 {
		return &ExitError{Code: lines.exitCode}
	}
	return nil
}

// logLines decodes NDJSON log records as they arrive and writes their data to the matching stream.
type logLines struct {
	out, errOut io.Writer
	pending     []byte
	exited      bool
	exitCode    int
}

func (l *logLines) Write(p []byte) (int, error) {
	l.pending = append(l.pending, p...)
	for {
		end := bytes.IndexByte(l.pending, '\n')
		if end < 0 {
			return len(p), nil
		}
		line := l.pending[:end]
		l.pending = l.pending[end+1:]
		if err := l.handle(line); err != nil {
			return 0, err
		}
	}
}

func (l *logLines) handle(line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	var record struct {
		Stream   string `json:"stream"`
		Data     string `json:"data"`
		Exit     bool   `json:"exit"`
		ExitCode int    `json:"exitCode"`
	}
	if err := json.Unmarshal(line, &record); err != nil {
		return fmt.Errorf("decode log line: %w", err)
	}
	if record.Exit {
		l.exited, l.exitCode = true, record.ExitCode
		return nil
	}
	out := l.out
	if record.Stream == "stderr" {
		out = l.errOut
	}
	_, err := io.WriteString(out, record.Data)
	return err
}

// kill sends a signal to a detached command's process group.
func (c *CLI) kill(ctx context.Context, args []string) error {
	usage := errors.New("usage: boxes kill <id|current> <processId> [--signal TERM]")
	if len(args) < 2 {
		return usage
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	fs := c.flagSet("kill")
	signal := fs.String("signal", "TERM", "signal to send: TERM, KILL, INT, HUP, QUIT, USR1, USR2")
	if err := fs.Parse(flagsBeforePositionals(fs, args[2:])); err != nil || fs.NArg() != 0 {
		return usage
	}
	endpoint := "/sandboxes/" + url.PathEscape(id) + "/commands/" + url.PathEscape(args[1]) + "/kill"
	result, err := c.client.doJSON(ctx, http.MethodPost, endpoint, map[string]any{"signal": *signal}, nil)
	if err != nil {
		return err
	}
	return c.printValue(result)
}

func (c *CLI) ssh(ctx context.Context, args []string) error {
	if len(args) < 1 || (len(args) > 1 && args[1] != "--") {
		return errors.New("usage: boxes ssh <id> [-- command...]")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	if err := c.ensureSSHKey(ctx, id); err != nil {
		return err
	}
	sshArgs := []string{"-o", "ProxyCommand=boxes proxy " + id}
	hostKeyOptions, err := c.hostKeyOptions(id)
	if err != nil {
		return err
	}
	sshArgs = append(sshArgs, hostKeyOptions...)
	sshArgs = append(sshArgs, "abc@"+id)
	if len(args) > 2 {
		sshArgs = append(sshArgs, args[2:]...)
	}
	return c.run(ctx, externalCommand("ssh", runtime.GOOS), sshArgs...)
}

func (c *CLI) ensureSSHKey(ctx context.Context, id string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var publicKey string
	for _, name := range []string{"id_ed25519.pub", "id_rsa.pub"} {
		data, readErr := os.ReadFile(filepath.Join(home, ".ssh", name))
		if readErr == nil {
			publicKey = strings.TrimSpace(string(data))
			break
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return fmt.Errorf("read SSH public key %s: %w", name, readErr)
		}
	}
	if publicKey == "" {
		return errors.New("no ~/.ssh/id_ed25519.pub or ~/.ssh/id_rsa.pub found")
	}
	if _, err := c.client.Do(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/ssh-key", map[string]string{"publicKey": publicKey}, nil); err != nil {
		return err
	}
	return nil
}

func (c *CLI) hostKeyOptions(id string) ([]string, error) {
	configDir := filepath.Dir(c.ConfigPath)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("create CLI config directory: %w", err)
	}
	knownHosts := filepath.Join(configDir, "known_hosts")
	file, err := os.OpenFile(knownHosts, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open SSH known_hosts file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("secure SSH known_hosts file: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close SSH known_hosts file: %w", err)
	}
	options := []string{
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "StrictHostKeyChecking=accept-new",
	}
	if id != "" {
		options = append(options, "-o", "HostKeyAlias="+id)
	}
	return options, nil
}

func (c *CLI) proxy(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: boxes proxy <id>")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	u, err := url.Parse(c.client.BaseURL)
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1/sandboxes/" + url.PathEscape(id) + "/tcp/22"
	header := http.Header{}
	if c.client.Token != "" {
		header.Set("Authorization", "Bearer "+c.client.Token)
	}
	ws, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: c.HTTPClient, HTTPHeader: header})
	if err != nil {
		return fmt.Errorf("connect TCP proxy: %w", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "proxy closed")
	done := make(chan error, 1)
	go func() {
		buffer := make([]byte, 32*1024)
		for {
			n, readErr := c.In.Read(buffer)
			if n > 0 {
				if writeErr := ws.Write(ctx, websocket.MessageBinary, buffer[:n]); writeErr != nil {
					done <- writeErr
					return
				}
			}
			if readErr != nil {
				done <- readErr
				return
			}
		}
	}()
	for {
		typ, reader, readErr := ws.Reader(ctx)
		if readErr != nil {
			select {
			case inputErr := <-done:
				if !errors.Is(inputErr, io.EOF) {
					return inputErr
				}
			default:
			}
			if websocket.CloseStatus(readErr) == websocket.StatusNormalClosure {
				return nil
			}
			return readErr
		}
		if _, err := io.Copy(c.Out, reader); err != nil {
			return err
		}
		if typ != websocket.MessageBinary && typ != websocket.MessageText {
			return fmt.Errorf("unexpected websocket message type %d", typ)
		}
	}
}

func (c *CLI) scp(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: boxes scp <source> <destination> [arguments]")
	}
	commandArgs := []string{"-o", "ProxyCommand=boxes proxy %h"}
	boxID := ""
	for _, arg := range args {
		if colon := strings.IndexByte(arg, ':'); colon > 0 && !strings.Contains(arg[:colon], "/") {
			if id, err := c.boxID(arg[:colon]); err == nil {
				boxID = id
				break
			}
		}
	}
	hostKeyOptions, err := c.hostKeyOptions(boxID)
	if err != nil {
		return err
	}
	if boxID != "" {
		if err := c.ensureSSHKey(ctx, boxID); err != nil {
			return err
		}
	}
	commandArgs = append(commandArgs, hostKeyOptions...)
	for _, arg := range args {
		if colon := strings.IndexByte(arg, ':'); colon > 0 && !strings.Contains(arg[:colon], "/") {
			id := arg[:colon]
			if _, err := c.boxID(id); err == nil {
				arg = "abc@" + id + arg[colon:]
			}
		}
		commandArgs = append(commandArgs, arg)
	}
	return c.run(ctx, externalCommand("scp", runtime.GOOS), commandArgs...)
}

func externalCommand(name, goos string) string {
	if goos == "windows" {
		return name + ".exe"
	}
	return name
}

func (c *CLI) prompt(ctx context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		catalog, err := c.client.Do(ctx, http.MethodGet, "/provider-models", nil, nil)
		if err != nil {
			return err
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, catalog, "", "  "); err != nil {
			return fmt.Errorf("format provider models: %w", err)
		}
		_, err = fmt.Fprintf(c.Out, "Usage: boxes prompt [id|current] \"text\" [--provider claude|codex|opencode|pi|prime|kimi --model MODEL]\nKnown models and reasoning levels:\n%s\n", pretty.String())
		return err
	}
	if len(args) == 0 {
		return errors.New("usage: boxes prompt [id] \"text\" [--provider PROVIDER --model MODEL --new --conversation ID --stop-when-done]")
	}
	idArg := "current"
	promptArgs := args
	if promptPositionalCount(args) >= 2 && !strings.HasPrefix(args[0], "-") {
		idArg, promptArgs = args[0], args[1:]
	}
	id, err := c.boxID(idArg)
	if err != nil {
		return err
	}
	fs := c.flagSet("prompt")
	provider := fs.String("provider", "claude", "agent provider: claude, codex, opencode, pi, prime, kimi")
	model := fs.String("model", "", "model name")
	newConversation := fs.Bool("new", false, "start a new conversation")
	conversation := fs.String("conversation", "", "conversation ID")
	stopWhenDone := fs.Bool("stop-when-done", false, "stop and snapshot the box when the run finishes")
	if err := fs.Parse(flagsBeforePositionals(fs, promptArgs)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("prompt text is required")
	}
	switch *provider {
	case "claude", "codex", "opencode", "pi", "prime", "kimi":
	default:
		return fmt.Errorf("unsupported provider %q; choose claude, codex, opencode, pi, prime, or kimi", *provider)
	}
	body := map[string]any{"prompt": fs.Arg(0), "provider": *provider}
	if *model != "" {
		body["model"] = *model
	}
	if *newConversation {
		body["new"] = true
	}
	if *conversation != "" {
		body["conversationId"] = *conversation
	}
	if *stopWhenDone {
		body["stopWhenDone"] = true
	}
	result, err := c.client.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/prompt", body, nil)
	if err != nil {
		return err
	}
	var started struct {
		RunID          string `json:"runId"`
		ConversationID string `json:"conversationId"`
	}
	if err := json.Unmarshal(result, &started); err != nil {
		return fmt.Errorf("decode prompt response: %w", err)
	}
	if started.RunID == "" {
		return errors.New("prompt response did not include runId")
	}
	return c.followRun(ctx, id, started.RunID, started.ConversationID)
}

func (c *CLI) followRun(ctx context.Context, id, runID, conversationID string) error {
	cursor := int64(0)
	readEvents := func() error {
		query := url.Values{"after": {fmt.Sprint(cursor)}, "wait": {"1"}}
		if conversationID != "" {
			query.Set("conversation", conversationID)
		}
		data, err := c.client.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id)+"/events?"+query.Encode(), nil, nil)
		if err != nil {
			return err
		}
		var events struct {
			Events []json.RawMessage `json:"events"`
			Cursor int64             `json:"cursor"`
		}
		if err := json.Unmarshal(data, &events); err != nil {
			return fmt.Errorf("decode events: %w", err)
		}
		for _, event := range events.Events {
			var item struct {
				Cursor int64  `json:"cursor"`
				RunID  string `json:"runId"`
			}
			_ = json.Unmarshal(event, &item)
			if item.Cursor > cursor {
				cursor = item.Cursor
			}
			if item.RunID == runID {
				if err := c.printEvent(event); err != nil {
					return err
				}
			}
		}
		if events.Cursor > cursor {
			cursor = events.Cursor
		}
		return nil
	}
	for {
		if err := readEvents(); err != nil {
			return err
		}
		runData, err := c.client.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id)+"/runs/"+url.PathEscape(runID), nil, nil)
		if err != nil {
			return err
		}
		var run struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(runData, &run); err != nil {
			return fmt.Errorf("decode run: %w", err)
		}
		switch run.Status {
		case "completed":
			return readEvents()
		case "failed", "interrupted":
			if err := readEvents(); err != nil {
				return err
			}
			if run.Error != "" {
				return fmt.Errorf("run %s: %s", run.Status, run.Error)
			}
			return fmt.Errorf("run %s", run.Status)
		}
	}
}

func (c *CLI) events(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: boxes events <id> [--follow] [--after CURSOR]")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	fs := c.flagSet("events")
	follow := fs.Bool("follow", false, "follow new events")
	after := fs.Int64("after", 0, "event cursor")
	if err := fs.Parse(flagsBeforePositionals(fs, args[1:])); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: boxes events <id> [--follow] [--after CURSOR]")
	}
	cursor := *after
	for {
		query := url.Values{"after": {fmt.Sprint(cursor)}}
		if *follow {
			query.Set("wait", "25")
		}
		data, err := c.client.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id)+"/events?"+query.Encode(), nil, nil)
		if err != nil {
			return err
		}
		var result struct {
			Events []json.RawMessage `json:"events"`
			Cursor int64             `json:"cursor"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("decode events: %w", err)
		}
		for _, event := range result.Events {
			if err := c.printEvent(event); err != nil {
				return err
			}
			var item struct {
				Cursor int64 `json:"cursor"`
			}
			_ = json.Unmarshal(event, &item)
			if item.Cursor > cursor {
				cursor = item.Cursor
			}
		}
		if result.Cursor > cursor {
			cursor = result.Cursor
		}
		if !*follow {
			return nil
		}
	}
}

func (c *CLI) steer(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: boxes steer <id> <message> [--conversation ID]")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	fs := c.flagSet("steer")
	conversation := fs.String("conversation", "", "conversation ID")
	if err := fs.Parse(flagsBeforePositionals(fs, args[1:])); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: boxes steer <id> <message> [--conversation ID]")
	}
	body := map[string]string{"message": fs.Arg(0)}
	if *conversation != "" {
		body["conversationId"] = *conversation
	}
	return c.postAction(ctx, "/sandboxes/"+url.PathEscape(id)+"/steer", body)
}

func (c *CLI) interrupt(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: boxes interrupt <id> [--conversation ID]")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	fs := c.flagSet("interrupt")
	conversation := fs.String("conversation", "", "conversation ID")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: boxes interrupt <id> [--conversation ID]")
	}
	body := map[string]string{}
	if *conversation != "" {
		body["conversationId"] = *conversation
	}
	return c.postAction(ctx, "/sandboxes/"+url.PathEscape(id)+"/interrupt", body)
}

func (c *CLI) desktop(ctx context.Context, args []string, browser bool) error {
	if len(args) == 0 {
		return errors.New("usage: boxes desktop <id> [--public] [--vnc]")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	fs := c.flagSet("desktop")
	public := fs.Bool("public", false, "create a public desktop URL")
	_ = fs.Bool("vnc", false, "ignored; desktop mode is selected by the box image")
	profile := fs.String("profile", "", "browser profile name")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected desktop arguments")
	}
	body := map[string]any{}
	if *public {
		body["public"] = true
	}
	if browser {
		body["browser"] = true
		if *profile != "" {
			body["profile"] = *profile
		}
	}
	result, err := c.client.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/desktop", body, nil)
	if err != nil {
		return err
	}
	var response struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(result, &response); err != nil {
		return fmt.Errorf("decode desktop response: %w", err)
	}
	if response.URL == "" {
		return errors.New("desktop response did not include a URL")
	}
	if c.jsonOutput {
		if err := c.printValue(result); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintln(c.Out, response.URL); err != nil {
		return err
	}
	return c.OpenBrowser(ctx, response.URL)
}

func (c *CLI) host(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: boxes host <id> <port> [--private]")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	fs := c.flagSet("host")
	private := fs.Bool("private", false, "require authentication for the host")
	if err := fs.Parse(flagsBeforePositionals(fs, args[1:])); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: boxes host <id> <port> [--private]")
	}
	port, err := strconv.Atoi(fs.Arg(0))
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid port %q", fs.Arg(0))
	}
	result, err := c.client.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/hosts", map[string]any{"port": port, "private": *private}, nil)
	if err != nil {
		return err
	}
	return c.printValue(result)
}

func (c *CLI) action(ctx context.Context, args []string, suffix, method string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: boxes %s <id>", actionName(suffix, method))
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	endpoint := "/sandboxes/" + url.PathEscape(id)
	if suffix != "" {
		endpoint += "/" + suffix
	}
	result, err := c.client.doJSON(ctx, method, endpoint, nil, nil)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(result))) == 0 || string(result) == "null" {
		if c.jsonOutput {
			return c.printJSON(map[string]any{"ok": true})
		}
		return nil
	}
	return c.printValue(result)
}

func (c *CLI) stop(ctx context.Context, args []string) error {
	if len(args) > 1 {
		return errors.New("usage: boxes stop [id|current]")
	}
	idArg := "current"
	if len(args) == 1 {
		idArg = args[0]
	}
	id, err := c.boxID(idArg)
	if err != nil {
		return err
	}
	result, err := c.client.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/stop", nil, nil)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(result))) == 0 || string(result) == "null" {
		if c.jsonOutput {
			return c.printJSON(map[string]any{"ok": true})
		}
		return nil
	}
	return c.printValue(unwrap(result, "sandbox"))
}

func (c *CLI) resume(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: boxes resume <id> [--type SIZE]")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	fs := c.flagSet("resume")
	boxType := fs.String("type", "", "box size")
	ttl := fs.Int64("ttl", 0, "time to live in seconds")
	noAutoStop := fs.Bool("no-auto-stop", false, "disable the auto-stop timer")
	if err := fs.Parse(flagsBeforePositionals(fs, args[1:])); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected resume arguments")
	}
	if *noAutoStop && flagWasSet(fs, "ttl") {
		return errors.New("--ttl and --no-auto-stop are mutually exclusive")
	}
	body := map[string]any{}
	if *boxType != "" {
		body["type"] = *boxType
	}
	if *noAutoStop {
		body["ttlSeconds"] = nil
	} else if flagWasSet(fs, "ttl") {
		body["ttlSeconds"] = *ttl
	}
	result, err := c.client.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/resume", body, nil)
	if err != nil {
		return err
	}
	return c.printValue(unwrap(result, "sandbox"))
}

func (c *CLI) fork(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: boxes fork <id|current> [--ttl SECONDS|--no-auto-stop]")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	fs := c.flagSet("fork")
	ttl := fs.Int64("ttl", 0, "time to live in seconds")
	noAutoStop := fs.Bool("no-auto-stop", false, "disable the auto-stop timer")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected fork arguments")
	}
	if *noAutoStop && flagWasSet(fs, "ttl") {
		return errors.New("--ttl and --no-auto-stop are mutually exclusive")
	}
	body := map[string]any{}
	if *noAutoStop {
		body["ttlSeconds"] = nil
	} else if flagWasSet(fs, "ttl") {
		body["ttlSeconds"] = *ttl
	}
	result, err := c.client.doJSON(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(id)+"/fork", body, nil)
	if err != nil {
		return err
	}
	box := unwrap(result, "sandbox")
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(box, &created); err != nil {
		return fmt.Errorf("decode fork response: %w", err)
	}
	if created.ID != "" {
		if err := c.setCurrent(created.ID); err != nil {
			return err
		}
	}
	return c.printValue(box)
}

func (c *CLI) extend(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: boxes extend <id|current> --hours N | --ttl SECONDS | --no-auto-stop")
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	fs := c.flagSet("extend")
	hours := fs.Int64("hours", 0, "extend by N hours")
	ttl := fs.Int64("ttl", 0, "set auto-stop timer in seconds")
	noAutoStop := fs.Bool("no-auto-stop", false, "disable the auto-stop timer")
	idleStop := fs.String("idle-stop", "", "set inactivity stop duration (Go duration or off)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected extend arguments")
	}
	selected := 0
	for _, name := range []string{"hours", "ttl", "no-auto-stop", "idle-stop"} {
		if flagWasSet(fs, name) {
			selected++
		}
	}
	if selected != 1 {
		return errors.New("choose exactly one of --hours, --ttl, --no-auto-stop, or --idle-stop")
	}
	body := map[string]any{}
	switch {
	case *noAutoStop:
		body["ttlSeconds"] = nil
	case flagWasSet(fs, "idle-stop"):
		seconds, err := parseIdleStopFlag(*idleStop)
		if err != nil {
			return err
		}
		body["idleStopSeconds"] = seconds
	case flagWasSet(fs, "hours"):
		if *hours <= 0 || *hours > 720 {
			return errors.New("--hours must be between 1 and 720")
		}
		body["ttlSeconds"] = *hours * 3600
	default:
		body["ttlSeconds"] = *ttl
	}
	result, err := c.client.doJSON(ctx, http.MethodPatch, "/sandboxes/"+url.PathEscape(id), body, nil)
	if err != nil {
		return err
	}
	return c.printValue(unwrap(result, "sandbox"))
}

func parseIdleStopFlag(value string) (int64, error) {
	if strings.EqualFold(strings.TrimSpace(value), "off") {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("--idle-stop must be a Go duration (such as 30m) or off")
	}
	if duration < 5*time.Minute || duration > 30*24*time.Hour {
		return 0, errors.New("--idle-stop must be between 5m and 720h, or off")
	}
	return int64(duration / time.Second), nil
}

func (c *CLI) delete(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: boxes delete <id|current> [--yes]")
	}
	fs := c.flagSet("delete")
	yes := fs.Bool("yes", false, "confirm permanent deletion")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: boxes delete <id|current> [--yes]")
	}
	if !*yes {
		if !c.Interactive {
			return errors.New("delete requires --yes when stdin is not a TTY")
		}
		if _, err := fmt.Fprintf(c.Err, "Permanently delete %s? [y/N] ", args[0]); err != nil {
			return err
		}
		answer, err := bufio.NewReader(c.In).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if strings.ToLower(strings.TrimSpace(answer)) != "y" && strings.ToLower(strings.TrimSpace(answer)) != "yes" {
			return errors.New("delete cancelled")
		}
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	result, err := c.client.doJSON(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return err
	}
	var accepted struct {
		Operation struct {
			ID string `json:"id"`
		} `json:"operation"`
	}
	if json.Unmarshal(result, &accepted) == nil && accepted.Operation.ID != "" {
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
			var status struct {
				Operation struct {
					Status string `json:"status"`
					Error  string `json:"error"`
				} `json:"operation"`
			}
			data, err := c.client.doJSON(ctx, http.MethodGet, "/operations/"+url.PathEscape(accepted.Operation.ID), nil, nil)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &status); err != nil {
				return err
			}
			switch status.Operation.Status {
			case "done":
				_, err := fmt.Fprintf(c.Out, "Deleted %s\n", id)
				return err
			case "failed":
				return fmt.Errorf("delete failed: %s", status.Operation.Error)
			}
		}
	}
	if c.jsonOutput {
		if len(strings.TrimSpace(string(result))) == 0 || string(result) == "null" {
			return c.printJSON(map[string]any{"ok": true})
		}
	}
	if len(strings.TrimSpace(string(result))) == 0 || string(result) == "null" {
		return nil
	}
	return c.printValue(result)
}

func (c *CLI) boxSubresource(ctx context.Context, args []string, name string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: boxes %s <id|current>", name)
	}
	id, err := c.boxID(args[0])
	if err != nil {
		return err
	}
	result, err := c.client.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id)+"/"+name, nil, nil)
	if err != nil {
		return err
	}
	return c.printValue(unwrap(result, name))
}

func (c *CLI) limits(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: boxes limits")
	}
	result, err := c.client.doJSON(ctx, http.MethodGet, "/limits", nil, nil)
	if err != nil {
		return err
	}
	return c.printValue(result)
}

func (c *CLI) status(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: boxes status")
	}
	if c.config.Current == "" {
		return errors.New("no current box; create or use a box first")
	}
	return c.info(ctx, []string{"current"})
}

func (c *CLI) snapshot(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: boxes snapshot <id> <name> | snapshot pull <snapshotId|boxId> [-o file.tar.gz] | snapshot rm <name>")
	}
	switch args[0] {
	case "pull":
		return c.snapshotPull(ctx, args[1:])
	case "rm":
		if len(args) != 2 {
			return errors.New("usage: boxes snapshot rm <name|snapshotId>")
		}
		var named struct {
			NamedSnapshots []struct {
				Name string `json:"name"`
			} `json:"namedSnapshots"`
		}
		data, err := c.client.Do(ctx, http.MethodGet, "/named-snapshots", nil, nil)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &named); err != nil {
			return err
		}
		for _, snapshot := range named.NamedSnapshots {
			if snapshot.Name == args[1] {
				_, err := c.client.Do(ctx, http.MethodDelete, "/named-snapshots/"+url.PathEscape(args[1]), nil, nil)
				return err
			}
		}
		return c.deleteSnapshot(ctx, []string{args[1]})
	default:
		if len(args) != 2 {
			return errors.New("usage: boxes snapshot <id> <name>")
		}
		id, err := c.boxID(args[0])
		if err != nil {
			return err
		}
		result, err := c.client.doJSON(ctx, http.MethodPost, "/named-snapshots", map[string]string{"boxId": id, "name": args[1]}, nil)
		if err != nil {
			return err
		}
		return c.printValue(result)
	}
}

func (c *CLI) deleteSnapshot(ctx context.Context, args []string) error {
	var boxID, snapshotID string
	if len(args) == 2 {
		var err error
		boxID, err = c.boxID(args[0])
		if err != nil {
			return err
		}
		snapshotID = args[1]
	} else if len(args) == 1 {
		snapshotID = args[0]
		var boxes struct {
			Sandboxes []struct {
				ID string `json:"id"`
			} `json:"sandboxes"`
		}
		data, err := c.client.Do(ctx, http.MethodGet, "/sandboxes", nil, nil)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &boxes); err != nil {
			return err
		}
		for _, box := range boxes.Sandboxes {
			var result struct {
				Snapshots []struct {
					ID string `json:"id"`
				} `json:"snapshots"`
			}
			data, err := c.client.Do(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(box.ID)+"/snapshots", nil, nil)
			if err != nil {
				continue
			}
			if json.Unmarshal(data, &result) != nil {
				continue
			}
			for _, snapshot := range result.Snapshots {
				if snapshot.ID == snapshotID {
					boxID = box.ID
					break
				}
			}
			if boxID != "" {
				break
			}
		}
	} else {
		return errors.New("usage: boxes snapshot rm <name|snapshotId> or boxes snapshots rm <boxId> <snapshotId>")
	}
	if boxID == "" {
		return fmt.Errorf("snapshot %q was not found", snapshotID)
	}
	_, err := c.client.Do(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(boxID)+"/snapshots/"+url.PathEscape(snapshotID), nil, nil)
	return err
}

func (c *CLI) snapshotPull(ctx context.Context, args []string) error {
	if len(args) != 1 && len(args) != 3 {
		return errors.New("usage: boxes snapshot pull <snapshotId|boxId> [-o file.tar.gz]")
	}
	output := ""
	if len(args) == 3 {
		if args[1] != "-o" || args[2] == "" {
			return errors.New("usage: boxes snapshot pull <snapshotId|boxId> [-o file.tar.gz]")
		}
		output = args[2]
	}
	id := args[0]
	if strings.HasPrefix(id, "bx_") {
		data, err := c.client.doJSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(id)+"/snapshots/latest", nil, nil)
		if err != nil {
			return err
		}
		var latest struct {
			Snapshot *struct {
				ID string `json:"id"`
			} `json:"snapshot"`
		}
		if err := json.Unmarshal(data, &latest); err != nil {
			return err
		}
		if latest.Snapshot == nil {
			return errors.New("box has no completed snapshot")
		}
		id = latest.Snapshot.ID
	}
	if _, err := requiredID(id); err != nil {
		return fmt.Errorf("invalid snapshot ID: %w", err)
	}
	if output == "" {
		output = id + ".tar.gz"
	}
	file, err := os.Create(output)
	if err != nil {
		return err
	}
	defer file.Close()
	var written int64
	progress := func(total int64) { written = total }
	if f, ok := c.Err.(*os.File); ok {
		if info, statErr := f.Stat(); statErr == nil && info.Mode()&os.ModeCharDevice != 0 {
			progress = func(total int64) { written = total; fmt.Fprintf(c.Err, "\rDownloaded %d bytes", total) }
		}
	}
	err = c.client.Download(ctx, "/snapshots/"+url.PathEscape(id)+"/download", file, progress)
	if err != nil {
		_ = os.Remove(output)
		return err
	}
	fmt.Fprintf(c.Err, "\rSaved %s (%d bytes)\n", output, written)
	return nil
}

func (c *CLI) environment(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: boxes env list|create|update|delete|upgrade ...")
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: boxes env list")
		}
		result, err := c.client.doJSON(ctx, http.MethodGet, "/environments", nil, nil)
		if err != nil {
			return err
		}
		return c.printValue(unwrap(result, "environments"))
	case "create":
		if len(args) < 2 {
			return errors.New("usage: boxes env create <name> [JSON document]")
		}
		body := map[string]any{"name": args[1]}
		if len(args) > 2 {
			var document map[string]any
			if err := json.Unmarshal([]byte(strings.Join(args[2:], " ")), &document); err != nil {
				return fmt.Errorf("environment document must be JSON: %w", err)
			}
			body["document"] = document
		}
		result, err := c.client.doJSON(ctx, http.MethodPost, "/environments", body, nil)
		if err != nil {
			return err
		}
		return c.printValue(unwrap(result, "environment"))
	case "update":
		if len(args) != 3 {
			return errors.New("usage: boxes env update <id> <JSON document>")
		}
		id, err := requiredID(args[1])
		if err != nil {
			return err
		}
		var document map[string]any
		if err := json.Unmarshal([]byte(args[2]), &document); err != nil {
			return fmt.Errorf("environment document must be JSON: %w", err)
		}
		result, err := c.client.doJSON(ctx, http.MethodPatch, "/environments/"+url.PathEscape(id), map[string]any{"document": document}, nil)
		if err != nil {
			return err
		}
		return c.printValue(unwrap(result, "environment"))
	case "delete", "rm":
		if len(args) != 2 {
			return errors.New("usage: boxes env delete <id>")
		}
		id, err := requiredID(args[1])
		if err != nil {
			return err
		}
		_, err = c.client.Do(ctx, http.MethodDelete, "/environments/"+url.PathEscape(id), nil, nil)
		if err == nil && c.jsonOutput {
			return c.printJSON(map[string]any{"ok": true})
		}
		return err
	case "upgrade":
		if len(args) != 2 {
			return errors.New("usage: boxes env upgrade <id>")
		}
		id, err := requiredID(args[1])
		if err != nil {
			return err
		}
		result, err := c.client.doJSON(ctx, http.MethodPost, "/environments/"+url.PathEscape(id)+"/upgrade", map[string]any{}, nil)
		if err != nil {
			return err
		}
		return c.printValue(unwrap(result, "environment"))
	default:
		return fmt.Errorf("unknown env command %q", args[0])
	}
}

func (c *CLI) postAction(ctx context.Context, endpoint string, body any) error {
	result, err := c.client.doJSON(ctx, http.MethodPost, endpoint, body, nil)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(result))) == 0 || string(result) == "null" {
		return nil
	}
	return c.printValue(result)
}

func (c *CLI) printEvent(event json.RawMessage) error {
	if c.jsonOutput {
		return c.printValue(event)
	}
	var item struct {
		Cursor int64  `json:"cursor"`
		Type   string `json:"type"`
		RunID  string `json:"runId"`
		Data   any    `json:"data"`
	}
	if err := json.Unmarshal(event, &item); err != nil {
		return err
	}
	_, err := fmt.Fprintf(c.Out, "%d\t%s\t%s\n", item.Cursor, item.Type, compactJSON(item.Data))
	return err
}

func (c *CLI) printValue(raw json.RawMessage) error {
	if c.jsonOutput {
		var compact json.RawMessage
		if err := json.Unmarshal(raw, &compact); err != nil {
			return err
		}
		_, err := fmt.Fprintf(c.Out, "%s\n", compact)
		return err
	}
	var object map[string]any
	if json.Unmarshal(raw, &object) == nil {
		if id, ok := object["id"]; ok {
			if state, stateOK := object["state"]; stateOK {
				_, err := fmt.Fprintf(c.Out, "%s\t%s\n", id, state)
				return err
			}
			if _, err := fmt.Fprintln(c.Out, id); err != nil {
				return err
			}
			return nil
		}
	}
	_, err := fmt.Fprintln(c.Out, compactJSON(raw))
	return err
}

func (c *CLI) printJSON(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(c.Out, "%s\n", data)
	return err
}

func (c *CLI) flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// flagsBeforePositionals permits the documented command forms that place
// options after their positional argument, while retaining stdlib flag parsing.
func flagsBeforePositionals(fs *flag.FlagSet, args []string) []string {
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
			positionals = append(positionals, arg)
			continue
		}
		nameValue := strings.TrimLeft(arg, "-")
		name, _, hasValue := strings.Cut(nameValue, "=")
		definition := fs.Lookup(name)
		if definition == nil {
			flags = append(flags, arg)
			continue
		}
		flags = append(flags, arg)
		if boolFlag, ok := definition.Value.(interface{ IsBoolFlag() bool }); ok && boolFlag.IsBoolFlag() {
			continue
		}
		if !hasValue && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positionals...)
}

func (c *CLI) printUsage() {
	_, _ = fmt.Fprintln(c.Out, "Usage: boxes [--json] <command>")
	_, _ = fmt.Fprintln(c.Out, "Commands: login logout whoami new list get info status current exec ps logs kill ssh proxy scp push pull prompt events steer interrupt conversations snapshots restore usage limits desktop browser host stop resume extend update fork snapshot delete dashboard forward skill env")
	_, _ = fmt.Fprintln(c.Out, "Push: boxes push <id> [dir] [--to path]")
	_, _ = fmt.Fprintln(c.Out, "Box sizes: xsmall (1 CPU, 2 GiB), small (2 CPU, 4 GiB), default (4 CPU, 8 GiB), large (8 CPU, 16 GiB), xlarge (16 CPU, 32 GiB)")
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func unwrap(raw json.RawMessage, key string) json.RawMessage {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) == nil {
		if value, ok := envelope[key]; ok {
			return value
		}
	}
	return raw
}

func compactJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "null"
	}
	return string(data)
}

func requiredID(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "/\\\r\n\t ") || value == "." || value == ".." {
		return "", fmt.Errorf("invalid ID %q", value)
	}
	return value, nil
}

func (c *CLI) boxID(value string) (string, error) {
	if value == "current" {
		value = c.config.Current
		if value == "" {
			return "", errors.New("no current box; create or use a box first")
		}
	}
	id, err := requiredID(value)
	if err != nil {
		return "", err
	}
	if err := c.setCurrent(id); err != nil {
		return "", err
	}
	return id, nil
}

func (c *CLI) setCurrent(id string) error {
	config := c.config
	config.Current = id
	return c.saveConfig(config)
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func indexOf(values []string, value string) int {
	for i, item := range values {
		if item == value {
			return i
		}
	}
	return -1
}

func promptPositionalCount(args []string) int {
	count := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			count++
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		switch name {
		case "provider", "model", "conversation":
			if !hasValue && i+1 < len(args) {
				i++
			}
		case "new", "stop-when-done":
		default:
			count++
		}
	}
	return count
}

func managedSkillContent() []byte {
	const marker = "<!-- boxes-skill-managed -->"
	body := strings.Replace(string(embeddedSkill), marker, "", 1)
	hash := sha256.Sum256([]byte(body))
	managedMarker := "<!-- boxes-skill-managed: sha256=" + hex.EncodeToString(hash[:]) + " -->"
	return []byte(strings.Replace(string(embeddedSkill), marker, managedMarker, 1))
}

func managedSkillIsUnmodified(content []byte) bool {
	text := string(content)
	start := strings.Index(text, "<!-- boxes-skill-managed: sha256=")
	if start < 0 {
		return false
	}
	end := strings.Index(text[start:], " -->")
	if end < 0 {
		return false
	}
	end += start + len(" -->")
	marker := text[start:end]
	const prefix = "<!-- boxes-skill-managed: sha256="
	if !strings.HasPrefix(marker, prefix) {
		return false
	}
	expected := strings.TrimSuffix(strings.TrimPrefix(marker, prefix), " -->")
	withoutMarker := text[:start] + text[end:]
	hash := sha256.Sum256([]byte(withoutMarker))
	return expected == hex.EncodeToString(hash[:])
}

func idempotencyKey() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate idempotency key: %w", err)
	}
	return hex.EncodeToString(random[:]), nil
}

func actionName(suffix, method string) string {
	switch {
	case method == http.MethodDelete:
		return "delete"
	case suffix != "":
		return suffix
	default:
		return "action"
	}
}
