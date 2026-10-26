# Paddock CLI

The `paddock` CLI connects to a Paddock server and sends requests to its `/api/v1`
API. Install it from a release, use the server's CLI installer
(`curl -fsSL https://<your-domain>/install/cli.sh | sh`), or build it from the
repository with `go build ./cmd/paddock`.

## Authentication

For agents and CI, authenticate with an API key:

```sh
paddock login --server https://paddock.example.com --api-key "$PADDOCK_API_KEY"
```

`login` validates the key with `GET /api/v1/me` before saving it. The config
file is written to `~/.config/paddock/config.json` with `0600` permissions. Use
`--key-stdin` to read the key from standard input without putting it in shell
history:

```sh
printf '%s' "$PADDOCK_API_KEY" | paddock login --server https://paddock.example.com --key-stdin
```

For interactive account login, pass an email. The password is prompted for
unless supplied with `--password` or `PADDOCK_PASSWORD`:

```sh
paddock login --server https://paddock.example.com --email you@example.com
```

`paddock whoami` prints the active server URL and authenticated account email,
verifying the current credentials with the server. `paddock logout` clears the
saved server, token, and current box.

API keys are created in the web UI (or with `POST /api/v1/api-keys`). Create
keys for scripts and CI, and inject them from your secret store rather than
pasting them into shell history.

## Environment overrides

Environment values override the saved config for each command, so scripts can
run without a config file:

| Variable | Purpose |
| --- | --- |
| `PADDOCK_SERVER` | Server base URL (also the default for `login --server`) |
| `PADDOCK_TOKEN` | Bearer token |
| `PADDOCK_API_KEY` | API key, used as the bearer token when `PADDOCK_TOKEN` is unset |
| `PADDOCK_PASSWORD` | Account password for `login --email` |

## Box sizes

`xsmall` (1 CPU, 2 GiB), `small` (2 CPU, 4 GiB), `default` (4 CPU, 8 GiB),
`large` (8 CPU, 16 GiB), and `xlarge` (16 CPU, 32 GiB). `new` defaults to
`default`; pass `--type xsmall` for a lighter box.

## Commands

Run `paddock --help` for the command list. Every command accepts `--help` or `-h` anywhere in its arguments (after a `--` separator the arguments belong to the remote command) and prints its usage without calling the server; `paddock help <command>` does the same. The full set of top-level commands is:

```
login logout whoami new list get info status current exec ps logs kill ssh proxy scp
push pull screenshot prompt events steer interrupt conversations snapshots restore usage limits
desktop browser host stop resume extend update fork snapshot delete dashboard forward
skill env drive user group help
```

Common forms:

```sh
paddock new [--type SIZE] [--image REF] [--ttl SECONDS | --no-auto-stop] [--idle-stop DURATION] \
          [-e KEY=VALUE]... [--setup-file FILE] [--from SNAPSHOT] [--environment ID | --no-env] \
          [--tag KEY=VALUE]... [--keep-snapshots N] [--network MODE] [--allow-domain HOST]... [--allow-cidr CIDR]... [--deny-cidr CIDR]... \
          [--git URL [--revision X] [--depth N] [--git-token-env VAR] [--path P] | --tarball URL [--path P]]
paddock list [--filter r|s|e...] [--all] [--tag KEY=VALUE|KEY]...
paddock update <id|current> [--tag KEY=VALUE]... [--clear-tags] [--keep-snapshots N] [--network MODE] [--allow-domain HOST]... [--allow-cidr CIDR]... [--deny-cidr CIDR]...
paddock get <id>
paddock info <id|current>
paddock current
paddock exec <id|current> [--user NAME] [--cwd DIR] [--env KEY=VALUE]... [--detach] [--status PID] -- <command> [arguments]
paddock ps <id|current>                               # detached commands for a box
paddock logs <id|current> <processId> [-f]             # output; -f follows and exits with the command status
paddock kill <id|current> <processId> [--signal TERM]  # signal a detached command
paddock prompt [id|current] "text" [--provider claude|codex|opencode|pi|prime|kimi] [--model M] [--new | --conversation ID] [--stop-when-done]
paddock steer <id> <message> [--conversation ID]
paddock interrupt <id> [--conversation ID]
paddock events <id> [--follow]
paddock conversations <id|current>
paddock snapshots <id|current>
paddock snapshots rm <boxId> <snapshotId>
paddock usage <id|current>
paddock desktop <id> [--public] [--profile NAME]
paddock screenshot <id|current> [-o file.png]  # capture the desktop to a PNG
paddock browser <id> [--public] [--profile NAME]
paddock host <id> <port> [--private]
paddock forward <id|current> <port> [--local-port N]
paddock push <id> [dir] [--to PATH]
paddock pull <id> <path> [-o DEST]
paddock stop [id|current]
paddock resume <id|current> [--type SIZE] [--ttl SECONDS | --no-auto-stop]
paddock restore <id|current> <snapshotId>
paddock fork <id|current> [--ttl SECONDS | --no-auto-stop]
paddock extend <id|current> --hours N | --ttl SECONDS | --no-auto-stop | --idle-stop DURATION
paddock delete <id|current> [--yes]
paddock snapshot <id> <name> | snapshot pull <snapshotId|boxId> [-o FILE] | snapshot rm <name|snapshotId>
paddock limits
paddock status
paddock dashboard [id|current]
paddock skill install [--claude] [--codex] [--force]
paddock env list | create <name> [JSON] | update <id> <JSON> | delete <id> | upgrade <id>
```

