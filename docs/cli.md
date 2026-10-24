# Boxes CLI

The `boxes` CLI connects to a Boxes server and sends requests to its `/api/v1`
API. Install it from a release, use the server's CLI installer
(`curl -fsSL https://<your-domain>/install/cli.sh | sh`), or build it from the
repository with `go build ./cmd/boxes`.

## Authentication

For agents and CI, authenticate with an API key:

```sh
boxes login --server https://boxes.example.com --api-key "$BOXES_API_KEY"
```

`login` validates the key with `GET /api/v1/me` before saving it. The config
file is written to `~/.config/boxes/config.json` with `0600` permissions. Use
`--key-stdin` to read the key from standard input without putting it in shell
history:

```sh
printf '%s' "$BOXES_API_KEY" | boxes login --server https://boxes.example.com --key-stdin
```

For interactive account login, pass an email. The password is prompted for
unless supplied with `--password` or `BOXES_PASSWORD`:

```sh
boxes login --server https://boxes.example.com --email you@example.com
```

`boxes whoami` prints the active server URL and authenticated account email,
verifying the current credentials with the server. `boxes logout` clears the
saved server, token, and current box.

API keys are created in the web UI (or with `POST /api/v1/api-keys`). Create
keys for scripts and CI, and inject them from your secret store rather than
pasting them into shell history.

## Environment overrides

Environment values override the saved config for each command, so scripts can
run without a config file:

| Variable | Purpose |
| --- | --- |
| `BOXES_SERVER` | Server base URL (also the default for `login --server`) |
| `BOXES_TOKEN` | Bearer token |
| `BOXES_API_KEY` | API key, used as the bearer token when `BOXES_TOKEN` is unset |
| `BOXES_PASSWORD` | Account password for `login --email` |

## Box sizes

`xsmall` (1 CPU, 2 GiB), `small` (2 CPU, 4 GiB), `default` (4 CPU, 8 GiB),
`large` (8 CPU, 16 GiB), and `xlarge` (16 CPU, 32 GiB). `new` defaults to
`default`; pass `--type xsmall` for a lighter box.

## Commands

Run `boxes --help` for the command list. The full set of top-level commands is:

```
login logout whoami new list get info status current exec ps logs kill ssh proxy scp
push pull prompt events steer interrupt conversations snapshots snapshot usage
limits desktop browser host stop resume restore extend fork delete dashboard forward
skill env
```

Common forms:

```sh
boxes new [--type SIZE] [--image REF] [--ttl SECONDS | --no-auto-stop] [--idle-stop DURATION] \
          [-e KEY=VALUE]... [--setup-file FILE] [--from SNAPSHOT] [--environment ID | --no-env] \
          [--tag KEY=VALUE]... [--keep-snapshots N] [--network MODE] [--allow-domain HOST]... [--allow-cidr CIDR]... [--deny-cidr CIDR]... \
          [--git URL [--revision X] [--depth N] [--git-token-env VAR] [--path P] | --tarball URL [--path P]]
boxes list [--filter r|s|e...] [--all] [--tag KEY=VALUE|KEY]...
boxes update <id|current> [--tag KEY=VALUE]... [--clear-tags] [--keep-snapshots N] [--network MODE] [--allow-domain HOST]... [--allow-cidr CIDR]... [--deny-cidr CIDR]...
boxes info <id|current>
boxes current
boxes exec <id|current> [--detach] [--status PID] -- <command> [arguments]
boxes ps <id|current>                               # detached commands for a box
boxes logs <id|current> <processId> [-f]             # output; -f follows and exits with the command status
boxes kill <id|current> <processId> [--signal TERM]  # signal a detached command
boxes prompt [id|current] "text" [--provider claude|codex|opencode|pi|prime|kimi] [--model M] [--new | --conversation ID] [--stop-when-done]
boxes steer <id> <message> [--conversation ID]
boxes interrupt <id> [--conversation ID]
boxes events <id> [--follow]
boxes conversations <id|current>
boxes snapshots <id|current>
boxes usage <id|current>
boxes desktop <id> [--public] [--profile NAME]
boxes browser <id> [--public] [--profile NAME]
boxes host <id> <port> [--private]
boxes forward <id|current> <port> [--local-port N]
boxes push <id> [dir] [--to PATH]
boxes pull <id> <path> [-o DEST]
boxes stop [id|current]
boxes resume <id|current> [--type SIZE] [--ttl SECONDS | --no-auto-stop]
boxes restore <id|current> <snapshotId>
boxes fork <id|current> [--ttl SECONDS | --no-auto-stop]
boxes extend <id|current> --hours N | --ttl SECONDS | --no-auto-stop | --idle-stop DURATION
boxes delete <id|current> [--yes]
boxes snapshot <id> <name> | snapshot pull <snapshotId|boxId> [-o FILE] | snapshot rm <name|snapshotId>
boxes limits
boxes status
boxes dashboard [id|current]
boxes skill install [--claude] [--codex] [--force]
boxes env list | create <name> [JSON] | update <id> <JSON> | delete <id> | upgrade <id>
```

`--json` goes before the command (for example, `boxes --json list`) and prints
machine-readable output. `boxes list` also accepts `--json` after the
subcommand.

Creating a box, forking one, or using a box command records that box ID as
`current` in the CLI config. Use `current` in place of any box ID to reuse that
box. `list` shows running boxes by default (`r`); `s` selects stopping/stopped,
`e` selects error/unreachable, and filters can be combined (for example,
`--filter rs`). `--all` shows every state.

