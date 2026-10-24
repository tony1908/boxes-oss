# HTTP API

Authenticated routes are mounted below `/api/v1` and accept a bearer token
(a login session token or an API key) in the `Authorization` header. Owners
can create API keys in the web UI or with `POST /api/v1/api-keys`.

## Route overview

The table lists the main routes registered in `internal/server/server.go`.
Every route except the public ones below requires authentication.

| Area | Routes |
| --- | --- |
| Auth | `GET /auth/status`, `POST /auth/signup`, `POST /auth/login`, `POST /auth/logout`, `GET /me` |
| API keys | `GET`/`POST /api-keys`, `DELETE /api-keys/{id}`, `GET /api-keys/{id}/usage` |
| Machine | `GET /machine`, `GET /limits` |
| Sandboxes | `GET`/`POST /sandboxes`, `GET`/`PATCH`/`DELETE /sandboxes/{id}`, `POST /sandboxes/{id}/stop`, `POST /sandboxes/{id}/resume`, `POST /sandboxes/{id}/restore`, `POST /sandboxes/{id}/fork` |
| Commands and files | `POST /sandboxes/{id}/commands`, `GET /sandboxes/{id}/commands`, `GET /sandboxes/{id}/commands/{pid}`, `GET /sandboxes/{id}/commands/{pid}/logs`, `POST /sandboxes/{id}/commands/{pid}/kill`, `POST /sandboxes/{id}/commands/{pid}/stdin`, `GET`/`PUT /sandboxes/{id}/files`, `PUT /sandboxes/{id}/archive`, `GET /sandboxes/{id}/artifacts` |
| Agent prompts | `POST /sandboxes/{id}/prompt`, `POST /sandboxes/{id}/steer`, `POST /sandboxes/{id}/interrupt`, `GET /sandboxes/{id}/conversations`, `GET /sandboxes/{id}/runs/{runId}`, `GET /sandboxes/{id}/events` |
| Snapshots | `GET /sandboxes/{id}/snapshots`, `GET /sandboxes/{id}/snapshots/latest`, `DELETE /sandboxes/{id}/snapshots/{snapshotId}`, `GET /snapshots`, `GET /snapshots/{id}/download`, `GET /snapshots/{id}/tree`, `GET /snapshots/{id}/file`, `GET`/`POST /named-snapshots`, `GET`/`DELETE /named-snapshots/{name}` |
| Access | `POST /sandboxes/{id}/desktop`, `GET /sandboxes/{id}/desktop`, `GET`/`POST /sandboxes/{id}/hosts`, `DELETE /sandboxes/{id}/hosts/{port}`, `POST /sandboxes/{id}/ssh-key`, `GET /sandboxes/{id}/tcp/{port}` (WebSocket), `/sandboxes/{id}/boxd/{path...}` |
| Usage and activity | `GET /sandboxes/{id}/usage`, `GET /sandboxes/{id}/activity`, `GET`/`PUT /settings/pricing`, `GET`/`PUT /settings/retention` |
| Environments | `GET`/`POST /environments`, `GET`/`PATCH`/`DELETE /environments/{id}`, `POST /environments/{id}/upgrade` |
| Webhooks | `GET`/`POST /webhooks`, `PATCH`/`DELETE /webhooks/{id}`, `POST /webhooks/{id}/rotate-secret` |
| Settings | `GET /settings/storage`, `PUT /settings/images`, `GET /provider-models` |

Public routes (no bearer token):

- `GET /api/v1/agent/connect` and `GET /agent/connect`: WebSocket endpoint used by the local `boxes-agent`.
- `GET /install/cli.sh`, `GET /install/cli.ps1`, `GET /install/cli/{binary}`, `GET /install/traefik.yml`: installer and CLI downloads.
- `POST /admin/storage`, `POST /admin/runtime/stop`, `POST /admin/runtime/resume`: local administration endpoints used by `boxes-server storage set` and the installer's `--upgrade --allow-runtime-restart` path. They require the admin token from `/var/lib/boxes/admin.token` and accept only loopback requests.

