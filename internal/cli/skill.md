---
name: boxes
description: Use when the user asks to run something in a cloud box/sandbox/VM, use "my box", spin up a machine for an agent, test in an isolated Linux environment, or mentions boxes / bx_ IDs. Controls boxes-oss sandboxes (self-hosted boat.dev) through the `boxes` CLI.
---

<!-- boxes-skill-managed -->

# boxes: remote Linux machines for agents

`boxes` controls full Ubuntu sandboxes (sudo, Docker inside, XFCE desktop, Claude Code/Codex/OpenCode preinstalled) on the user's self-hosted server. The CLI is already logged in (check with `boxes whoami`). Box IDs look like `bx_xxxxxxxxxxxx`. User is `abc`, home is `/config`.

## Lifecycle

- `boxes new --type small --ttl 3600` creates a box that auto-stops after an hour. `--idle-stop 30m` enables idle stop (Go durations from 5m to 720h). Sizes are `xsmall|small|default|large|xlarge`. Use `--ttl SECONDS` for a custom timer or `--no-auto-stop` for an always-on box. Creation records the box as `current`.
- `boxes extend <id|current> --hours 2`, `--ttl SECONDS`, or `--no-auto-stop` changes its timer. Use `--idle-stop 30m` or `--idle-stop off` to set or disable idle stop.
- `boxes info <id|current>` shows box details. `boxes list` shows running boxes by default; use `--filter r|s|e` or `--all` to choose a state group or show all boxes.
- `boxes current` prints the most recently created or used box ID. Lifecycle commands accept `current` in place of an ID.
- `boxes stop <id|current>` snapshots and stops the box; `boxes resume <id|current>` resumes it. `boxes delete <id|current> --yes` permanently deletes it.
- `boxes pull <id> <path> [-o dest]` downloads a file or extracts a directory from a running sandbox.

## Commands

```sh
boxes list [--filter r|s|e...] [--all]
boxes new [--type xsmall|small|default|large|xlarge] [--ttl SECONDS|--no-auto-stop] [--idle-stop DURATION] [-e K=V]... [--setup-file FILE] [--from NAME] [--environment NAME] [--no-env]
boxes info <id|current>
boxes extend <id|current> --hours N|--ttl SECONDS|--no-auto-stop|--idle-stop DURATION|off
boxes exec <id|current> -- bash -lc '<command>'
boxes push <id> [dir] [--to path]
boxes prompt [id|current] "task" --provider claude|codex|opencode|pi|prime|kimi [--stop-when-done]
boxes conversations <id|current>
boxes snapshots <id|current>
boxes snapshot pull <snapshotId|boxId> [-o file.tar.gz]
boxes snapshots rm <boxId> <snapshotId>
boxes snapshot rm <name|snapshotId>
boxes usage <id|current>
boxes stop <id|current> / boxes resume <id|current>
boxes fork <id|current> [--ttl SECONDS|--no-auto-stop]
boxes delete <id|current> --yes
boxes dashboard [id|current]
boxes forward <id|current> <port> [--local-port N]
```

Agent credentials are supplied through an Environment attached to the box. Claude uses `ANTHROPIC_API_KEY`; Codex uses `OPENAI_API_KEY`; OpenCode uses the key for its selected model provider; Pi accepts provider keys such as `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and `OPENROUTER_API_KEY`; Kimi Code uses `MOONSHOT_API_KEY` / `KIMI_API_KEY` or its saved login; Prime Agent accepts provider keys such as `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and `OPENROUTER_API_KEY`.

Use `boxes push <id> [dir] [--to path]` to copy a repo into a box. The default destination is `/config/<directory name>`. Don't copy `node_modules` or base64-encode files through `exec`.

SDKs: [TypeScript](../../sdk/typescript/README.md) · [Python](../../sdk/python/README.md).

## Run a repo's tests

```sh
boxes new --type small --ttl 3600
boxes push <id> .
boxes exec <id> -- bash -lc 'cd /config/<name> && <install> && <test>'
```

Node: `npm ci || (rm -rf node_modules package-lock.json && npm install)`. The fallback handles lockfiles made on another OS or out of sync with package.json; it only changes the box's copy. Report the test results. Don't do extra work such as starting dev servers or taking screenshots unless asked.

Run `boxes --help` for all commands. Add `--json` before a command for
machine-readable output; JSON output is automatic when stdout is not a TTY.

## Usage notes

- Prefer reusing an existing ready box from `boxes list` over creating a new one; create one only when the user wants a fresh or isolated machine.
- Servers you want to expose publicly must bind `0.0.0.0`, then run `boxes host <id|current> <port>`.
- `boxes forward` exposes a box TCP port only on local `127.0.0.1`.
- Long-running processes: start them detached (`nohup … &`) so `exec` returns.
- Never delete or stop a box the user didn't ask you to; stopped boxes cost nothing and keep their disk.
- Never print API keys, desktop URLs, or tokens into files or commit them.
