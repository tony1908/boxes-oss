# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## Unreleased

### Breaking

- **SDK:** `exec.detached` in the TypeScript and Python SDKs now returns a `Command` object (with `processId`, `status`, `logs`, `kill`, and stdin methods) instead of the raw `{processId}` response. Read the ID from `command.processId`.

### Added

- Initial single-VM open-source release.
- Box tags: key/value `tags` on create, update, and fork, with `boxes list --tag` and `GET /sandboxes?tag=` filters.
- Per-box outbound network policy: `allow-all`, `deny-all`, or an allowlist of domains (including `*.example.com`) and IPv4 CIDRs, plus `denyCidrs` that win over every mode. Changes apply to a running box at once.
- Agent DNS forwarder on the `boxes-net` gateway. Allowed names are forwarded and each returned IPv4 address is opened for that box; other resolvers are blocked.
- Cloud metadata address `169.254.169.254` is blocked for every box.
- Create a box from a git repository (`--git`, with revision, depth, path, and a token from an environment variable) or a `.tar.gz` (`--tarball`), extracted on first boot.
- Detached command control: `boxes logs -f` streams output until exit, `boxes kill --signal`, stdin for detached commands (`stdin: true`), and `boxes ps` to list them.
- `keepSnapshots` (1 to 50, default 5) per box on create and update, and `boxes restore` to roll a box back to one of its own snapshots.
- Image per box with `boxes new --image`.
- Usage totals (`cpuSeconds`, `rxBytes`, `txBytes`, plus a live counter) in `boxes usage` and `GET /sandboxes/{id}/usage`.
- Drives: named persistent volumes mounted under `/mnt` with `--mount DRIVE:PATH[:ro]`, managed with `boxes drive`.
- Per-box Linux users and shared groups (`boxes user`, `boxes group`) and `exec --user`.
- `boxes exec --desktop` runs a command in the box desktop session so GUI apps appear on the desktop, and `boxes screenshot` saves the desktop as a PNG.
- `--help` (or `-h`) on every command, and `boxes help <command>`.
- `INSTALL_KIMI` build argument for the tools image, so it builds where `code.kimi.com` is unreachable.

### Fixed

- Ports 3000, 3001, and 7070 are reserved for `host` because the box uses them for its desktop and boxd (`reserved_port`).
- OpenCode and Codex failures pass the provider's real error through instead of a generic message.
- Activity logs redact only secret-looking `NAME=value` pairs in command previews (for example `TOKEN=`); other values such as `PORT=8080` stay readable.
- `boxes exec` waits for the command inside the box, so output is not cut and the exit code is kept; named users run through `setpriv --init-groups`.
- Setup scripts can read their config, drives are writable, snapshot retention applies on every capture, and DNS allowlist history is kept.
- The CLI `current` box is set only when the server returns a valid box ID.