## Detached commands

`POST /sandboxes/{id}/commands` with `detached: true` returns `202` and a `processId`. Detached commands allow `timeoutSeconds` up to 86400 (default 3600); sync commands allow 600. Add `stdin: true` to write to stdin later.

- Output: `GET .../commands/{pid}` returns output so far. Each stream keeps its last 1 MiB; `stdoutOffset`/`stderrOffset` count all bytes written.
- Logs: `GET .../commands/{pid}/logs?stdoutOffset=N&stderrOffset=N[&follow=1]` streams NDJSON lines `{stream,data,offset}`. With `follow=1` it ends with `{exit:true,exitCode:N}` when the command exits.
- Kill: `POST .../commands/{pid}/kill` with `{signal:TERM}` (TERM, KILL, INT, HUP, QUIT, USR1, USR2). Returns `409` if the command is not running.
- Stdin: `POST .../commands/{pid}/stdin` with a raw body (at most 1 MiB); `?close=1` closes stdin after the write. Returns `409` without stdin or after close, and `504` if the write does not drain within 30 seconds.
- List: `GET /sandboxes/{id}/commands` returns `{commands: [...]}`, oldest first. Finished commands are kept for one hour, up to 100 per sandbox. The agent keeps this state in memory, so it is lost on agent restart.

## Sandbox tags

Sandboxes accept up to 10 key/value `tags`. Keys must match `^[a-z0-9][a-z0-9_.-]{0,62}$`; values are at most 255 characters with no control characters. Invalid tags return `400 invalid_tags`.

- `POST /sandboxes` takes `tags` in the body.
- `POST /sandboxes` takes `source` to place project files on first boot: `{"type":"git","url":"https://...","revision":"main","depth":1,"token":"...","path":"app"}` or `{"type":"tarball","url":"https://...","path":"app"}`. The source runs once, before `setupScript`; `path` defaults to `project` under `/config`. Only https URLs are accepted (no `git@`/ssh URLs or embedded credentials). `token` is for private git repositories and is never returned or stored; the response reports `source` without it. Tarballs must be `.tar.gz`. Invalid sources return `400 invalid_source`; `POST /sandboxes/{id}/fork` rejects `source` because the fork already has the files.
- `PATCH /sandboxes/{id}` with `tags` replaces the whole map; `{}` clears all tags; omitting `tags` leaves them unchanged.
- `GET /sandboxes?tag=key=value` returns sandboxes whose tag equals the value; `?tag=key` matches any value of that key. Repeat `tag` to require several matches (AND).
- `POST /sandboxes/{id}/fork` copies the source sandbox's tags unless the body sets `tags`.

```sh
curl -X PATCH "$BOXES/api/v1/sandboxes/$ID" -H "Authorization: Bearer $KEY" \
  -d '{"tags":{"team":"agents"}}'
curl "$BOXES/api/v1/sandboxes?tag=team=agents&tag=owner" -H "Authorization: Bearer $KEY"
```

## Outbound network policy

`networkPolicy` controls where a box can send traffic. It is accepted by `POST /sandboxes` (and copied by `POST /sandboxes/{id}/fork` unless the body sets its own). Invalid policies return `400 invalid_network_policy`.

- `mode`: `allow-all` (the default), `deny-all` (no new outbound connections; existing ones may finish), or `allowlist` (only `allowDomains` and `allowCidrs`).
- `denyCidrs` win over every other rule in every mode. `169.254.169.254/32` (cloud metadata) is always blocked.
- `POST /sandboxes/{id}/restore` with `{"snapshotId": "..."}` rolls the sandbox back to one of its own snapshots (`404 snapshot_not_found` otherwise). A running sandbox is stopped, which captures a final snapshot so the rollback can be undone, and then resumed from the chosen snapshot. A stopped sandbox records the snapshot for its next resume. Busy states (`provisioning`, `stopping`, `resuming`, `deleting`) return `409 sandbox_busy`.
- `keepSnapshots` on `POST /sandboxes` and `PATCH /sandboxes/{id}` sets how many automatic snapshots are kept, from 1 to 50; `0` or omitted uses the default of 5. Values outside that range return `400 invalid_keep_snapshots`. Forks copy the source value unless one is given. Named snapshots are never pruned.
- `PATCH /sandboxes/{id}` with `networkPolicy` replaces the policy; `null` clears it. A running box is updated immediately; a stopped box stores the policy and applies it when it next starts or resumes. If the node cannot apply a running box's policy, the request returns `502 network_policy_failed`, the stored policy stays unchanged, and the box is stopped so it is never left unfiltered.
- Limits: at most 64 entries per list; `allowDomains` entries are hostnames or `*.example.com`; CIDRs must be IPv4.

