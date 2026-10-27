# Paddock

Website: https://tony1908.github.io/paddock/

Paddock gives your AI agents computers and sandboxes on a VPS you own. Each agent gets a full Linux box: an XFCE desktop, sudo, Docker inside the box (via Sysbox), SSH, and preinstalled coding-agent tooling. Use a box as a long-lived computer the agent keeps working in, or as a locked-down sandbox for one task that you delete afterwards. Boxes can be created, stopped with a snapshot, resumed, forked, and deleted through a web UI, a CLI, or the HTTP API.

## Features

- Full Ubuntu boxes with a web desktop, SSH access, and Docker-in-box (Sysbox).
- Box sizes from `xsmall` (1 CPU, 2 GiB) to `xlarge` (16 CPU, 32 GiB).
- Auto-stop timers (`--ttl`, `--no-auto-stop`) and idle stop (`--idle-stop`).
- Filesystem snapshots taken periodically and on stop, with `resume`, `fork`, and `restore` (rollback). Retention per box with `--keep-snapshots`.
- Named snapshots, snapshot download, and a local or S3-compatible restic repository.
- Agent prompts (`paddock prompt`) for claude, codex, opencode, pi, prime, and kimi,
  with steering, interrupt, event streaming, and optional stop-when-done.
- Desktop sessions, GUI apps on the box desktop (`exec --desktop`), screenshots (`paddock screenshot`), public or private host ports (`paddock host`), and local port
  forwarding (`paddock forward`).
- File push/pull, command execution, and detached processes with live logs, stdin, and kill (`exec --detach`, `ps`, `logs -f`, `kill`).
- Box tags (`--tag`), filterable with `paddock list --tag`.
- Outbound network policy (`allow-all`, `deny-all`, or a domain/CIDR allowlist), changeable on a running box. DNS goes through the agent, and cloud metadata is always blocked.
- Git repository or tarball sources extracted on first boot (`--git`, `--tarball`).
- `--help` on every command, and `paddock help <command>`.
- Named drives that persist across boxes and mount into them (`paddock drive`, `--mount`).
- Isolated Linux users and shared groups inside a box (`paddock user`, `paddock group`).
- Versioned environments (named documents applied at box creation), webhooks, and API keys.
- Per-box container image (`--image`), activity log, per-box usage totals, and optional cost estimates from hourly pricing.
- Owner and user accounts, with owners creating later accounts.

## Requirements

