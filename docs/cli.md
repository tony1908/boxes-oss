# Boxes CLI

The `boxes` CLI connects to a Boxes server and sends requests to its `/api/v1`
API. Install the `boxes` executable from a release, use the server's CLI
installer (`curl -fsSL https://boxes.example.test/install/cli.sh | sh`), or
build it from the repository with `go build ./cmd/boxes`.

## Authentication

For agents and CI, authenticate with an API key:

```sh
boxes login --server https://boxes.example.test --api-key "$BOXES_API_KEY"
```

`login` validates the key with `GET /api/v1/me` before saving it. The config
file is written to `~/.config/boxes/config.json` with `0600` permissions.
Use `--key-stdin` to read an API key from standard input without putting it in
shell history. `boxes logout` clears the saved server, token, and current box.
Alternatively, `BOXES_SERVER` and `BOXES_API_KEY` can supply the server and key
for login:

```sh
BOXES_SERVER=https://boxes.example.test BOXES_API_KEY=bx_live_… boxes login
```

For interactive account login, `boxes login --server URL --email EMAIL` prompts
for the password. The password can also be supplied with `--password` or
`BOXES_PASSWORD`.

`boxes whoami` prints the active server URL and authenticated account email.
It verifies the current credentials with the server.

## Environment overrides

Environment values override the saved config for each command, so scripts can
run without a config file:

| Variable | Purpose |
| --- | --- |
| `BOXES_SERVER` | Server base URL |
| `BOXES_TOKEN` | Bearer token |
| `BOXES_API_KEY` | API key used as the bearer token if `BOXES_TOKEN` is unset |

The server URL and bearer token are saved per user by `boxes login`. API keys
can be created in the web UI or through the API keys API. Avoid putting keys in
shell history; use environment injection from your CI secret store.

## Commands

Run `boxes --help` for the command list. Common commands include:

Supported sizes are `xsmall` (1 CPU, 2 GiB), `small` (2 CPU, 4 GiB),
`default` (4 CPU, 8 GiB), `large` (8 CPU, 16 GiB), and `xlarge` (16 CPU,
32 GiB). Use `--type xsmall` for a lighter box.

```sh
boxes new [--type SIZE] [--ttl SECONDS|--no-auto-stop] [-e KEY=VALUE]... [--setup-file FILE] [--from NAME] [--environment NAME] [--no-env]
boxes list [--filter r|s|e...] [--all]
boxes info <id|current>
boxes extend <id|current> --hours N | --ttl SECONDS | --no-auto-stop
boxes current
boxes exec <id|current> [--detach] [--status PID] -- <command> [arguments]
boxes prompt [id|current] "text" [--provider claude|codex|opencode] [--stop-when-done]
boxes conversations <id|current>
boxes snapshots <id|current>
boxes usage <id|current>
boxes events <id> [--follow] [--after CURSOR]
boxes desktop <id> [--public]
boxes host <id> <port> [--private]
boxes stop [id|current]
boxes resume <id|current> [--ttl SECONDS|--no-auto-stop] [--type SIZE] [--node ID]
boxes fork <id|current> [--ttl SECONDS|--no-auto-stop]
boxes delete <id|current> [--yes]
boxes limits
boxes status
boxes dashboard [id|current]
boxes forward <id|current> <port> [--local-port N]
boxes skill install [--claude] [--codex] [--force]
boxes image push [--box IMAGE] [--tools IMAGE] [--tag TAG]
```

Use the global `--json` option before the command (for example,
`boxes --json list`) for machine-readable output. It is enabled automatically
when stdout is not a TTY.

Creating a box, forking one, or using a box command records that box ID as
`current` in the CLI config. Use `current` in place of any box ID to reuse that
box. `list` shows running boxes by default (`r`); `s` selects stopping/stopped,
`e` selects error/unreachable, and filters can be combined (for example,
`--filter rs`). `--all` shows every state.

`--no-auto-stop` disables the TTL. The default for new boxes is one hour;
`--ttl` is in seconds. `extend --hours N` converts hours to seconds, while
`extend --ttl` sets an explicit timer and `extend --no-auto-stop` turns it off.
`prompt --stop-when-done` asks the server to stop and snapshot the box after the
run reaches a terminal state. `delete` requires `--yes` when stdin is not a
terminal.

`dashboard [id]` opens the server root or the selected box page. `forward` binds
only to `127.0.0.1` and forwards each local TCP connection over the server's
WebSocket tunnel. `boxes proxy <id>` remains available for the CLI's SSH and SCP
tunnel commands.

`skill install` installs the embedded Boxes agent skill for Claude Code and
Codex. Pass `--claude` or `--codex` to select one. Existing modified files are
preserved unless `--force` is supplied.

`image push` publishes one or both local Docker images to the server's built-in
authenticated registry and updates the server's default image references. For
example, `boxes image push --box boxes/box:dev --tools boxes/tools:dev --tag $(git rev-parse --short HEAD)`.