Ceilings of the allowlist, so plan around them:

- It is DNS-based. Domains are resolved on the node and the IPv4 addresses are re-resolved every 60 seconds. A CDN that rotates addresses faster than that can break connections.
- `*.example.com` allows only `example.com`. Wildcard subdomains cannot be enumerated without an SNI proxy.
- IPv4 only. Boxes use an IPv4 bridge network, so IPv6 is rejected rather than filtered.
- DNS lookups (UDP and TCP port 53) are not blocked in allowlist mode, so a box can still resolve any name. The rules control destination addresses, not names.

```sh
curl -X POST "$BOXES/api/v1/sandboxes" -H "Authorization: Bearer $KEY" \
  -d '{"networkPolicy":{"mode":"allowlist","allowDomains":["api.github.com","*.npmjs.org"],"denyCidrs":["10.0.0.0/8"]}}'
curl -X PATCH "$BOXES/api/v1/sandboxes/$ID" -H "Authorization: Bearer $KEY" \
  -d '{"networkPolicy":{"mode":"deny-all"}}'
```

## Activity

`GET /api/v1/sandboxes/{id}/activity` returns newest first. `category` is optional, `before` is the opaque `nextCursor` from the preceding page, and `limit` defaults to 50 with a maximum of 100.

```json
{
  "items": [
    {
      "id": "act_…",
      "time": "2026-09-25T12:00:00Z",
      "actorType": "user",
      "actorLabel": "owner@example.com",
      "category": "access",
      "action": "files pushed",
      "detail": {"path": "/config/app.py", "sizeBytes": 2048},
      "outcome": "ok"
    }
  ],
  "nextCursor": ""
}
```

`durationMs` and `outcome` are omitted when they do not apply. `nextCursor` is an empty string on the last page. Activity detail stores short command and prompt previews plus non-sensitive metadata; it omits environment values, tokens, credentials, file contents, and full prompts. Rows older than 90 days are pruned daily. Activity rows are kept in `box_activity` without a foreign key to the box, so they remain readable after the sandbox is deleted.

## Usage and cost

`GET /api/v1/sandboxes/{id}/usage` returns `runningSeconds`, `runningSecondsThisMonth`, `perDay` (30 UTC dates), `commandsCount`, `agentRunsCount`, `desktopSessionsCount`, `snapshotsCount`, and `since`. It includes `snapshotsBytes` and `lastSnapshotAt` when a snapshot exists, using the most recent snapshot's size. When an hourly rate is configured for the box size (falling back to the `default` rate), it also returns `estimatedCost`:

```json
{
  "runningSeconds": 1800,
  "runningSecondsThisMonth": 1800,
  "perDay": [{"date": "2026-09-25", "runningSeconds": 1800}],
  "commandsCount": 2,
  "agentRunsCount": 1,
  "desktopSessionsCount": 1,
  "snapshotsCount": 3,
  "snapshotsBytes": 123456,
  "since": null,
  "estimatedCost": {"currency": "USD", "amount": 0.25, "hourlyRate": 0.5}
}
```

Owners can read or update pricing at `GET` and `PUT /api/v1/settings/pricing`. Both require the `owner` role:

```json
{"currency":"USD","hourlyBySize":{"xsmall":0.1,"small":0.2,"default":0.5,"large":1,"xlarge":2}}
```

An empty `hourlyBySize` disables cost estimates.
