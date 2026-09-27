# boxes-oss

boxes-oss is an open-source, self-hosted platform for running and managing AI agent machines on your own VPSes. It is designed around a Go server, node agents, Docker/Sysbox boxes, snapshots, a web UI, and a CLI.

## Status

### Verified (2026-09-24)

A fresh DigitalOcean Ubuntu 24.04 droplet (4 vCPU/8 GB) was installed with `install/install.sh` and Sysbox 0.7.1. Verification confirmed Let's Encrypt issuance via `<ip>.sslip.io`, survival across a host reboot, and a successful installer re-run. `scripts/e2e-remote.sh` passed checks for box lifecycle; Docker-in-box; snapshots, including system changes across stop/resume/fork; desktop TLS and token authentication; public host URLs; SSH through `boxes proxy`; a real OpenCode prompt with a computer-use screenshot; and X server health.

`scripts/e2e-local.sh` also verifies box lifecycle, exec/files, snapshot behavior (including `/config` and system changes surviving stop/resume; `/config` also survives fork), and desktop access through Traefik with token authentication. `scripts/e2e-agents.sh` verifies the web UI, CLI, computer-use MCP tools, and—when credentials are supplied—a real OpenCode prompt.

### Verified (2026-09-25)

- DigitalOcean autoscaling: scaling up from a full control plane creates a droplet with cloud-init that installs and joins the cluster in about 3 minutes; the requested box is ready in about 5 minutes.
- Idle scale-down destroys the extra droplet about 3.5 minutes after its last box stops.
- Resuming a box on a second, fresh droplet restores both user and system data from Spaces. Failed boots are cleaned up after the configured boot timeout.
- Option A image: 5.78 GB; eight idle boxes use approximately 2.8 GB of host RAM on an 8 GB droplet.
- Prebuilt node images (bake) verified on DigitalOcean 2026-09-25: bake 7.75 min; cold start on a new droplet from the baked image 2 min 29 s (was ~5 min); box on an already-running node ready in ~5 s (scale-ahead headroom / warm schedule keep one ready).
- S3/Spaces snapshot storage, the built-in image registry, and safe `install.sh --upgrade` behavior are verified.

### Not yet verified

- Claude Code and Codex harnesses against real accounts
- Hetzner and script providers with live nodes
- arm64 nodes

See [the design](docs/plans/2026-09-24-boxes-oss-design.md) and [implementation plan](docs/plans/2026-09-24-boxes-oss-implementation.md) for architecture and roadmap.

SDKs: [TypeScript](sdk/typescript/README.md) · [Python](sdk/python/README.md).

## Development

Prerequisites: Go, Docker, `curl`, `jq`, and (for the agents E2E) Node.js and `pnpm`. Build the local box image:

```sh
docker build -f images/box/Dockerfile -t boxes/box:dev .
```

Run the local lifecycle and snapshot suite, then the UI/CLI/MCP suite:

```sh
scripts/e2e-local.sh
scripts/e2e-agents.sh
```

`E2E_OPENCODE_AUTH_FILE` is optional; set it to a non-empty OpenCode `auth.json` to run the real OpenCode prompt check.

Run the live deployment E2E against an already-installed HTTPS deployment with Sysbox. It creates a small sandbox and fork, exercises snapshot restoration, desktop and host TLS routes, Docker-in-box, and SSH, then deletes the sandboxes it created:

```sh
BOXES_URL=https://boxes.example.com \
BOXES_EMAIL=you@example.com \
BOXES_PASSWORD_FILE=/secure/path/to/password \
scripts/e2e-remote.sh
```

`BOXES_PASSWORD` can be used instead of `BOXES_PASSWORD_FILE`. Optionally set `E2E_OPENCODE_AUTH_FILE` to test a real OpenCode prompt and `BOXES_SSH_HOST=root@<ip>` to run the 60-second Xvfb/container-log health check over SSH. The script requires `curl`, `jq`, Go, `ssh`, and `ssh-keygen` on the host.

On Windows, install the CLI from a PowerShell prompt:

```powershell
irm https://<server>/install/cli.ps1 | iex
```

The project gate is:

```sh
go build ./... && go vet ./... && go test ./...
```

Build the CLI binaries for all supported platforms with `make cli-binaries`; outputs go to `dist/cli/`.
