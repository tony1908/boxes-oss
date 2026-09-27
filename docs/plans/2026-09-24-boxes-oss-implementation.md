# boxes-oss Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Executor is Luna 6 via OpenCode (see AGENTS.md); Opus verifies each task.

**Goal:** Open-source, self-hosted boat.dev: agent machines on your own VPSes, managed Dokploy-style (one-line install, web UI, CLI).

**Architecture:** `boxes-server` (Go, SQLite, embedded Next.js UI) ⇄ reverse WSS/yamux tunnel ⇄ `boxes-agent` on each node (Docker CLI + Sysbox runtime, restic, Traefik file config) → box containers (Webtop XFCE image + `boxd` agent server + computer-use MCP). Design: `docs/plans/2026-09-24-boxes-oss-design.md`.

**Tech Stack:** Go 1.26 (`net/http` ServeMux patterns, `modernc.org/sqlite`, `github.com/hashicorp/yamux`, `github.com/coder/websocket`, `golang.org/x/crypto/ssh`, `github.com/modelcontextprotocol/go-sdk`), restic, Traefik v3, Next.js (static export) + TypeScript, LinuxServer Webtop.

**Module:** `github.com/boxes-oss/boxes`

---

## Global rules (every task)
- TDD: write the failing test first, then the code. Every package has `_test.go` files. External commands (docker, restic, xdotool, harness CLIs) go behind a small `Executor` interface so tests use fakes. Never shell-string-concatenate user input; always build argv slices.
- Keep it minimal (YAGNI). No interface with one implementation except the executor seams above.
- Done = `go build ./... && go vet ./... && go test ./...` all green (plus task-specific checks).
- Never log secrets, tokens, or desktop URLs.
- Only touch the files your task lists (plus `go.mod`/`go.sum`).

## Shared contracts

### Box sizes (`internal/types`)
| type | cpus | memory | disk |
|---|---|---|---|
| small | 2 | 4 GiB | 12 GiB |
| default | 4 | 8 GiB | 50 GiB |
| large | 8 | 16 GiB | 125 GiB |
| xlarge | 16 | 32 GiB | 251 GiB |

Box states: `provisioning, ready, idle, stopping, stopped, resuming, error, deleting, unreachable`.
IDs: `bx_` + 12 lowercase base32 chars (boxes), `nd_` (nodes), `sn_` (snapshots), `cv_` (conversations), `run_`, `ev_` (events use int64 cursor), `env_`, `wh_`, `key_`.

### Agent HTTP API (served by `boxes-agent`, reached by the server through the tunnel)
All JSON. Container name `box-<boxId>`.
- `GET /v1/health` → `{nodeId, cpus, memoryBytes, freeMemoryBytes, diskFreeBytes, runtime, boxes:[{boxId, containerId, state}]}`
- `POST /v1/boxes` `{boxId, image, type, runtime, env{}, files[{path,mode,content(base64)}], fromSnapshot?, desktopPassword, boxdToken}` → create, (restore), start → `{containerId}`
- `POST /v1/boxes/{id}/stop` → always capture first; a failed capture returns 409 and the box keeps running → `{snapshot}`
- `POST /v1/boxes/{id}/start`, `DELETE /v1/boxes/{id}`
- `POST /v1/boxes/{id}/exec` `{cmd:[...]|string, cwd, user, env{}, timeoutSeconds(≤600), detached}` → `{exitCode, stdout, stderr}` or `{processId}`; `GET /v1/boxes/{id}/exec/{processId}` → `{running, exitCode?, stdout, stderr}`
- `GET /v1/boxes/{id}/files?path=` (bytes) · `PUT /v1/boxes/{id}/files?path=&mode=` (body bytes)
- `POST /v1/boxes/{id}/snapshot` → `{snapshotId, sizeBytes, createdAt}`
- `GET /v1/snapshots/{sid}/tree` → `[{path,size,type}]` · `GET /v1/snapshots/{sid}/file?path=` (bytes, or tar for dirs) · `DELETE /v1/snapshots/{sid}`
- `PUT /v1/boxes/{id}/routes` `{desktop:{host, tokenHash, expiresAt}?, hosts:[{host, port, private, tokenHash?}]}` → writes Traefik dynamic file
- `GET /v1/boxes/{id}/tcp/{port}` (WebSocket) → raw TCP bridge to container IP:port (SSH)
- `ANY /v1/boxes/{id}/boxd/{path...}` → reverse proxy to `http://<containerIP>:7070/{path}` with `Authorization: Bearer <boxdToken>`
- Local only (127.0.0.1:7001, not via tunnel): `GET /auth/forward` Traefik forward-auth (token from `?token=` in `X-Forwarded-Uri` or cookie `boxes_auth`; sets cookie on success).

