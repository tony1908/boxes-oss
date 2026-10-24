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
| Sandboxes | `GET`/`POST /sandboxes`, `GET`/`PATCH`/`DELETE /sandboxes/{id}`, `POST /sandboxes/{id}/stop`, `POST /sandboxes/{id}/resume`, `POST /sandboxes/{id}/fork` |
| Commands and files | `POST /sandboxes/{id}/commands`, `GET /sandboxes/{id}/commands/{pid}`, `GET`/`PUT /sandboxes/{id}/files`, `PUT /sandboxes/{id}/archive`, `GET /sandboxes/{id}/artifacts` |
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

## Sandbox tags

Sandboxes accept up to 10 key/value `tags`. Keys must match `^[a-z0-9][a-z0-9_.-]{0,62}$`; values are at most 255 characters with no control characters. Invalid tags return `400 invalid_tags`.

- `POST /sandboxes` takes `tags` in the body.
- `PATCH /sandboxes/{id}` with `tags` replaces the whole map; `{}` clears all tags; omitting `tags` leaves them unchanged.
- `GET /sandboxes?tag=key=value` returns sandboxes whose tag equals the value; `?tag=key` matches any value of that key. Repeat `tag` to require several matches (AND).
- `POST /sandboxes/{id}/fork` copies the source sandbox's tags unless the body sets `tags`.

```sh
curl -X PATCH "$BOXES/api/v1/sandboxes/$ID" -H "Authorization: Bearer $KEY" \
  -d '{"tags":{"team":"agents"}}'
curl "$BOXES/api/v1/sandboxes?tag=team=agents&tag=owner" -H "Authorization: Bearer $KEY"
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
