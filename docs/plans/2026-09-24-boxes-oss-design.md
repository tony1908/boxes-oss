# boxes-oss design

**Goal:** an open-source, self-hosted version of [boat.dev](https://boat.dev): full Ubuntu machines for AI agents, with a desktop, computer use, agent prompting, snapshots, and fork. Users run it on their own VPS or cloud provider, and install and manage it the way they would [Dokploy](https://dokploy.com): a one-line install, a web UI, and a CLI.

**Rule for every decision:** match boat's behavior, CLI shape, and API shape. Differ only when self-hosting forces it.

License: Apache 2.0, everything. No proprietary folder.

## 1. Architecture

```
            ┌──────────── Control plane (first VPS) ─────────────────┐
 Browser ──▶│ boxes-server (Go): REST + WS API, embedded Next.js UI   │
 CLI/SDK ──▶│   SQLite · Traefik · restic repo (S3 or local)          │
            └──────────────────┬──────────────────────────────────────┘
                               │ WSS reverse tunnel (yamux), agent dials out
          ┌────────────────────┴────────────────────┐
   Node (any VPS)                              Node (any VPS)
   boxes-agent ── Docker + Sysbox ── Traefik ── box containers
                                                  ├ Ubuntu (Webtop, s6 init), Docker inside
                                                  ├ XFCE + Selkies desktop
                                                  ├ boxd (agent server + computer-use MCP)
                                                  └ claude · codex · opencode
```

| Program | Runs on | Job |
|---|---|---|
| `boxes-server` | control plane | API, auth, scheduler, SQLite, event store, environments/secrets, webhooks, serves the UI |
| `boxes-agent` | every node | Controls Docker/Sysbox locally, snapshots (restic), exec/files/SSH tunnel, Traefik routes, forwards events |
| `boxd` | inside every box (s6 service) | Conversations with agent harnesses, normalized events, computer-use tools, setup scripts |
| `boxes` | user's laptop | CLI, same verbs as boat |

### Decisions
- **Box = Docker container on the Sysbox runtime** (`--runtime=sysbox-runc`). This gives real systemd and Docker inside the box, root in the box is not root on the host (user namespaces), it needs no `--privileged`, and it runs on any VPS without KVM. A Kata runtime option for VM-level isolation on KVM hosts is left for later; it uses the same image with a different `--runtime`.
- **Go backend.** The agent dials the server over WSS with a join token and opens a **yamux** session. The server then calls the agent's ordinary HTTP API through that tunnel (exec and streams use WebSockets inside it). There's no protobuf toolchain, and it works behind NAT.
- **Next.js UI**, built as a static export and embedded in `boxes-server` with `go:embed`, so there's one binary and no Node runtime in production.
- **SQLite** (WAL, pure-Go `modernc.org/sqlite`) stores all control-plane state, with embedded SQL migrations and plain `database/sql`. The ceiling is one control-plane process; move to Postgres if HA is ever needed.
- **Box image**: ported from Orbit's `orbit-webtop` (LinuxServer Webtop Ubuntu XFCE + Selkies), run under Sysbox with its own s6-overlay init (boxd runs as an s6 service; user services go in `/custom-services.d`, which restarts them on resume, standing in for boat's "enabled systemd services"), and published to GHCR as `ghcr.io/<org>/box:<version>`. Nodes pull it instead of building it. Preinstalled, same as boat: Docker, VS Code, Chrome (amd64) or Chromium (arm64), gh, git, Node, Bun, Rust, Python, claude, codex, opencode.

## 2. Install and nodes (Dokploy-style)
- `curl -sSL https://get.boxes.dev | sh` on the first VPS installs Docker, Sysbox, Traefik, `boxes-server`, and `boxes-agent`. The first VPS is both control plane and node #1. The first user to sign up becomes the owner.
- **Add node:** in the UI or with `boxes node add --ip --ssh-key`. The server makes a single SSH connection to run the node installer (Docker, Sysbox, Traefik, agent) with a one-time join token, and the agent receives its mTLS certificate. SSH is never used again after that.
- A node reports its capacity (CPU, RAM, disk) and health. It can be **drained** (no new boxes) or removed.
- Supported OS: Ubuntu 22.04/24.04 and Debian 12, amd64 and arm64.

