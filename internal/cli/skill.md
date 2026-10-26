---
name: paddock
description: Use when the user asks to run something in a cloud box, sandbox, or VM, use "my box", spin up a machine for an agent, test in an isolated Linux environment, mentions paddock, or mentions boxes / bx_ IDs. Controls self-hosted Paddock sandboxes through the `paddock` CLI.
---

<!-- paddock-skill-managed -->

# Paddock: remote Linux machines for agents

`paddock` controls full Ubuntu sandboxes (sudo, Docker inside, XFCE desktop, Claude Code/Codex/OpenCode preinstalled) on the user's self-hosted server. The CLI is already logged in (check with `paddock whoami`). Box IDs look like `bx_xxxxxxxxxxxx`. User is `abc`, home is `/config`.

## Lifecycle

- `paddock new --type small --ttl 3600` creates a box that auto-stops after an hour. `--idle-stop 30m` enables idle stop (Go durations from 5m to 720h). Sizes are `xsmall|small|default|large|xlarge`. Use `--ttl SECONDS` for a custom timer or `--no-auto-stop` for an always-on box. Creation records the box as `current`.
- `paddock extend <id|current> --hours 2`, `--ttl SECONDS`, or `--no-auto-stop` changes its timer. Use `--idle-stop 30m` or `--idle-stop off` to set or disable idle stop.
- `paddock info <id|current>` shows box details. `paddock list` shows running boxes by default; use `--filter r|s|e` or `--all` to choose a state group or show all boxes.
- `paddock current` prints the most recently created or used box ID. Lifecycle commands accept `current` in place of an ID.
- `paddock stop <id|current>` snapshots and stops the box; `paddock resume <id|current>` resumes it. `paddock delete <id|current> --yes` permanently deletes it.
- `paddock pull <id> <path> [-o dest]` downloads a file or extracts a directory from a running sandbox.

## Commands

```sh
paddock list [--filter r|s|e...] [--all]
paddock new [--type xsmall|small|default|large|xlarge] [--ttl SECONDS|--no-auto-stop] [--idle-stop DURATION] [-e K=V]... [--setup-file FILE] [--from NAME] [--environment NAME] [--no-env]
paddock info <id|current>
paddock extend <id|current> --hours N|--ttl SECONDS|--no-auto-stop|--idle-stop DURATION|off
paddock exec <id|current> [--user NAME] [--cwd DIR] [--env K=V]... -- bash -lc '<command>'
paddock exec <id|current> --detach -- <command>
paddock ps <id|current>
paddock logs <id|current> <processId> [-f]
paddock kill <id|current> <processId> [--signal TERM]
paddock restore <id|current> <snapshotId>
paddock drive create|list|rm|fork
paddock user add|list|rm <id|current> [name]
paddock group add <id|current> <group>
paddock group member add|rm <id|current> <group> <user>
paddock push <id> [dir] [--to path]
paddock prompt [id|current] "task" --provider claude|codex|opencode|pi|prime|kimi [--stop-when-done]
paddock conversations <id|current>
paddock snapshots <id|current>
paddock snapshot pull <snapshotId|boxId> [-o file.tar.gz]
paddock snapshots rm <boxId> <snapshotId>
paddock snapshot rm <name|snapshotId>
paddock usage <id|current>
paddock stop <id|current> / paddock resume <id|current>
paddock fork <id|current> [--ttl SECONDS|--no-auto-stop]
paddock delete <id|current> --yes
paddock dashboard [id|current]
paddock forward <id|current> <port> [--local-port N]
```

Agent credentials are supplied through an Environment attached to the box. Claude uses `ANTHROPIC_API_KEY`; Codex uses `OPENAI_API_KEY`; OpenCode uses the key for its selected model provider; Pi accepts provider keys such as `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and `OPENROUTER_API_KEY`; Kimi Code uses `KIMI_MODEL_API_KEY` with `KIMI_MODEL_NAME` for headless API-key access, or its saved login; Prime Agent accepts provider keys such as `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and `OPENROUTER_API_KEY`.

Use `paddock push <id> [dir] [--to path]` to copy a repo into a box. The default destination is `/config/<directory name>`. Don't copy `node_modules` or base64-encode files through `exec`.

SDKs: [TypeScript](../../sdk/typescript/README.md) · [Python](../../sdk/python/README.md).

## Working in a box

