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
| Drives | `GET`/`POST /drives`, `DELETE /drives/{name}`, `POST /drives/{name}/fork` |
| Users and groups | `GET`/`POST /sandboxes/{id}/users`, `DELETE /sandboxes/{id}/users/{username}`, `POST /sandboxes/{id}/groups`, `PUT`/`DELETE /sandboxes/{id}/groups/{name}/members/{username}` |
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

`POST /sandboxes/{id}/commands` with `detached: true` returns `202` and a `processId`. Detached commands allow `timeoutSeconds` up to 86400 (default 3600); sync commands allow 600. Add `stdin: true` to write to stdin later. Add `desktop: true` to run the command inside the box desktop session (`DISPLAY=:1` plus the session's bus and runtime variables) so GUI apps appear on the desktop; it runs as `abc` only and returns `409 desktop_not_running` when no session is up.

- Output: `GET .../commands/{pid}` returns output so far. Each stream keeps its last 1 MiB; `stdoutOffset`/`stderrOffset` count all bytes written.
- Logs: `GET .../commands/{pid}/logs?stdoutOffset=N&stderrOffset=N[&follow=1]` streams NDJSON lines `{stream,data,offset}`. With `follow=1` it ends with `{exit:true,exitCode:N}` when the command exits.
- Kill: `POST .../commands/{pid}/kill` with `{signal:TERM}` (TERM, KILL, INT, HUP, QUIT, USR1, USR2). Returns `409` if the command is not running.
- Stdin: `POST .../commands/{pid}/stdin` with a raw body (at most 1 MiB); `?close=1` closes stdin after the write. Returns `409` without stdin or after close, and `504` if the write does not drain within 30 seconds.
- List: `GET /sandboxes/{id}/commands` returns `{commands: [...]}`, oldest first. Finished commands are kept for one hour, up to 100 per sandbox. The agent keeps this state in memory, so it is lost on agent restart.

## Hosts and reserved ports

`POST /sandboxes/{id}/hosts` with `{"port", "private"}` exposes a box port at its own URL. Ports `3000`, `3001`, and `7070` are reserved because the box uses them for its desktop and boxd; the request returns `400 reserved_port`. Use another port such as `8080`. `GET /sandboxes/{id}/hosts` lists the exposed ports and `DELETE /sandboxes/{id}/hosts/{port}` removes one.

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

## Sources

- `POST /sandboxes` takes `source` to place project files on first boot: `{"type":"git","url":"https://...","revision":"main","depth":1,"token":"...","path":"app"}` or `{"type":"tarball","url":"https://...","path":"app"}`. The source runs once, before `setupScript`; `path` defaults to `project` under `/config`. Only https URLs are accepted (no `git@`/ssh URLs or embedded credentials). `token` is for private git repositories and is never returned or stored; the response reports `source` without it. Tarballs must be `.tar.gz`. Invalid sources return `400 invalid_source`; `POST /sandboxes/{id}/fork` rejects `source` because the fork already has the files.

## Box image

`image` on `POST /sandboxes` picks the container image for one box. Omitted uses the server's default box image (`GET`/`PUT /settings/images`). A reference that fails validation returns `400 invalid_image`.

Custom images must be built `FROM ghcr.io/tony1908/boxes-box`, which carries `boxd`. An image without it never becomes ready and the create call returns `503 box_not_ready`.

`POST /sandboxes/{id}/fork` always runs the source box's image; an `image` in the fork body is ignored. Resume and migrate also keep the box's own image.

## Drives

A drive is a named persistent volume. Sandboxes mount drives with `mounts` on `POST /sandboxes`:

```json
{"mounts": [{"drive": "data", "path": "/mnt/data"}, {"drive": "cache", "path": "/mnt/cache", "readOnly": true}]}
```

- `path` must be a clean absolute path under `/mnt/` (not `/mnt` itself). No more than 4 mounts, and no duplicate or nested paths. Invalid mounts return `400 invalid_mounts`.
- A drive must exist. At most one non-deleted sandbox may mount a drive read-write (`409 drive_busy`). Read-only mounts are unlimited and see live data.
- `POST /drives` with `{"name"}` creates a drive (`201`); a taken name returns `409 drive_exists`. Names match `^[a-z0-9][a-z0-9-]{0,62}$`.
- `GET /drives` returns `{"drives": [{name, createdAt, mountedBy: [{sandboxId, path, readOnly}]}]}`.
- `DELETE /drives/{name}` returns `409 drive_in_use` while any sandbox, running or stopped, mounts it. Otherwise the drive and its data are deleted.
- `POST /drives/{name}/fork` with `{"name": target}` copies the data into a new drive (`201`). It returns `409 drive_busy` while a running sandbox mounts the source read-write.
- Forks keep only the source's read-only mounts; read-write drives are not copied.
- Drive data persists until the drive is deleted and is not included in snapshots. There is no size quota.

## Users and groups

Users and groups are Linux accounts inside the box. Each change runs as root through the agent's exec endpoint, one argv per step (no shell), so names are never interpreted by a shell. Every route requires a running box (`ready` or `idle`); otherwise it returns `409 sandbox_not_running`.

- `POST /sandboxes/{id}/users` with `{"username"}` runs `useradd -m -s /bin/bash`, `chmod 770 /home/<name>`, and `usermod -aG <name> abc`. It returns `201 {"username", "homeDir": "/home/<name>"}`. An existing name returns `409 user_exists`.
- `GET /sandboxes/{id}/users` returns `{"users": [{username, homeDir, groups}]}` for accounts whose home is under `/home/`.
- `DELETE /sandboxes/{id}/users/{username}` runs `userdel -r` (the home is removed). It returns `204`, or `404 user_not_found` when the user does not exist.
- `POST /sandboxes/{id}/groups` with `{"name"}` runs `groupadd`, creates `/shared/<name>`, sets its group to `<name>` and mode `2770`, and adds `abc` to the group. It returns `201 {"name", "sharedDir": "/shared/<name>"}`. An existing name returns `409 group_exists`.
- `PUT /sandboxes/{id}/groups/{name}/members/{username}` runs `usermod -aG` and returns `200 {"group", "username"}`. `DELETE` on the same path runs `gpasswd -d` and returns `204`. A missing user or group returns `404 not_found`.
- Names match `^[a-z_][a-z0-9_-]{0,31}$`. `root` and `abc` are reserved. Invalid names return `400 invalid_user`, `invalid_group`, or `invalid_membership`.
- Run a command as a user with `user` on `POST /sandboxes/{id}/commands`. When `cwd` is omitted, a non-default user starts in `/home/<name>`; `abc` and `root` start in `/config`.

Isolation model: a home is `/home/<name>`, mode `770`, owned by the user and that user's primary group. `abc` joins the group so it can read the home. Two users cannot read each other's homes, and shared data goes in `/shared/<group>`. Setgid `2770` keeps files created there owned by the group. Membership changes apply to new processes only. A process that is already running (including `abc`'s processes) keeps its old groups until it restarts. Users, groups, and their files live in the box filesystem, so they persist across stop and resume like other system changes, and they are not part of drive data.

## Outbound network policy

`networkPolicy` controls where a box can send traffic. It is accepted by `POST /sandboxes` (and copied by `POST /sandboxes/{id}/fork` unless the body sets its own). Invalid policies return `400 invalid_network_policy`.

- `mode`: `allow-all` (the default), `deny-all` (no new outbound connections; existing ones may finish), or `allowlist` (only `allowDomains` and `allowCidrs`).
- `denyCidrs` win over every other rule in every mode. `169.254.169.254/32` (cloud metadata) is blocked for every box, with or without a `networkPolicy`: the agent installs one host firewall rule per `boxes-net` subnet when it starts, and a policy change never removes it.
- `POST /sandboxes/{id}/restore` with `{"snapshotId": "..."}` rolls the sandbox back to one of its own snapshots (`404 snapshot_not_found` otherwise). A running sandbox is stopped, which captures a final snapshot so the rollback can be undone, and then resumed from the chosen snapshot. A stopped sandbox records the snapshot for its next resume. Busy states (`provisioning`, `stopping`, `resuming`, `deleting`) return `409 sandbox_busy`.
- `keepSnapshots` on `POST /sandboxes` and `PATCH /sandboxes/{id}` sets how many automatic snapshots are kept, from 1 to 50; `0` or omitted uses the default of 5. Values outside that range return `400 invalid_keep_snapshots`. Forks copy the source value unless one is given. Named snapshots are never pruned.
- `PATCH /sandboxes/{id}` with `networkPolicy` replaces the policy; `null` clears it. A running box is updated immediately; a stopped box stores the policy and applies it when it next starts or resumes. If the node cannot apply a running box's policy, the request returns `502 network_policy_failed`, the stored policy stays unchanged, and the box is stopped so it is never left unfiltered.
- Limits: at most 64 entries per list; `allowDomains` entries are hostnames or `*.example.com`; CIDRs must be IPv4.

DNS goes through the agent. Each box is created with the agent's `boxes-net` gateway as its resolver, and that resolver is the only DNS server a box can reach in `allowlist` and `deny-all` mode. The agent answers each lookup from the box's policy:

- An allowed name is forwarded upstream. Every IPv4 address in the answer is opened for that box before the answer is returned, so the box can only connect to addresses it was actually given. AAAA answers are dropped because IPv6 is not filtered.
- A name that is not allowed is refused (`REFUSED`) without being forwarded. In `deny-all` mode every name is refused.
- `*.example.com` allows `example.com` and every subdomain, for example `api.example.com`. Matching ignores case and a trailing dot.
- DNS to any other resolver is blocked, so names cannot be used to leak data. This closes the DNS exfiltration channel that earlier allowlist boxes had.

Host-side resolution at apply time and the 60-second refresh still run, but they only warm the rules; the forwarder is the primary path.

Ceilings of the allowlist, so plan around them:

- IPv4 only. Boxes use an IPv4 bridge network, so IPv6 is rejected rather than filtered.
- `--dns` is fixed when a container is created. Boxes created before the forwarder keep their old resolver and the old unrestricted port-53 rule until they are recreated; they do not use the forwarder, so their names are not checked. Recreate them to get the forwarder.
- DNS depends on the agent for boxes created with the forwarder. If the agent is down, those boxes cannot resolve names. If the forwarder fails to start, the agent logs an error and new boxes get Docker's default resolver with no DNS restriction.
- On hosts whose INPUT policy drops by default, the agent adds an ACCEPT for DNS from `boxes-net` to the forwarder at startup. Per-box chains still drop DNS to any other address.
- A forwarded IP stays open for 30 minutes after its last lookup, and the rules are rebuilt on each refresh. A domain with more than 64 live IPs is capped at the 64 most recent.

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

`durationMs` and `outcome` are omitted when they do not apply. `nextCursor` is an empty string on the last page. Activity detail stores short command and prompt previews plus non-sensitive metadata; it omits file contents, full prompts, and credentials. In command previews only `NAME=value` pairs whose name looks secret (for example `API_TOKEN=`) are redacted, so `PORT=8080` stays readable. Rows older than 90 days are pruned daily. Activity rows are kept in `box_activity` without a foreign key to the box, so they remain readable after the sandbox is deleted.

## Usage and cost

`GET /api/v1/sandboxes/{id}/usage` returns `runningSeconds`, `runningSecondsThisMonth`, `perDay` (30 UTC dates), `commandsCount`, `agentRunsCount`, `desktopSessionsCount`, `snapshotsCount`, and `since`. It also returns `totals` (`cpuSeconds`, `rxBytes`, `txBytes`): the container's CPU time and network bytes accumulated at each stop, plus `live` when the box is running. `live` has the same three counters for the current container and is omitted when the box is stopped or its agent cannot answer. Network bytes are summed over every interface except loopback. The counters are host-side and reset when a container is recreated (resume or migrate), which is why they are accumulated at each stop; a crash between stops loses that session's numbers. Box JSON does not include these counters. It includes `snapshotsBytes` and `lastSnapshotAt` when a snapshot exists, using the most recent snapshot's size. When an hourly rate is configured for the box size (falling back to the `default` rate), it also returns `estimatedCost`:

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
  "live": {"cpuSeconds": 42.5, "rxBytes": 90112, "txBytes": 20480},
  "totals": {"cpuSeconds": 310.2, "rxBytes": 1048576, "txBytes": 524288},
  "estimatedCost": {"currency": "USD", "amount": 0.25, "hourlyRate": 0.5}
}
```

Owners can read or update pricing at `GET` and `PUT /api/v1/settings/pricing`. Both require the `owner` role:

```json
{"currency":"USD","hourlyBySize":{"xsmall":0.1,"small":0.2,"default":0.5,"large":1,"xlarge":2}}
```

An empty `hourlyBySize` disables cost estimates.