### Lifetime

New boxes default to a one-hour auto-stop timer (`--ttl 3600`). `--ttl` is in
seconds and `--no-auto-stop` disables the timer; the two are mutually exclusive.
`--idle-stop` stops a box after a period of inactivity (a Go duration such as
`30m`; `off` disables it). `extend --hours N` converts hours to seconds, while
`extend --ttl` sets an explicit timer and `extend --no-auto-stop` turns it off.

`prompt --stop-when-done` asks the server to stop and snapshot the box once the
run reaches a terminal state. `delete` requires `--yes` when stdin is not a
terminal.

### Tags

`--tag KEY=VALUE` (repeatable) attaches tags to a new box. Keys match
`^[a-z0-9][a-z0-9_.-]{0,62}$`, values are at most 255 characters, and a box can
hold 10 tags. `boxes update <id|current> --tag KEY=VALUE` merges the given tags
into the existing ones, and `--clear-tags` removes them all (the two cannot be
combined). `boxes list --tag KEY=VALUE` keeps boxes with that exact value, and
`--tag KEY` keeps boxes that have the key; repeated filters must all match. Tags
appear in the `tags` field of `boxes info` and `boxes get` output.

### Outbound network policy

`boxes new --image REF` runs the box in a specific container image. The image must be built `FROM ghcr.io/tony1908/boxes-box` so boxd is present; otherwise the box never becomes ready (`box_not_ready`). Without the flag the server default image is used.

`boxes usage <id|current>` prints the usage JSON from `GET /sandboxes/{id}/usage`, including `totals` (`cpuSeconds`, `rxBytes`, `txBytes`) and `live` while the box runs. See [api.md](api.md#usage-and-cost).

`boxes new` and `boxes update <id|current>` take the same network flags:

- `--git URL` clones an https repository into `/config/<path>` once on first boot, before `--setup-file` runs. `--revision` takes a branch, tag, or commit SHA; `--depth N` makes a shallow clone (1 to 1000); `--path` picks the directory (default `project`).
- `--git-token-env VAR` reads the git token from the environment variable `VAR` for private repositories. The token is never passed as a flag value and is not stored.
- `--tarball URL` downloads a `.tar.gz` and extracts it into `/config/<path>`. It cannot be combined with `--git`, and `--revision`, `--depth`, and `--git-token-env` apply only to `--git`.

```sh
boxes new --git https://github.com/acme/app.git --revision v2.1.0 --depth 1 --git-token-env GITHUB_TOKEN --path app
```
- `--network allow-all|deny-all|allowlist` sets the mode (`allow-all` is the default for new boxes).
- `--allow-domain HOST` (repeatable) allows a hostname or `*.example.com`; `--allow-cidr CIDR` (repeatable) allows an IPv4 range. Both require `--network allowlist`.
- `--deny-cidr CIDR` (repeatable) blocks an IPv4 range in every mode and wins over allows. `169.254.169.254/32` is always blocked.

```sh
boxes new --network allowlist --allow-domain api.github.com --allow-domain '*.npmjs.org' --deny-cidr 10.0.0.0/8
boxes update current --network deny-all
```

`update` applies to a running box at once and to a stopped box on its next start.
Allowlists are DNS-based and IPv4 only; see the limits in the API reference.

### Desktop, browser, and forwarding

`desktop` opens the box's XFCE desktop session in the browser. `browser` opens
the same session in browser mode and accepts `--profile`. `--public` creates a
publicly reachable URL with a seven-day session; without it the session lasts
eight hours by default. See [desktop.md](desktop.md).

`host <id> <port>` exposes a TCP port from the box through the server;
`--private` requires authentication to reach it.

`forward` binds only to `127.0.0.1` and forwards each local TCP connection over
the server's WebSocket tunnel. `dashboard [id]` opens the server root or the
selected box page. `ssh`, `scp`, and `proxy <id>` are the SSH helpers.

### Skill installation

`skill install` writes the embedded Boxes agent skill to `~/.claude/skills/boxes/SKILL.md`
and `~/.codex/skills/boxes/SKILL.md`. Pass `--claude` or `--codex` to select one.
Existing modified files are preserved unless `--force` is supplied.

## Quickstart

```sh
boxes login --server https://boxes.example.com --email you@example.com
boxes new --type small --ttl 3600        # creates a box and makes it "current"
boxes exec current -- uname -a            # run a command
boxes push current ./project --to /config/project
boxes exec current -- sh -c 'cd /config/project && ls'
boxes exec current --detach -- sleep 600   # prints a processId
boxes logs current <processId> -f        # stream output until it exits
boxes kill current <processId>            # SIGTERM the command
boxes desktop current                     # open the desktop session
boxes prompt current "summarize the repo" --provider claude --stop-when-done
boxes stop current                        # captures a snapshot, then stops
boxes resume current                      # restores from the latest snapshot
boxes restore current <snapshotId>        # roll back to one of its snapshots
boxes fork current --ttl 1800             # copies the filesystem into a new box
boxes delete current --yes
```

`fork` creates a new box from a fresh capture of the source; `resume` brings a
stopped box back from its latest snapshot.

`new --keep-snapshots N` and `update --keep-snapshots N` set how many automatic
snapshots the box keeps (1 to 50; default 5). `restore` rolls the box back to one
of its own snapshots: a running box is stopped and resumed from it, and a stopped
box resumes from it next. Snapshot IDs come from `boxes snapshots <id>`. See [lifecycle.md](lifecycle.md) and
[snapshots.md](snapshots.md).
