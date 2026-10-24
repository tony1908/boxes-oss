# Sandbox lifecycle API

Timed task boxes and always-on boxes use the same sandbox API. Authenticated
routes are mounted below `/api/v1`.

## States

A box moves between `provisioning`, `ready`, `idle`, `stopping`, `stopped`,
`resuming`, `error`, and `unreachable`. `GET /sandboxes?filter=` groups these
states by letter:

- `r`: provisioning, ready, idle, resuming
- `s`: stopping, stopped
- `e`: error, unreachable

Letters combine (`filter=rs`). An omitted filter returns every sandbox.

## Auto-stop timers

`POST /sandboxes` defaults `ttlSeconds` to 3600 when omitted. Send `null` to
disable auto-stop:

```json
{"type":"small","ttlSeconds":null}
```

Integer values must be at least 60 seconds. Values above 2592000 seconds (30
days) are capped to 2592000. `0` also disables the timer.

`PATCH /sandboxes/{id}` extends from the current request time. An integer sets
`expiresAt = now + ttlSeconds`; `null` clears the expiration. Leaving the field
out does not change the timer.

`POST /sandboxes/{id}/resume` restarts the timer from the resume time. If
`ttlSeconds` is omitted, the box keeps its current setting. `POST
/sandboxes/{id}/fork` defaults to one hour when its TTL is omitted.

Idle stop is a separate setting. The CLI exposes it as `--idle-stop` on `new`
and `extend`.

## Sandbox response

Sandbox responses include `autoStop`, nullable `ttlSeconds`, `expiresAt`,
`ttlRemainingSeconds`, `lastSnapshotAt`, `desktopAvailable`, accounted
`runningSeconds`, and `setupStatus` (`none`, `running`, `succeeded`, or
`failed`).

`GET /sandboxes/{id}/usage` returns `runningSeconds` and `since`, the start of
the active run interval or null when stopped. See [api.md](api.md) for the full
usage response.

## Setup and environment

Creation accepts up to 100 `env` variables with a combined 64 KiB key/value
limit, and a `setupScript` up to 64 KiB. The server merges them into
`/run/boxes/env` and `/run/boxes/setup.json` inside the box. boxd runs setup in
the background once, persists `.boxd/setup.status`, and emits setup-status
events that the server stores for sandbox responses.

The CLI accepts the same values as `boxes new -e KEY=VALUE` and
`--setup-file FILE`.

## Prompts

`POST /sandboxes/{id}/prompt` accepts `stopWhenDone`. When set, the server
snapshots and stops the sandbox once the run completes, fails, or is
interrupted. A prompt sent to a stopped sandbox resumes it first.

`POST /sandboxes/{id}/steer` and `POST /sandboxes/{id}/interrupt` send a
follow-up message or cancel the active run.

## Ports

`GET /sandboxes/{id}/tcp/{port}` is a WebSocket tunnel to a TCP port inside the
box. `boxes forward` uses this tunnel to bind a local port on `127.0.0.1`.

## Snapshot-backed stop and resume

`POST /sandboxes/{id}/stop` captures a snapshot before stopping the container.
If the capture fails the box remains running and the API returns `409` with
`snapshot_failed`. `POST /sandboxes/{id}/resume` restores the box from its
latest snapshot; a box without a snapshot cannot resume. Details are in
[snapshots.md](snapshots.md).

## CLI installation

The server publishes `GET /install/cli.sh` and `GET /install/cli.ps1`, and serves
binaries from `<data-dir>/cli/` (by default `/var/lib/boxes/cli/`) at
`GET /install/cli/{binary}`. Supported names are `boxes-darwin-arm64`,
`boxes-darwin-amd64`, `boxes-linux-amd64`, `boxes-linux-arm64`,
`boxes-windows-amd64.exe`, and `boxes-windows-arm64.exe`. A missing binary
returns a `404` that names the expected directory.

The `cli.sh` script downloads the matching binary into `~/.local/bin/boxes`
(or `/usr/local/bin/boxes` when that directory is writable), then prints
`boxes login --server <url> --key-stdin` and `boxes skill install` as next steps.

The self-host installer copies any matching CLI binaries found in
`BOXES_BINARIES_DIR` into `/var/lib/boxes/cli/`.