### Snapshot mechanism (runtime-agnostic, decided in M0)
- Capture (box running): `docker diff box-<id>` → changed/added paths (A/C) minus excludes, deleted paths (D) → `docker exec box-<id> tar --create --no-recursion --numeric-owner --xattrs --acls --file - --files-from -` (stdin = NUL-separated path list, includes `/.boxes/deletions` written first) piped into `restic backup --stdin --stdin-filename rootfs.tar --tag box=<id>`.
- Excludes: `/proc /sys /dev /run /tmp /var/tmp /var/lib/docker /var/cache/apt /config/.cache /.boxes/restored` + `.boxignore` rules (gitignore syntax, dirs up to 6 levels under `/config`, rule paths relative to the file's dir).
- Restore (container created, not started): `restic dump <sid> rootfs.tar | docker cp -a - box-<id>:/`; the image's cont-init script `/custom-cont-init.d/00-boxes-restore` deletes every path listed in `/.boxes/deletions`, then renames it to `/.boxes/restored`.
- restic repo: `RESTIC_REPOSITORY` (S3 URL or local path) + `RESTIC_PASSWORD` from node config.

### boxd HTTP API (inside box, `:7070`, `Authorization: Bearer $BOXD_TOKEN`)
- `POST /prompt` `{prompt, provider(claude|codex|opencode), model?, conversationId?, new?}` → `{runId, conversationId}`
- `POST /steer` `{conversationId?, message}` · `POST /interrupt` `{conversationId?}` (none = all)
- `GET /conversations` → `[{id, provider, model, prompts, running, lastPrompt, current}]`
- `GET /runs/{runId}` → `{id, conversationId, status(queued|running|completed|failed|interrupted), error?}`
- `GET /events?after=<cursor>&wait=<sec>&conversation=` → `{events:[Event], cursor}` (long-poll)
- `GET /healthz`
- Event: `{cursor int64, id, conversationId, runId?, type, time, data{}}`; types `prompt, message.delta, message, tool.call, tool.result, turn.started, turn.completed, turn.failed, lifecycle, setup.output`.

### Server REST API (`/api/v1`, `Authorization: Bearer <session|api key>`)
`POST /auth/signup` (first user → owner; afterwards owner-only) · `POST /auth/login` · `GET /me` · `GET|POST|DELETE /api-keys` · `GET|POST /nodes` (POST → `{node, joinToken, installCommand}`) · `POST /nodes/ssh` `{name, host, port, user, privateKey}` · `POST /nodes/{id}/drain` · `DELETE /nodes/{id}` · `GET /agent/connect` (WebSocket, node token) · `GET|POST /sandboxes` (`Idempotency-Key`) · `GET|PATCH|DELETE /sandboxes/{id}` · `POST /sandboxes/{id}/stop|resume|fork` · `POST /sandboxes/{id}/commands` · `GET /sandboxes/{id}/commands/{pid}` · `GET|PUT /sandboxes/{id}/files` · `POST /sandboxes/{id}/desktop` `{browser?, profile?, public?}` · `GET|POST /sandboxes/{id}/hosts` · `DELETE /sandboxes/{id}/hosts/{port}` · `POST /sandboxes/{id}/ssh-key` · `GET /sandboxes/{id}/tcp/22` (WS) · `GET /sandboxes/{id}/snapshots` · `GET /snapshots/{sid}/tree|file` · `GET|POST /named-snapshots` · `GET|DELETE /named-snapshots/{name}` · `POST /sandboxes/{id}/prompt|steer|interrupt` · `GET /sandboxes/{id}/events|conversations` · `GET /sandboxes/{id}/runs/{runId}` · `GET|POST /environments` · `PATCH|DELETE /environments/{id}` · `POST /environments/{id}/upgrade` · `GET|POST /webhooks` · `PATCH|DELETE /webhooks/{id}` · `GET /limits`.
Errors: `{"error":{"code":"snake_case","message":"..."}}` with proper HTTP status.

---

## Tasks

### Task 1: Scaffold + shared types
Files: `go.mod`, `Makefile` (`build`, `test`, `lint`, `web`), `.gitignore`, `README.md` (what it is, status), `internal/types/types.go` (+test): sizes table with `SizeFor(name) (Size, error)`, state constants, `NewID(prefix)`, API structs (`Box`, `Node`, `Snapshot`, `NamedSnapshot`, `Event`, `Conversation`, `Run`, `Environment`, `Webhook`, `APIKey`, `User`), `APIError`.
Verify: `go test ./internal/types/...`.

### Task 2: M0 snapshot spike
Files: `scripts/spike-snapshot.sh`. Using plain Docker (runc) and the `restic/restic` image with a local repo in a Docker volume: start `ubuntu:24.04` box container → `apt-get install -y cowsay`, `rm /etc/issue`, write `/root/marker` owned by uid 1234 with xattr-free content → capture per the contract → create fresh container from `ubuntu:24.04` → restore per contract, then run the deletion step manually (the spike image has no cont-init) → start → assert `/usr/games/cowsay` exists, `/etc/issue` missing, `/root/marker` content and uid 1234. Prints `SPIKE PASS`.
Verify: run it, expect `SPIKE PASS`.

### Task 3: Store (SQLite)
Files: `internal/store/{store.go,migrations/001_init.sql,*_test.go}`. `Open(path)` (WAL, foreign keys, busy timeout), embedded migrations (`schema_migrations` table). Tables: users, sessions, api_keys (sha256 hash, scopes, expires_at), nodes (token hash, host base, capacity, status, drained, last_seen), boxes (node_id, type, state, image, env_id, env_version, ttl_seconds, expires_at, desktop_password_enc, boxd_token_enc, idempotency_key unique, from_snapshot, created_at, stopped_at), snapshots (box_id, restic_id, size, created_at), named_snapshots (name unique, snapshot_id), environments + environment_versions (JSON doc, secrets encrypted), conversations, runs, events (cursor autoincrement, box_id, conversation_id, type, data JSON), hosts (box_id, port, private, token_hash), webhooks (+ secret), idempotency. Typed CRUD methods used by the server. Tests with a temp DB.
Verify: `go test ./internal/store/...`.

### Task 4: Tunnel
Files: `internal/tunnel/{server.go,agent.go,tunnel_test.go}`. Server: `Hub` with `Handler(authn func(token) (nodeID string, ok bool)) http.Handler` accepting a WebSocket, wrapping it as `net.Conn` (`websocket.NetConn`), `yamux.Client`; `Hub.Client(nodeID) (*http.Client, bool)` whose Transport dials a yamux stream; `Hub.Connected() []string`; `OnConnect/OnDisconnect` callbacks. Agent: `Dial(ctx, url, token, handler http.Handler) error` → WebSocket → `yamux.Server` → `http.Serve(session, handler)`; `RunForever` reconnect with exponential backoff (1s→30s). Test: httptest server hub + agent handler; server GETs `http://agent/v1/health` through the tunnel; reconnect after the conn drops; WebSocket upgrade through the tunnel works (needed for exec/tcp).
Verify: `go test -race ./internal/tunnel/...`.

### Task 5: Docker CLI runtime
Files: `internal/dockercli/{docker.go,docker_test.go}`. `Executor` interface (`Run(ctx, argv, stdin io.Reader, stdout, stderr io.Writer) (exitCode int, err error)`). `CreateArgs(spec)` → `docker create --name box-<id> --hostname <id> --runtime <rt> --label boxes.managed=true --label boxes.box-id=<id> --cpus --memory --pids-limit 4096 --shm-size 1g --network boxes-net --restart unless-stopped -e PUID=1000 -e PGID=1000 -e TZ=Etc/UTC -e PASSWORD=<pw> -e BOXD_TOKEN=<tok> [-e K=V...] <image>` (storage-opt size only if `spec.DiskQuota`). Methods: `EnsureNetwork` (bridge `boxes-net`, `com.docker.network.bridge.enable_icc=false`), `EnsureImage` (inspect → pull), `Create, Start, Stop(timeout), Remove(force), Inspect → {ID, State, IP}`, `List(label)`, `Exec(ctx, name, user, cwd, env, argv, stdin, stdout, stderr)`, `Diff(name) → []Change{Kind, Path}`, `CopyIn(name, dst, tarReader)` (`docker cp -a - name:dst`). Errors carry sanitized stderr (secrets redacted). Tests: argv assertions with a scripted fake.
Verify: `go test ./internal/dockercli/...`.

### Task 6: Snapshot engine
Files: `internal/snapshot/{snapshot.go,boxignore.go,*_test.go}`. `Engine{Docker, Restic Executor, Repo, Password}`. `Capture(ctx, boxID) (Snapshot, error)` per contract (diff → filter excludes + `.boxignore` → write deletions → exec tar → `restic backup --stdin ... --json` parse `snapshot_id`, `total_bytes_processed`). `Restore(ctx, boxID, sid)` (`restic dump` piped to `CopyIn`). `Tree(sid)` (`restic dump` → read tar headers), `File(sid, path)`, `Forget(sid)` (`restic forget --prune`), `Init()` (`restic init` if `restic cat config` fails). `.boxignore` parser: gitignore subset (`*`, `**`, `?`, trailing `/`, leading `/`, `!` negation, `#` comments). Tests with fakes + boxignore table tests.
Verify: `go test ./internal/snapshot/...`.

### Task 7: Agent (`boxes-agent`)
Files: `internal/agent/{agent.go,routes.go,forwardauth.go,*_test.go}`, `cmd/boxes-agent/main.go`. Implements the Agent HTTP API over `dockercli` + `snapshot`. Config via env/flags: `BOXES_SERVER_URL`, `BOXES_NODE_TOKEN`, `BOXES_RUNTIME` (default `sysbox-runc`), `BOXES_TRAEFIK_DIR` (default `/etc/boxes/traefik/dynamic`), `RESTIC_REPOSITORY`, `RESTIC_PASSWORD`. The server schedules periodic captures for ready and idle boxes and records each result. Routes: write `box-<id>.yml` (desktop router `https` + forwardAuth `http://127.0.0.1:7001/auth/forward` with `addAuthCookiesToResponse: [boxes_auth]` + headers middleware injecting `Authorization: Basic base64(abc:<password>)` → service `http://<ip>:3000`; hosts routers → `http://<ip>:<port>`, private ones with forwardAuth). File written 0600 atomically. Detached exec keeps output in memory (cap 1 MiB per stream). Exec `user` default `abc`, cwd default `/config`. `main.go`: runs local forward-auth server on 127.0.0.1:7001 and `tunnel.RunForever`. Tests with httptest + fakes.
Verify: `go test ./internal/agent/... && go build ./cmd/boxes-agent`.

### Task 8: Server core
Files: `internal/server/{server.go,auth.go,nodes.go,sandboxes.go,scheduler.go,reaper.go,commands.go,httputil.go,*_test.go}`, `cmd/boxes-server/main.go`. Auth (bcrypt passwords, session tokens, API keys `bx_live_...` hashed with sha256, scopes `*`), nodes (join token → node token; `/agent/connect` via `tunnel.Hub`), scheduler (healthy + not drained + most free memory, or explicit node), sandbox create (idempotency; `type`, `ttlSeconds` default 3600, `environment`, `noEnv`, `fromSnapshot`/`from` named) → agent `POST /v1/boxes`; stop always captures before stopping / resume (new container from latest snapshot, optional `type`/`node`) / fork (capture source → new box from it) / delete (agent delete + forget snapshots); TTL reaper every 15s stops expired boxes; server snapshot ticker captures ready/idle boxes (default 60s via `--snapshot-interval`) and retains five unpinned snapshots per box; reconcile on node connect (agent-reported boxes vs DB; containers for boxes now owned by another node are removed = fencing); node offline → boxes `unreachable`. Commands + files proxy to agent. Config flags: `--addr`, `--data-dir`, `--public-url`, `--image`, `--snapshot-interval`. Tests: fake agent behind a real tunnel hub.
Verify: `go test ./internal/server/... && go build ./cmd/boxes-server`.

### Task 9: Server platform features
Files: `internal/server/{snapshots.go,desktop.go,hosts.go,ssh.go,events.go,agentapi.go,environments.go,secrets.go,webhooks.go,limits.go,nodessh.go,*_test.go}`. Snapshots list/tree/file + named snapshots. Desktop: mint 32-byte token (URL `https://desk-<box>.<nodeHostBase>/?token=`), TTL 15 min (`public` = 7 days), push to agent routes; `browser` mode = same with `?app=browser` placeholder handled by image later. Hosts: `https://<box>-<port>.<nodeHostBase>`. SSH key → agent exec appends to `/config/.ssh/authorized_keys`. TCP WS proxy to agent. Event pump: per ready box goroutine long-polling agent `/boxd/events` and inserting into `events`; API events endpoint reads from DB with `after` + optional `wait`. Prompt/steer/interrupt/conversations/runs proxy to boxd (store conversations/runs rows). Environments: immutable versions (vars, secret files, repos `{url, branch, tokenSecret}`, setupScript, flags `noSecrets`); secrets AES-256-GCM with master key file `<data-dir>/master.key` (created 0600); on create, env delivered as `files` to `/run/boxes/env` (dotenv, 0600) + `/run/boxes/secrets/*` + `/run/boxes/setup.json`; upgrade re-pushes files. Webhooks: events `sandbox.ready|restored|error|stopped|deleted`, body JSON, header `X-Boxes-Signature: sha256=<hmac>`, 3 retries with backoff. Limits: node capacity totals. Node SSH install: `golang.org/x/crypto/ssh`, run `install/node-install.sh` (embedded) with join token and server URL. Tests.
Verify: `go test ./internal/server/...`.

### Task 10: Computer use + MCP
Files: `internal/computer/{computer.go,computer_test.go}`, `internal/boxd/mcp.go` (+test). `computer` wraps xdotool/scrot/wmctrl with `DISPLAY=:1` via Executor: `Screenshot() (png []byte)`, `Click(x,y,button,double)`, `Move`, `Drag`, `Scroll(dir, amount)`, `Type(text)`, `Key(combo)`, `OpenURL(url)` (`xdg-open`), `LaunchApp(name)`, `ListWindows()`. MCP stdio server (`boxd mcp`) exposing those as tools `screenshot` (returns image content), `click`, `move`, `drag`, `scroll`, `type`, `key`, `open_url`, `launch_app`, `list_windows`.
Verify: `go test ./internal/computer/... ./internal/boxd/...`.

### Task 11: boxd core + claude adapter
Files: `internal/boxd/{boxd.go,conversations.go,outbox.go,api.go,harness.go,claude.go,*_test.go}`, `cmd/boxd/main.go` (`boxd serve`, `boxd mcp`, `boxd setup`). Local SQLite outbox at `/config/.boxd/boxd.db` (captured in snapshots, so conversations survive stop/resume/fork). `Harness` interface: `Start(ctx, ConvSpec) (Session, error)`; `Session{Send(prompt) error; Steer(msg) error; Interrupt() error; Events() <-chan HarnessEvent; SessionID() string; Close() error}`. Per conversation a FIFO of prompts; run statuses. Claude adapter: persistent `claude -p --input-format stream-json --output-format stream-json --verbose --dangerously-skip-permissions --mcp-config /config/.boxd/mcp.json [--model] [--resume <sid>]` as user abc in `/config`; map stream-json (`system/init`, `assistant` content blocks text/tool_use, `user` tool_result, `result`) to Events; steer = write another user message to stdin; interrupt = SIGINT. `boxd setup`: reads `/run/boxes/setup.json`, clones repos into `/config/<name>`, runs setup script once (marker `/config/.boxd/setup.done`), streams output as `setup.output` events; writes harness MCP config files (claude `/config/.boxd/mcp.json`, codex `/config/.codex/config.toml` `[mcp_servers.computer]`, opencode `/config/.config/opencode/opencode.json` `mcp.computer`) if missing. Env for harnesses loaded from `/run/boxes/env`.
Verify: `go test ./internal/boxd/... && go build ./cmd/boxd`.

### Task 12: codex + opencode adapters
Files: `internal/boxd/{codex.go,opencode.go,*_test.go}`. Codex: `codex app-server` stdio JSON-RPC (reference implementation: `orbit/crates/orbit-daemon/src/codex_harness.rs` + `codex_protocol.rs`): initialize → thread start/resume → turn start; map item/agent message deltas, command/tool items, turn completed/failed; interrupt = turn interrupt; steer = turn steer if supported else queue. OpenCode: `opencode serve --port <free>` (reference: `.../orbit-daemon/src/opencode_harness.rs` + `opencode_protocol.rs`): create session, POST message, consume `/event` SSE, abort for interrupt. Tests replay recorded JSON lines through fakes.
Verify: `go test ./internal/boxd/...`.

### Task 13: CLI `boxes`
Files: `cmd/boxes/main.go`, `internal/cli/{cli.go,client.go,*_test.go}`. stdlib `flag` subcommands: `login --server URL --email`, `new [--type] [--ttl] [--env] [--no-env] [--from] [--node]`, `list`, `get`, `exec <id> -- cmd` (and `exec <id> 'string'`), `ssh <id>` (uploads `~/.ssh/id_ed25519.pub` or `id_rsa.pub`, then execs `ssh -o ProxyCommand="boxes proxy <id>" abc@<id>`), `proxy <id>` (stdio ⇄ WS `/tcp/22`), `scp`, `prompt <id> "text" [--provider --model --new --conversation]` (then follows events until the run ends), `events <id> [--follow] [--after]`, `steer`, `interrupt`, `desktop <id> [--vnc ignored] [--public]` (prints URL + opens browser), `browser <id> [--profile]`, `host <id> <port> [--private]`, `stop`, `resume [--type --node]`, `fork`, `snapshot <id> <name>`, `snapshot pull <id|name> <dir>`, `snapshot rm <name>`, `delete`, `env ...`, `node add|list|drain|rm`. Global `--json` = JSONL. Config `~/.config/boxes/config.json` (0600). Tests against httptest server.
Verify: `go test ./internal/cli/... && go build ./cmd/boxes`.

### Task 14: Box image
Files: `images/box/Dockerfile`, `images/box/root/custom-cont-init.d/00-boxes-restore`, `images/box/root/custom-cont-init.d/10-boxes-setup`, `images/box/root/custom-services.d/{boxd,sshd,dockerd}`, `images/box/README.md`. Multi-stage: build `boxd` (Go) → `FROM lscr.io/linuxserver/webtop:ubuntu-xfce` pinned by digest (copy from Orbit's `images/orbit-webtop/Dockerfile`), install xdotool scrot wmctrl openssh-server git gh jq nodejs npm, Firefox (Mozilla apt repo, arm64-safe) / google-chrome on amd64, docker-ce (dockerd service only starts if `/proc/self/uid_map` shows a user namespace i.e. Sysbox, else sleeps forever), `@openai/codex opencode-ai @anthropic-ai/claude-code` via npm, Selkies manual 1920×1080, `BROWSER`. sshd: password auth off, key only, port 22. boxd service runs `boxd serve` as abc. Restore script per contract. Build context is repo root.
Verify: `docker build -f images/box/Dockerfile -t boxes/box:dev .` succeeds (arm64 locally) and `docker run` smoke: boxd `/healthz` answers inside.

### Task 15: Web UI
Files: `web/` Next.js App Router, `output: 'export'`, TypeScript, no UI kit (plain CSS modules, dark+light). Pages: login/signup, Sandboxes (list with state, node, type, TTL; create dialog), Sandbox detail tabs (Overview, Terminal = exec runner, Desktop = iframe of desktop URL + open-in-new-tab, Conversations = prompt box + live events via polling `events?wait=`, Snapshots, Hosts), Templates (named snapshots), Environments, Nodes (add → shows install command; SSH form), API keys, Webhooks, Settings. API base = same origin. `internal/webui/webui.go` with `//go:embed all:dist` (Makefile copies `web/out` → `internal/webui/dist`; a placeholder `index.html` is committed so `go build` works without Node). Server serves it at `/` with SPA fallback.
Verify: `pnpm --dir web build` and `go build ./cmd/boxes-server`.

### Task 16: Install + dev stack
Files: `install/install.sh` (Ubuntu/Debian: Docker, Sysbox `.deb` for arch, restic, Traefik container with file provider `/etc/boxes/traefik` + Let's Encrypt HTTP-01 + ports 80/443, `boxes-server` + `boxes-agent` systemd units from GitHub release binaries, create first join token, print URL), `install/node-install.sh` (same minus server, takes `--server --token`), `install/traefik.yml`, `deploy/dev/compose.yaml` (server + agent-in-privileged-container with `/var/run/docker.sock`, restic local repo volume, `BOXES_RUNTIME=runc`, traefik), `Dockerfile.agent`/`Dockerfile.server` for dev, `.github/workflows/ci.yml` (go vet/test, golangci-lint, web build, image build), `.goreleaser.yaml` (linux amd64/arm64 binaries).
Verify: `bash -n install/*.sh`, `docker compose -f deploy/dev/compose.yaml config`.

### Task 17: OpenAPI + TS/Python SDKs + docs
Files: `api/openapi.yaml` (all server endpoints above), `sdk/typescript` (`@boxes-oss/sdk`, small fetch client, typed), `sdk/python` (`boxes_sdk`, httpx client), `docs/{quickstart,cli,api,self-hosting,snapshots,agents,desktop}.md`, update `README.md`.
Verify: `npx @redocly/cli lint api/openapi.yaml`, `pnpm --dir sdk/typescript build`, `python3 -m py_compile sdk/python/boxes_sdk/*.py`.

### Task 18: Local end-to-end
Files: `scripts/e2e-local.sh`. Brings up `deploy/dev`, signs up, creates a box from `boxes/box:dev`, exec `echo hi`, writes a file, installs an apt package, stop (snapshot) → resume → file + package present, fork → fork has the file, desktop URL returns 200 through Traefik with token and 401 without, prompt boxd `/healthz` via server, delete. Prints `E2E PASS`.
Verify: run it.