- Reuse a box: run `paddock current` or `paddock list` first and reuse the running box. Don't create a second box unless the user asks.
- `paddock push <id> .` lands the repo in `/config/<directory name>`. For a fixed path use `paddock push <id> . --to /config/project`, and `cd` there in every `exec`.
- Ports 3000, 3001 and 7070 are taken by the box (desktop, boxd). Serve apps on 8080 or another port, then run `paddock host <id> <port>`.
- Long-running servers: start them with `paddock exec <id> --detach -- <command>`, then check them with `paddock ps <id>` and `paddock logs <id> <processId>`.
- Re-run the tests after every push and report the final exit code.

## Run a repo's tests

```sh
paddock new --type small --ttl 3600
paddock push <id> .
paddock exec <id> -- bash -lc 'cd /config/<name> && <install> && <test>'
```


Run `paddock --help` for all commands. Add `--json` before a command for
machine-readable output; JSON output is automatic when stdout is not a TTY.

## Tags, network, and sources

- `paddock new --tag KEY=VALUE` tags a box; `paddock list --tag KEY=VALUE` (or `--tag KEY` for any value) filters, and all filters must match. `paddock update <id|current> --tag K=V` merges tags; `--clear-tags` removes them.
- `paddock new --network allow-all|deny-all|allowlist` sets outbound policy. `--allow-domain HOST` and `--allow-cidr CIDR` (repeatable) need `allowlist`; `--deny-cidr CIDR` blocks in every mode. `paddock update <id|current> --network ...` applies the same policy to a live box.
- `paddock new --git URL [--revision REF] [--depth N] [--git-token-env VAR] [--path DIR]` clones a repo on first boot; `--tarball URL` extracts a `.tar.gz` instead. The two are mutually exclusive.

## Detached commands

- `paddock exec <id|current> --detach -- <command>` starts a background process and prints its processId. `paddock ps <id>` lists them; `paddock exec <id> --status PID` shows one.
- `paddock logs <id> <processId> [-f]` prints output; `-f` follows until the command exits.
- `paddock kill <id> <processId> [--signal TERM|KILL|INT|HUP|QUIT|USR1|USR2]` signals it.
- `exec` and `logs -f` exit with the remote command's exit code; other failures exit 1.

## Desktop and GUI apps

- The box runs an XFCE desktop on display `:1`. To launch a GUI app on it: `paddock exec <id> --desktop --detach -- ./my-app`. `--desktop` sets `DISPLAY=:1` and the session's bus and runtime variables; it fails with `desktop_not_running` if the session is down, and it runs as user `abc` only.
- `paddock screenshot <id> -o shot.png` captures the full desktop to a PNG and downloads it. Without `-o` the file is `screenshot-<id>.png`. Look at it to check GUI state.
- `paddock desktop <id>` prints a desktop URL and opens it in a browser (`paddock browser <id>` opens the browser mode). Don't paste the URL into files.
- Every command accepts `--help` (or `-h`) and prints its usage without calling the server; `paddock help <command>` does the same.

## Snapshots, images, usage

- `paddock new --keep-snapshots N` (or `update`) sets how many automatic snapshots to keep (1 to 50, default 5). `paddock snapshots <id>` lists them; `paddock restore <id> <snapshotId>` rolls the box back to one.
- `paddock new --image REF` picks a container image, which must be built `FROM ghcr.io/tony1908/paddock-box`.
- `paddock usage <id|current>` prints CPU and network usage totals.

## Drives and users

- Drives are named volumes that survive box deletion: `paddock drive create|list|rm|fork`. Mount one with `paddock new --mount DRIVE:/mnt/x[:ro]` (path under `/mnt`). `drive rm` fails while a box mounts it.
- `paddock user add <id> <name>`, `user list <id>`, `user rm <id> <name>` manage isolated users; `paddock group add <id> <group>` and `paddock group member add|rm <id> <group> <user>` manage shared groups. Run as a user with `paddock exec <id> --user NAME -- <command>`.

## Usage notes

- Prefer reusing an existing ready box from `paddock list` over creating a new one; create one only when the user wants a fresh or isolated machine.
- Servers you want to expose publicly must bind `0.0.0.0`, then run `paddock host <id|current> <port>`.
- `paddock forward` exposes a box TCP port only on local `127.0.0.1`.
- Long-running processes: start them detached (`nohup … &`) so `exec` returns.
- Never delete or stop a box the user didn't ask you to; stopped boxes cost nothing and keep their disk.
- Never print API keys, desktop URLs, or tokens into files or commit them.
