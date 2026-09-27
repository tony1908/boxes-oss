# Sandbox lifecycle API

The server supports timed task boxes and always-on boxes through the same sandbox
API. Authenticated routes are mounted below `/api/v1`.

## Auto-stop timers

`POST /sandboxes` defaults `ttlSeconds` to 3600 when omitted. Send `null` to
disable auto-stop; integer values must be at least 60 seconds, and values above
2592000 seconds are capped. For example:

```json
{"type":"small","ttlSeconds":null}
```

`PATCH /sandboxes/{id}` extends from the current request time. An integer sets
`expiresAt = now + ttlSeconds`; `null` clears the expiration. Leaving the field
out does not change the timer. Resume also restarts the timer from resume time;
omitted TTL preserves the current setting. Fork defaults to one hour when its
TTL is omitted.

`GET /sandboxes?filter=rse` combines lifecycle state groups: `r` includes
provisioning, ready, idle, and resuming; `s` includes stopping and stopped; `e`
includes error and unreachable. An omitted filter returns every sandbox.

Sandbox responses include `autoStop`, nullable `ttlSeconds`, `expiresAt`,
`ttlRemainingSeconds`, `lastSnapshotAt`, `desktopAvailable`, accounted
`runningSeconds`, and `setupStatus` (`none`, `running`, `succeeded`, or `failed`).
`GET /sandboxes/{id}/usage` returns `runningSeconds` and `since`, the start of
the active run interval or null when stopped.

## Setup and prompts

Creation accepts up to 100 `env` variables with a combined 64 KiB key/value
limit and a `setupScript` up to 64 KiB. The server merges them into the existing
`/run/boxes/env` and `/run/boxes/setup.json` files. boxd runs setup in the
background once, persists `.boxd/setup.status`, and emits setup-status events
that the server stores for sandbox responses.

`POST /sandboxes/{id}/prompt` accepts `stopWhenDone`. When set, the server
reuses the run terminal events and snapshots/stops the sandbox once a run
completes, fails, or is interrupted. A prompt sent to a stopped sandbox resumes
it first.

`GET /sandboxes/{id}/tcp/{port}` is a WebSocket tunnel to any TCP port in the
sandbox.

## CLI installation

The server publishes `GET /install/cli.sh` and `GET /install/cli.ps1`, and serves binaries from
`<data-dir>/cli/boxes-<os>-<arch>`. Supported names are `boxes-darwin-arm64`,
`boxes-darwin-amd64`, `boxes-linux-amd64`, `boxes-linux-arm64`,
`boxes-windows-amd64.exe`, and `boxes-windows-arm64.exe`. Missing
binary responses identify the expected installation directory. The install
script downloads the matching binary into `~/.local/bin/boxes` (or
`/usr/local/bin/boxes` when writable), then prints `boxes login --server <url>
--key-stdin` and `boxes skill install` as next steps.

The self-host installer copies any matching CLI binaries found in
`BOXES_BINARIES_DIR` into `/var/lib/boxes/cli/`.
