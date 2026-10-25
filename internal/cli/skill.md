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
boxes exec <id|current> [--user NAME] [--cwd DIR] [--env K=V]... -- bash -lc '<command>'
boxes exec <id|current> --detach -- <command>
boxes ps <id|current>
boxes logs <id|current> <processId> [-f]
boxes kill <id|current> <processId> [--signal TERM]
boxes restore <id|current> <snapshotId>
boxes drive create|list|rm|fork
boxes user add|list|rm <id|current> [name]
boxes group add <id|current> <group>
boxes group member add|rm <id|current> <group> <user>
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

Agent credentials are supplied through an Environment attached to the box. Claude uses `ANTHROPIC_API_KEY`; Codex uses `OPENAI_API_KEY`; OpenCode uses the key for its selected model provider; Pi accepts provider keys such as `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and `OPENROUTER_API_KEY`; Kimi Code uses `KIMI_MODEL_API_KEY` with `KIMI_MODEL_NAME` for headless API-key access, or its saved login; Prime Agent accepts provider keys such as `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and `OPENROUTER_API_KEY`.

Use `boxes push <id> [dir] [--to path]` to copy a repo into a box. The default destination is `/config/<directory name>`. Don't copy `node_modules` or base64-encode files through `exec`.

SDKs: [TypeScript](../../sdk/typescript/README.md) · [Python](../../sdk/python/README.md).

## Working in a box

- Reuse a box: run `boxes current` or `boxes list` first and reuse the running box. Don't create a second box unless the user asks.
- `boxes push <id> .` lands the repo in `/config/<directory name>`. For a fixed path use `boxes push <id> . --to /config/project`, and `cd` there in every `exec`.
- Ports 3000, 3001 and 7070 are taken by the box (desktop, boxd). Serve apps on 8080 or another port, then run `boxes host <id> <port>`.
- Long-running servers: start them with `boxes exec <id> --detach -- <command>`, then check them with `boxes ps <id>` and `boxes logs <id> <processId>`.
- Re-run the tests after every push and report the final exit code.

## Run a repo's tests

```sh
boxes new --type small --ttl 3600
boxes push <id> .
boxes exec <id> -- bash -lc 'cd /config/<name> && <install> && <test>'
```


Run `boxes --help` for all commands. Add `--json` before a command for
machine-readable output; JSON output is automatic when stdout is not a TTY.

## Tags, network, and sources

- `boxes new --tag KEY=VALUE` tags a box; `boxes list --tag KEY=VALUE` (or `--tag KEY` for any value) filters, and all filters must match. `boxes update <id|current> --tag K=V` merges tags; `--clear-tags` removes them.
- `boxes new --network allow-all|deny-all|allowlist` sets outbound policy. `--allow-domain HOST` and `--allow-cidr CIDR` (repeatable) need `allowlist`; `--deny-cidr CIDR` blocks in every mode. `boxes update <id|current> --network ...` applies the same policy to a live box.
- `boxes new --git URL [--revision REF] [--depth N] [--git-token-env VAR] [--path DIR]` clones a repo on first boot; `--tarball URL` extracts a `.tar.gz` instead. The two are mutually exclusive.

## Detached commands

- `boxes exec <id|current> --detach -- <command>` starts a background process and prints its processId. `boxes ps <id>` lists them; `boxes exec <id> --status PID` shows one.
- `boxes logs <id> <processId> [-f]` prints output; `-f` follows until the command exits.
- `boxes kill <id> <processId> [--signal TERM|KILL|INT|HUP|QUIT|USR1|USR2]` signals it.
- `exec` and `logs -f` exit with the remote command's exit code; other failures exit 1.

## Snapshots, images, usage

- `boxes new --keep-snapshots N` (or `update`) sets how many automatic snapshots to keep (1 to 50, default 5). `boxes snapshots <id>` lists them; `boxes restore <id> <snapshotId>` rolls the box back to one.
- `boxes new --image REF` picks a container image, which must be built `FROM ghcr.io/tony1908/boxes-box`.
- `boxes usage <id|current>` prints CPU and network usage totals.

## Drives and users

- Drives are named volumes that survive box deletion: `boxes drive create|list|rm|fork`. Mount one with `boxes new --mount DRIVE:/mnt/x[:ro]` (path under `/mnt`). `drive rm` fails while a box mounts it.
- `boxes user add <id> <name>`, `user list <id>`, `user rm <id> <name>` manage isolated users; `boxes group add <id> <group>` and `boxes group member add|rm <id> <group> <user>` manage shared groups. Run as a user with `boxes exec <id> --user NAME -- <command>`.

## Usage notes

- Prefer reusing an existing ready box from `boxes list` over creating a new one; create one only when the user wants a fresh or isolated machine.
- Servers you want to expose publicly must bind `0.0.0.0`, then run `boxes host <id|current> <port>`.
- `boxes forward` exposes a box TCP port only on local `127.0.0.1`.
- Long-running processes: start them detached (`nohup … &`) so `exec` returns.
- Never delete or stop a box the user didn't ask you to; stopped boxes cost nothing and keep their disk.
- Never print API keys, desktop URLs, or tokens into files or commit them.
