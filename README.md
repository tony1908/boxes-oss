# boxes-oss

boxes-oss is an open-source, self-hosted platform for running AI agent boxes on one VM. A Go server and its local boxes-agent use that VM’s Docker/Sysbox runtime and real memory capacity. The project includes snapshots, a web UI, and a CLI.

## Status

A single VM hosts the control plane and every box. The local agent connects through the existing server tunnel. Admission uses live memory availability, pressure, swap use, and configured host headroom. Snapshots can use local disk or S3-compatible storage such as Spaces.

The installer configures Docker, Sysbox, Traefik, TLS, swap, sysctl settings, and the local server/agent services. `install/install.sh --upgrade` updates an existing installation while preserving its configuration.

See [the design](docs/plans/2026-09-24-boxes-oss-design.md) and [implementation plan](docs/plans/2026-09-24-boxes-oss-implementation.md) for project details.

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