## 3. Box lifecycle
States: `provisioning → ready ⇄ idle → stopping → stopped → resuming → ready`, plus `error` and `deleting`.
- **Sizes, same as boat:** `small` 2 vCPU/4 GB/12 GB, `default` 4/8/50, `large` 8/16/125, `xlarge` 16/32/251. Limits are Docker `--cpus`, `--memory`, `--pids-limit`. Disk uses `--storage-opt size=` when the node's filesystem supports it (xfs pquota); otherwise it's enforced as a cap on snapshot size.
- **TTL:** defaults to 1h, like boat. When it expires, the box stops (and is snapshotted). `--ttl 0` disables it.
- **Scheduler:** places the box on a healthy, non-drained node with enough CPU and memory admission capacity, unless `--node` is given.
- **Idempotency:** `Idempotency-Key` header on create and fork.
- **Reconcile loop** (from Orbit): the server compares desired state with what each agent reports. Orphan containers are reported, never deleted automatically.

## 4. Snapshots, resume, fork, templates
Same model as boat: **the filesystem only.** Memory, processes, and Docker-in-box's `/var/lib/docker` are not captured. Resume behaves like a reboot: enabled systemd services start again.
- **Capture:** the agent uses `docker diff` to select changed/added paths and record deletions, and also fully captures image-declared volumes (including `/config`). It creates a tar preserving numeric ownership, xattrs, and ACLs, then streams it to `restic backup --stdin`. Restic remains chunked, content-addressed, deduplicated, compressed, and incremental. It captures every minute while the box is `ready`/`idle` (configurable) and once more on stop. `.boxignore` rules are applied along with system excludes.
- **Stop never loses data:** if the final capture fails, the stop is refused, the box stays running, billing/TTL pauses, and the user is notified.
- **Snapshot storage:** any S3-compatible bucket (R2, B2, Hetzner, AWS, self-hosted MinIO). A single-node install can use a local-disk restic repo instead.
- **Restore and resume:** `docker create` a fresh container from the pinned image, then stream `restic dump` into `docker cp -a` before start. The image's `/custom-cont-init.d/00-boxes-restore` applies recorded deletions. Every resume creates a fresh container, so same-node and cross-node resume use the same flow.
- **Fork:** take a fresh capture of the source, then resume it as a new box. The source keeps running.
- **Named snapshots (templates):** `boxes snapshot <id> <name>` pins a snapshot, and `boxes new --from <name>` starts a box from it.
- **Pull:** `boxes snapshot pull <id>` runs `restic restore` to the laptop. The API also exposes the file tree and single-file download from a snapshot.
- **Not in v1:** lazy streaming restore (v1 restores fully before start) and memory snapshots.
- **Scheduler admission:** allocated box memory plus the requested `memoryBytes` must be at most node `memoryBytes * --memory-overcommit` (default `1.5`).

**M0 status:** passed on Sysbox on a real VPS using overlay upper-dir capture, including package installation, system-file deletion, and ownership preservation. The host requires `kernel.apparmor_restrict_unprivileged_userns=0` for the Sysbox runtime.

