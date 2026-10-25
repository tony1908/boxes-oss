# AGENTS.md

Guidance for coding agents and human contributors working in this repository.

## What this is

boxes-oss is a self-hosted sandbox platform for AI agents. Each box is a full
Linux sandbox (sudo, Docker inside, optional desktop) that runs on a single VPS
you control. This edition has no extra nodes, node management, autoscaling, or
cloud providers.

## Repository layout

- `cmd/boxes-server`: control-plane HTTP server.
- `cmd/boxes-agent`: host agent that creates and manages box containers.
- `cmd/boxes`: the `boxes` CLI.
- `cmd/boxd`: in-box daemon.
- `internal/server`: HTTP API handlers (`openapi_test.go` checks the spec).
- `internal/agent`: host-side box runtime (Sysbox, iptables, DNS forwarder).
- `internal/boxd`: in-box daemon logic.
- `internal/store`: database access; schema lives in `internal/store/migrations`.
- `internal/cli`: CLI commands; `skill.md` is the embedded agent skill.
- `internal/webui`: serves the dashboard embedded from `internal/webui/dist`.
- `web/`: dashboard source (Next.js).
- `images/box`, `images/tools`: sandbox and tool image definitions.
- `sdk/typescript`, `sdk/python`: client SDKs.
- `site/`: landing page.
- `api/openapi.yaml`: API specification.
- `docs/`: user and operator documentation.

## Checks

CI runs these; run them before opening a pull request:

```sh
go vet ./...
go test ./...
golangci-lint run ./...   # CI pins golangci-lint v2.15.0
pnpm --dir web install --frozen-lockfile && pnpm --dir web build
(cd sdk/typescript && npm test)
(cd sdk/python && python -m pytest)
```

The Go build check is `go build ./... && go vet ./... && go test ./...`.

Unit tests use fake executors, so no real Docker or iptables is needed. The
Linux-only runtime pieces are Sysbox, iptables, and the DNS forwarder.

## Conventions

- Database changes go in a new numbered migration in `internal/store/migrations`. Do not edit an applied migration.
- Every new API route must be added to `api/openapi.yaml`; `internal/server/openapi_test.go` enforces this.
- When you change CLI or API behavior, keep `docs/cli.md`, `docs/api.md`, and `internal/cli/skill.md` in sync.
- Keep pull requests small and focused on one change. Describe what changed, why, and how you verified it.

See `CONTRIBUTING.md` for the contribution process.
