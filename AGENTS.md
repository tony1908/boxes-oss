# AGENTS.md

## Model routing

| Work | Model | Effort | Where |
|------|-------|--------|-------|
| Thinking: planning, design, validation, review, debugging root cause, anything effort-heavy | Claude Opus 5.5 (`claude-opus-5-5`) | medium | Claude Code |
| Execution: implementing a settled plan, edits, running commands, low-effort tasks | Luna 6 (`gpt-6-luna`) | high | Codex CLI |

Flow: Opus plans → Luna executes (Codex) → Opus validates the result. OpenCode is no longer used.

Run an execution task with Codex. Write the prompt to a file and **feed it on stdin** (`-`); a prompt passed only as an argument can hang waiting on stdin:

```sh
codex exec -m gpt-6-luna -c model_reasoning_effort=high --color never \
  -s workspace-write -c sandbox_workspace_write.network_access=true \
  -c "sandbox_workspace_write.writable_roots=[\"$HOME/Library/Caches/go-build\",\"$HOME/go/pkg/mod\",\"$HOME/.docker\"]" \
  - < prompt.txt
```

- Always pass `-m` and `-c model_reasoning_effort=high` (owner's choice; `xhigh` was tried and not preferred, `max` is silently ignored), because `~/.codex/config.toml` defaults to a different model.
- `network_access=true` is needed for Go module downloads, `pnpm`, and SSH to the test droplet.
- The extra writable roots are needed for Go's build cache and module cache and for the Docker Desktop socket.
- For parallel tasks, run each in its own git worktree.
- Stop after about 2 failures in a row and tell the user. Don't retry the same failing call.

**If you are the Luna/Codex executor reading this, you ARE the executor:** implement the task you were given directly. Don't brainstorm, don't write plans, don't ask questions, and never spawn another agent, codex or opencode.

## Project
boxes-oss is an open-source self-hosted sandbox platform for AI agents, running all boxes on one VM.
This edition has no extra nodes, node management, autoscaling, or cloud providers.
Gate: `go build ./... && go vet ./... && go test ./...`