## 5. Desktop and computer use
- **Stream:** Selkies (already in the Webtop base) in **WebSocket mode** at 1920×1080. It runs over plain HTTPS through Traefik, with no TURN or UDP. WebRTC mode (boat's 60 fps Moonlight equivalent) comes later.
- **Access:** `boxes desktop <id>` / `POST /sandboxes/{id}/desktop` returns a short-lived URL containing a secret. Traefik on the node checks the token with a forward-auth call to the agent, and the agent injects the Selkies basic-auth header (from Orbit's gateway). Tokens and URLs are never logged. `--public` makes a longer-lived shareable link.
- **Browser-only mode:** `boxes browser <id> [--profile dir]` streams a kiosk Chrome window with a chosen user data dir, like boat.
- **Computer use on every harness:** `boxd` includes `boxctl` (a Go port of Orbit's `orbitctl`: xdotool + scrot) and exposes it as an **MCP server** (`boxd mcp`) that every harness is preconfigured to load. Tools: `screenshot`, `click`, `move`, `drag`, `scroll`, `type`, `key`, `open_url`, `launch_app`, `list_windows`. The desktop is the same `DISPLAY=:0` the user watches.

## 6. Agent prompting (`boxd`)
- `boxd` is an always-on s6 service in the box. A **conversation** has one harness process and one history. Many conversations run in parallel. Harnesses run as `user` in `/home/user`, reading ordinary config files.
- **Harness adapters (v1):**

  | Harness | How it runs |
  |---|---|
  | `claude` | `claude -p --input-format stream-json --output-format stream-json` |
  | `codex` | `codex app-server` over JSON-RPC (Orbit's adapter) |
  | `opencode` | `opencode serve` HTTP + SSE |

  pi, Kimi, and others come later.
- **Normalized events:** `prompt`, `message.delta`, `message`, `tool.call`, `tool.result`, `turn.started`, `turn.completed`, `turn.failed`, `lifecycle`. Every event carries `conversationId` and a monotonically increasing cursor.
- **Event storage:** `boxd` writes to a local outbox (SQLite in `/var/lib/boxd`) → agent → server. The server acknowledges by cursor. SQLite on the server is the source of truth, so `boxes events` works even when the box is stopped.
- **Prompt, steer, interrupt:** `POST /prompt` (with `--provider`, `--model`, `--conversation`, or `--new`) queues a run with first-class status. `steer` sends to the running turn where the harness supports it, and otherwise queues the message as the next prompt. `interrupt` stops all conversations or one.
- **Sessions survive** stop/resume/fork because harness session files live on disk and are captured in snapshots.
- **Keys:** API keys and subscription auth files come from the box's environment (section 7). `--no-env` boxes get none.
- **Bring your own harness:** anything reachable through `ssh`/`exec` works. `boxd` doesn't get in the way.

## 7. Environments, secrets, repos
- **Environment** = env vars + secret files + git repos + a setup script, with **immutable versions**, like boat. A box is pinned to a version, and `upgrade` repoints running boxes and hot-pushes the new config.
- **Secrets:** encrypted at rest in SQLite with AES-GCM under a master key in `/etc/boxes/master.key`. Delivered by the agent into `/run/boxes` (tmpfs, 0600) and never passed in Docker argv (from Orbit). Redacted from logs.
- **Repos:** v1 supports a git URL plus a token secret, cloned by `boxd` into `/home/user/<repo>` on first boot. A GitHub App integration comes later.
- **Setup script:** runs once on first boot. Output goes to the event stream.

## 8. Networking and hosting
- **Box network:** each node has one bridge network `boxes-net` with inter-container traffic disabled (`enable_icc=false`), plus iptables drops to cloud metadata (169.254.169.254), the agent's port, and the Docker host. Outbound internet is allowed.
- **Hostnames:** each node has a base host: a custom wildcard (`*.n1.example.com`) or zero-config `<ip>.sslip.io`. Let's Encrypt HTTP-01 through the node's Traefik.
- **`boxes host <id> <port>`:** a stable HTTPS URL `https://<box>-<port>.<node-host>`. The app must bind to `0.0.0.0` (same warning as boat). Public, or token-protected with `--private`.
- **SSH:** `boxes ssh <id>` tunnels through the agent's reverse tunnel (`ProxyCommand boxes proxy %h`), so boxes don't need a public port each. `boxes scp` uses the same tunnel. `POST /ssh-key` adds a key for the user `user`.

## 9. API, CLI, SDK, UI
- **REST API** at `/api/v1`, with the same resource shape as boat: `sandboxes`, `snapshots`, `named-snapshots`, `environments`, `prompt`, `events`, `conversations`, `commands`, `files`, `desktop`, `ssh-key`, `webhooks`, `api-keys`, `limits`. The OpenAPI spec is generated from Go and published.
- **CLI `boxes`:** `login, new, list, get, ssh, scp, exec, prompt, events, steer, interrupt, desktop, browser, host, stop, resume, fork, snapshot, delete, env, node`. `--json` gives stable JSONL, like boat.
- **SDKs:** TypeScript and Python, generated from OpenAPI.
- **Webhooks:** `ready`, `restored`, `error`, `stopped`, `deleted`, signed with HMAC-SHA256.
- **Auth:** email + password users, roles `owner`/`member`, API keys that are scoped and expiring (hashed at rest). Organizations and SSO come later.
- **UI pages:** Sandboxes (list, create, detail with Terminal / Desktop / Conversations / Snapshots / Ports tabs), Templates, Environments, Nodes, API keys, Webhooks, Settings (S3, domains).

## 10. Errors and recovery
- The agent reconnects with exponential backoff. While a node is offline its boxes show `unreachable`. The owner can **resume a box elsewhere from its last snapshot**. That **fences** the old copy: on reconnect, the agent stops any container whose box has moved, which prevents split brain.
- Every mutating agent operation is idempotent and keyed by operation id, so retries after a lost response are safe.
- Snapshot failures pause billing/TTL, emit a webhook, and retry. They never cause a lost stop.
- A failed box create cleans up its container and network on that node and reports `error` with a sanitized reason.

## 11. Testing
- **Go unit tests** for scheduler, state machine, event cursoring, secret redaction, and harness event normalization, using a fake Docker/agent.
- **Integration suite** (`make e2e`) against a real VPS pair: M0 snapshot round-trip, create/exec/stop/resume/fork, cross-node resume, desktop token auth, `host` URL, and a prompt on each harness.
- **UI:** Playwright smoke tests for create box → open desktop → prompt → stop.
- **CI:** `go vet`, `golangci-lint`, `go test ./...`, `next lint`, `next build`, box image build.

## 12. Repo layout
```
cmd/boxes-server  cmd/boxes-agent  cmd/boxd  cmd/boxes
internal/{api,auth,scheduler,store,agentproto,snapshot,harness,computer,netpolicy,secrets}
web/              Next.js (static export, embedded)
images/box/       box Dockerfile (from orbit-webtop)
install/          install.sh, node-install.sh
docs/
```

## 13. Milestones
| # | Milestone | Done when |
|---|---|---|
| M0 | Snapshot spike | **Passed on Sysbox** on a real VPS using overlay upper-dir capture |
| M1 | Single node core | install.sh; `new/list/exec/ssh/stop/delete` via CLI + API + UI |
| M2 | Desktop + computer use | Desktop URL works; MCP computer tools drive it |
| M3 | Agent prompting | `prompt/events/steer/interrupt` on claude, codex, opencode |
| M4 | Snapshots | Per-minute capture, stop/resume, fork, templates, pull |
| M5 | Multi-node | Add node, scheduler, cross-node resume, fencing, `host` URLs |
| M6 | Platform polish | Environments/secrets/repos, API keys, webhooks, SDKs, docs site |

## Out of v1 (explicitly)
Billing, organizations, SSO, GPU, WebRTC 60 fps stream, lazy restore, memory snapshots, Kata/VM runtime, GitHub App, pi/Kimi/Prime harnesses, autoscaling nodes through provider APIs.

## Local development
The Docker Desktop VM has no Sysbox, so the runtime is configurable (`BOXES_RUNTIME=runc` in dev, `sysbox-runc` in prod). In dev the agent runs as a privileged container with `/var/lib/docker` and the Docker socket mounted, so it can reach overlay `UpperDir`s inside the VM.