- Ubuntu or Debian VPS, `amd64` or `arm64`, with root access.
- Ports **80** and **443** open to the internet (Traefik serves HTTPS and the
  Let's Encrypt HTTP-01 challenge on port 80).
- A DNS name that points at the VPS for `--domain`: your own domain, or a sslip.io
  name such as `paddock.203.0.113.10.sslip.io`.
- CPU and RAM sized for the boxes you run. The installer sets no hard minimum.
  The server admits a box only with headroom of the larger of 15% of RAM or 1 GiB,
  and assumes at least 300 MiB per box. If the host has no swap, the installer
  adds up to 8 GiB.

The installer installs Docker, Sysbox, restic, and Traefik. See
[docs/self-hosting.md](docs/self-hosting.md#requirements) for details.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/tony1908/paddock/main/install/install.sh \
  | sudo bash -s -- --domain paddock.example.com --email ops@example.com
```

`--domain` and `--email` are required. When the installer finishes it prints a
one-time owner setup link:

```
Owner setup link (one time): https://paddock.example.com/signup/?setup=<token>
```

Open the link, create the owner account (password of at least 12 characters),
then sign in at `https://paddock.example.com/login/`. The setup token is also in
`/var/lib/paddock/admin.token`. Full flags and first-login steps are in
[docs/self-hosting.md](docs/self-hosting.md#install).

## Install the CLI

The server publishes the CLI installers at `/install/cli.sh` and `/install/cli.ps1`.

macOS and Linux:

```sh
curl -fsSL https://paddock.example.com/install/cli.sh | sh
```

Windows (PowerShell):

```powershell
irm https://paddock.example.com/install/cli.ps1 | iex
```

Or download a release binary from [GitHub Releases](https://github.com/tony1908/paddock/releases/latest). On Apple silicon, for example:

```sh
curl -fsSL https://github.com/tony1908/paddock/releases/download/v0.2.0/paddock_0.2.0_darwin_arm64.tar.gz | tar xz paddock
sudo mv paddock /usr/local/bin/
```

Then log in with the owner account, or with an API key from the dashboard's API keys page:

```sh
paddock login --server https://paddock.example.com --email you@example.com
paddock login --server https://paddock.example.com --api-key <key>
```

## Install the agent skill

`paddock skill install` writes the Paddock skill to `~/.claude/skills/paddock/SKILL.md` (Claude Code, and OpenCode) and `~/.codex/skills/paddock/SKILL.md` (Codex). Use `--claude`, `--codex`, or `--force` to limit the install or overwrite a changed copy. Run it again after upgrading the CLI.

```sh
paddock skill install
```

To install manually from the source file:

```sh
mkdir -p ~/.claude/skills/paddock && curl -fsSL https://raw.githubusercontent.com/tony1908/paddock/main/internal/cli/skill.md -o ~/.claude/skills/paddock/SKILL.md
```

Then ask your agent, for example: "Run the tests in a Paddock box".

## CLI quickstart

```sh
paddock new --type small --ttl 3600        # create a box (it becomes "current")
paddock exec current -- uname -a           # run a command
paddock push current ./project --to /config/project
paddock desktop current                    # open the desktop session
paddock prompt current "summarize the repo" --provider claude
paddock stop current                       # snapshot, then stop
paddock resume current                     # restore from the latest snapshot
paddock fork current --ttl 1800            # new box from a fresh snapshot of this one
paddock delete current --yes
```

The full command reference is in [docs/cli.md](docs/cli.md).

## Upgrade

On an existing server, refresh the server, agent, and CLI binaries. Your
`/etc/paddock` settings are kept:

```sh
sudo bash install/install.sh --upgrade
```

or, through the installer URL:

```sh
curl -fsSL https://raw.githubusercontent.com/tony1908/paddock/main/install/install.sh \
  | sudo bash -s -- --upgrade
```

Sysbox version changes restart Docker and need `--allow-runtime-restart`; see
[docs/self-hosting.md](docs/self-hosting.md#upgrading).

## Development

Build and check the Go code:

```sh
go build ./...
go vet ./...
go test ./...
```

Build the web UI (it is embedded into the server through `make web`):

```sh
pnpm --dir web build
```

Build the box and tools images:

```sh
docker build -f images/box/Dockerfile -t paddock/box:dev .
docker build -f images/tools/Dockerfile -t paddock/tools:dev .
```

Run the end-to-end scripts (they start a local stack with Docker):

- `scripts/e2e-local.sh`: creates a box, runs commands, checks stop, resume,
  fork, and desktop routing. Requires `docker`, `go`, `jq`, and `curl`.
- `scripts/e2e-agents.sh`: builds the web UI and runs the agent-facing checks
  against the local stack.
- `scripts/e2e-remote.sh`: runs the same checks against a live server. Set
  `PADDOCK_URL` (HTTPS), `PADDOCK_EMAIL`, and `PADDOCK_PASSWORD` or `PADDOCK_PASSWORD_FILE`.
  `PADDOCK_SSH_HOST=root@<host>` is optional.

Release binaries and the CLI matrix are built with `make cli-binaries` and
GoReleaser (`.goreleaser.yaml`).

## SDKs

- [TypeScript SDK](sdk/typescript/README.md)
- [Python SDK](sdk/python/README.md)

## Docs

- [HTTP API](docs/api.md): routes, commands, drives, users and groups, network policy, sources, tags, activity, usage, and pricing.
- [CLI](docs/cli.md): authentication, commands, and environment overrides.
- [Desktop sessions](docs/desktop.md): session lifetime, storage, and forward-auth.
- [Lifecycle](docs/lifecycle.md): states, auto-stop timers, prompts, and CLI installation.
- [Snapshots](docs/snapshots.md): capture, restore, and repositories.
- [Self-hosting](docs/self-hosting.md): requirements, install, upgrades, storage, DNS forwarder, and capacity.

Landing page: [site/index.html](site/index.html).

## License

MIT. See [LICENSE](LICENSE).
