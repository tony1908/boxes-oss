# AGENTS.md

## Model routing

| Work | Model | Effort | Where |
|------|-------|--------|-------|
| Thinking: planning, design, validation, review, debugging root cause, anything effort-heavy | Claude Opus 5.5 (`claude-opus-5-5`) | medium | Claude Code |
| Execution: implementing a settled plan, edits, running commands, low-effort tasks | Claude Haiku 4.5 (`claude-haiku-4-5-20251001`) | low | Claude Code headless (`claude -p`) |

Flow: Opus plans → Haiku executes → Opus validates the result. Codex/Luna and OpenCode are no longer used.

Run an execution task with headless Claude Code. Write the prompt to a file and **feed it on stdin**, from the repo (or worktree) the task should change:

```sh
claude -p --model claude-haiku-4-5-20251001 --effort low \
  --permission-mode acceptEdits \
  --allowedTools "Bash,Edit,Write,Read,Glob,Grep" \
  < prompt.txt
```

- Always pass `--model` and `--effort low` (owner's choice, 2026-10-07). There is no "Haiku 5.5"; Haiku 4.5 is the newest Haiku.
- `--allowedTools` must include `Bash`, or the executor cannot run builds and tests in headless mode.
- Haiku at low effort needs tighter prompts than Luna did: one focused task per run, exact files, exact acceptance commands. Split large phases into several runs.
- For parallel tasks, run each in its own git worktree.
- The executor leaves changes uncommitted; Opus reviews the diff, runs the gates itself, and commits.
- Stop after about 2 failures in a row and tell the user. Don't retry the same failing call.

**If you are the Haiku executor reading this, you ARE the executor:** implement the task you were given directly. Don't brainstorm, don't write plans, don't ask questions, and never spawn another agent.

## Project
boxes-oss is an open-source self-hosted sandbox platform for AI agents, running all boxes on one VM.
This edition has no extra nodes, node management, autoscaling, or cloud providers.
Gate: `go build ./... && go vet ./... && go test ./...`