`--json` goes before the command (for example, `paddock --json list`) and prints
machine-readable output. `paddock list` also accepts `--json` after the
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
run reaches a terminal state.

`paddock delete <id|current>` permanently deletes a box. It prompts for
confirmation on a terminal; when stdin is not a terminal it requires `--yes`.

### Tags

`--tag KEY=VALUE` (repeatable) attaches tags to a new box. Keys match
`^[a-z0-9][a-z0-9_.-]{0,62}$`, values are at most 255 characters, and a box can
hold 10 tags. `paddock update <id|current> --tag KEY=VALUE` merges the given tags
into the existing ones, and `--clear-tags` removes them all (the two cannot be
combined). `paddock list --tag KEY=VALUE` keeps boxes with that exact value, and
`--tag KEY` keeps boxes that have the key; repeated filters must all match. Tags
appear in the `tags` field of `paddock info` and `paddock get` output.

### Images

`paddock new --image REF` runs the box in a specific container image. The image must be built `FROM ghcr.io/tony1908/paddock-box` so boxd is present; otherwise the box never becomes ready (`box_not_ready`). Without the flag the server default image is used.

### Usage

`paddock usage <id|current>` prints the usage JSON from `GET /sandboxes/{id}/usage`, including `totals` (`cpuSeconds`, `rxBytes`, `txBytes`) and `live` while the box runs. See [api.md](api.md#usage-and-cost).

### Sources

`paddock new` takes one source, either a git repository or a tarball, which is fetched once on first boot before `--setup-file` runs:

- `--git URL` clones an https repository into `/config/<path>`. `--revision` takes a branch, tag, or commit SHA; `--depth N` makes a shallow clone (1 to 1000); `--path` picks the directory (default `project`).
- `--git-token-env VAR` reads the git token from the environment variable `VAR` for private repositories. The token is never passed as a flag value and is not stored.
- `--tarball URL` downloads a `.tar.gz` and extracts it into `/config/<path>`. It cannot be combined with `--git`, and `--revision`, `--depth`, and `--git-token-env` apply only to `--git`.

```sh
paddock new --git https://github.com/acme/app.git --revision v2.1.0 --depth 1 --git-token-env GITHUB_TOKEN --path app
```

### Outbound network policy

`paddock new` and `paddock update <id|current>` take the same network flags:

- `--network allow-all|deny-all|allowlist` sets the mode (`allow-all` is the default for new boxes).
- `--allow-domain HOST` (repeatable) allows a hostname or `*.example.com`; `--allow-cidr CIDR` (repeatable) allows an IPv4 range. Both require `--network allowlist`.
- `--deny-cidr CIDR` (repeatable) blocks an IPv4 range in every mode and wins over allows. `169.254.169.254/32` is always blocked.

```sh
paddock new --network allowlist --allow-domain api.github.com --allow-domain '*.npmjs.org' --deny-cidr 10.0.0.0/8
paddock update current --network deny-all
```

`update` applies to a running box at once and to a stopped box on its next start.
Allowlists are DNS-based and IPv4 only; see the limits in the API reference.

### Exec and detached commands

`paddock exec <id|current> [flags] -- <command> [arguments]` runs a command in the box.
Flags may appear before or after the box ID, but must come before `--`:

- `--user NAME` runs as a box user (default `abc`).
- `--cwd DIR` sets the working directory inside the box.
- `--env KEY=VALUE` (repeatable) sets environment variables for the command.
- `--desktop` runs the command inside the box desktop session (`DISPLAY=:1` plus the session's bus and runtime variables), so GUI apps appear on the desktop. Use it with `--detach` for apps that keep running. It runs as `abc` only and returns `409 desktop_not_running` when no session is up.
- `--detach` starts the command in the background and prints its processId. `paddock ps <id>` lists detached commands, `paddock exec <id> --status PID` shows one, `paddock logs <id> PID [-f]` prints its output (`-f` follows until it exits), and `paddock kill <id> PID [--signal TERM]` signals it.

`exec` exits with the command's exit code, so `paddock exec` works in shell conditions and CI. `logs -f` does the same once the command has exited. Other CLI failures exit with 1.

### Drives

Drives are named volumes that survive box deletion. Create one, then mount it
with `--mount DRIVE:PATH[:ro]` (repeatable; `PATH` must be under `/mnt`):

```
paddock drive create data
paddock new --mount data:/mnt/data --mount cache:/mnt/cache:ro
paddock drive list
paddock drive fork data data-copy
paddock drive rm data
```

Data persists until the drive is deleted and is not included in snapshots.
A drive can be mounted read-write by one box at a time; read-only mounts see
live data from the writer. `drive rm` fails while any box mounts the drive.
`drive fork` copies the data and fails while a running box mounts the source
read-write. Forks keep only read-only mounts. There is no size quota.

### Users and groups

Each box can hold isolated Linux users and shared groups. Users run commands
with `exec --user`; their homes are `/home/<name>` (mode 770), and `abc` can
read them through group membership. Shared groups get `/shared/<group>`
(setgid 2770).

```
paddock user add <box> alice
paddock user list <box>
paddock user rm <box> alice
paddock group add <box> team
paddock group member add <box> team alice
paddock group member rm <box> team alice
paddock exec <box> --user alice -- id -un
```

Accounts live in the box filesystem and persist across stop and resume.
`user rm` deletes the home directory. Membership changes apply to new
processes only; a process that is already running keeps its old groups.
`user add` and `group add` fail with 409 if the name is taken; `root` and `abc`
are reserved. See the API docs for the isolation model.

### Desktop, browser, and forwarding

Launch a GUI app on the desktop with `paddock exec <id> --desktop --detach -- ./my-app`.
`paddock screenshot <id> [-o file.png]` captures the full desktop (display `:1`) to a PNG
and downloads it; the default file is `screenshot-<id>.png`. The temporary capture in the
box is removed after the download.

`desktop` opens the box's XFCE desktop session in the browser. `browser` opens
the same session in browser mode and accepts `--profile`. `--public` creates a
publicly reachable URL with a seven-day session; without it the session lasts
eight hours by default. See [desktop.md](desktop.md).

`host <id> <port>` exposes a TCP port from the box through the server;
`--private` requires authentication to reach it.
Ports 3000, 3001 and 7070 are reserved: the box uses them for its desktop and
boxd, so `host` rejects them with `reserved_port`. Serve apps on 8080 or another
port. `push <id> [dir]` copies to `/config/<directory name>` by default; pass
`--to /config/project` for a fixed destination.

`forward` binds only to `127.0.0.1` and forwards each local TCP connection over
the server's WebSocket tunnel. `dashboard [id]` opens the server root or the
selected box page. `ssh`, `scp`, and `proxy <id>` are the SSH helpers.

### Skill installation

`skill install` writes the embedded Paddock agent skill to `~/.claude/skills/paddock/SKILL.md`
and `~/.codex/skills/paddock/SKILL.md`. Pass `--claude` or `--codex` to select one.
Existing modified files are preserved unless `--force` is supplied.

## Quickstart

```sh
paddock login --server https://paddock.example.com --email you@example.com
paddock new --type small --ttl 3600        # creates a box and makes it "current"
paddock exec current -- uname -a            # run a command
paddock push current ./project --to /config/project
paddock exec current -- sh -c 'cd /config/project && ls'
paddock exec current --detach -- sleep 600   # prints a processId
paddock logs current <processId> -f        # stream output until it exits
paddock kill current <processId>            # SIGTERM the command
paddock desktop current                     # open the desktop session
paddock prompt current "summarize the repo" --provider claude --stop-when-done
paddock stop current                        # captures a snapshot, then stops
paddock resume current                      # restores from the latest snapshot
paddock restore current <snapshotId>        # roll back to one of its snapshots
paddock fork current --ttl 1800             # copies the filesystem into a new box
paddock delete current --yes
```

`fork` creates a new box from a fresh capture of the source; `resume` brings a
stopped box back from its latest snapshot.

`new --keep-snapshots N` and `update --keep-snapshots N` set how many automatic
snapshots the box keeps (1 to 50; default 5). `restore` rolls the box back to one
of its own snapshots: a running box is stopped and resumed from it, and a stopped
box resumes from it next. Snapshot IDs come from `paddock snapshots <id>`. See [lifecycle.md](lifecycle.md) and
[snapshots.md](snapshots.md).
