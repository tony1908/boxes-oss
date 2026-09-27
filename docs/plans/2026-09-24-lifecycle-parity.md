# Lifecycle + boat quickstart parity

Goal: manage boxes exactly like https://docs.boat.dev/quickstart and /long-running-tasks: on for a task (auto-stop timer) or always on (no auto-stop). The goal is feature parity on self-hosted servers, not boat's billing or orgs.

## API contract (server)
- `POST /sandboxes` body: `type`, `ttlSeconds` (omitted → 3600; `null` → no auto-stop; integer 60..2592000, larger values are capped at 2592000), `env` (map, ≤100 vars / 64 KB), `setupScript` (string ≤64 KB, run by boxd after ready in the background; status is exposed as `setupStatus`), `from` (named snapshot), `environment`, `noEnv`.
- `PATCH /sandboxes/{id}` `{ttlSeconds: int|null}` = extend: `expiresAt = now + ttl`; `null` disables auto-stop.
- `POST /sandboxes/{id}/resume` `{ttlSeconds?, type?, node?}`: omitted keeps the box's current setting. `POST /sandboxes/{id}/fork` `{ttlSeconds?}`: omitted → 3600.
- `GET /sandboxes?filter=<letters>`: `r` = provisioning, ready, idle, resuming; `s` = stopping, stopped; `e` = error, unreachable; combine letters. Omitted → all.
- Sandbox JSON adds: `autoStop` (bool), `ttlSeconds` (null when off), `expiresAt`, `ttlRemainingSeconds`, `lastSnapshotAt`, `desktopAvailable`, `runningSeconds`, `setupStatus` (none|running|succeeded|failed).
- `GET /sandboxes/{id}/usage` → `{runningSeconds, since}` (running time accounted per start/stop interval).
- `POST /sandboxes/{id}/prompt` adds `stopWhenDone` (bool). A prompt to a stopped box resumes it first; the run is queued until the box is ready. With `stopWhenDone`, the server stops (and snapshots) the box when the run reaches a terminal state.
- `GET /sandboxes/{id}/tcp/{port}` (WebSocket) for any port, for `boxes forward`.
- CLI distribution: the server serves `GET /install/cli.sh` and `GET /install/cli/boxes-<os>-<arch>` from `<data-dir>/cli/`. install.sh puts the darwin/linux amd64/arm64 CLI binaries there.

## CLI
`new [--type] [--ttl S] [--no-auto-stop] [-e K=V]... [--setup-file F] [--from NAME] [--environment NAME] [--no-env]` (saves the box as `current`) · `list [--filter r|s|e..] [--all]` (default `r`) · `info <id>` · `extend <id> --hours N | --ttl S | --no-auto-stop` · `resume <id> [--ttl|--no-auto-stop|--type|--node]` · `fork <id> [--ttl|--no-auto-stop]` · `stop` · `delete <id> [--yes]` (`--yes` is required without a TTY) · `exec <id> [--detach] [--status PID] -- cmd` · `prompt [id] "task" [--provider --model --new --conversation --stop-when-done]` · `conversations <id>` · `snapshots <id>` · `usage <id>` · `limits` · `status` · `login [--api-key K | --key-stdin]` · `logout` · `dashboard` · `forward <id> <port> [--local-port N]` · `skill install [--claude] [--codex]`. Every `<id>` accepts `current`. `--json` turns on automatically when stdout isn't a TTY.

## Web
- Create dialog: auto-stop choice: 1 hour (task, default), 4 h, 24 h, custom, Always on.
- Box page: "Auto-stops in 42m" / "Always on", with Extend +1h, Set timer and Always on actions. Also Stop, Resume, Delete (with confirm).
- List: auto-stop column, plus Running / Stopped / All filter tabs.
