# AGENTS.md

## Model routing

| Work | Model | Effort | Where |
|------|-------|--------|-------|
| Thinking: planning, design, validation, review, debugging root cause, anything effort-heavy | Claude Opus 5.5 (`claude-opus-5-5`) | medium | Claude Code (main session) |
| Execution: implementing a settled plan, edits, running commands, low-effort tasks | Claude Haiku 4.5 (`claude-haiku-4-5-20251001`) | low | Claude Code, in-session subagent |

Flow: Opus plans → Haiku executes → Opus validates the result. Everything runs inside Claude Code. Codex/Luna and OpenCode are no longer used.

Run an execution task by spawning the `executor` subagent from the main session with the Agent tool. The agent is defined in `~/.claude/agents/executor.md` (model `haiku`, effort `low`, tools: Bash, Read, Edit, Write, Glob, Grep).

- If the `executor` agent type isn't listed in the session, spawn `general-purpose` with `model: "haiku"` and paste the executor rules into the prompt.
- The owner has asked for this (2026-10-07), so spawning the executor for implementation work needs no further confirmation.
- There is no "Haiku 5.5"; Haiku 4.5 is the newest Haiku.
- Haiku at low effort needs tight prompts: one focused task per run, the absolute repo or worktree path, exact files, exact acceptance commands. Split large phases into several runs.
- For parallel tasks, give each executor its own git worktree (`isolation: "worktree"` or an explicit worktree path).
- The executor leaves changes uncommitted; Opus reviews the diff, runs the gates itself, and commits.
- Stop after about 2 failures in a row and tell the user. Don't retry the same failing call.

**If you are the Haiku executor reading this, you ARE the executor:** implement the task you were given directly. Don't brainstorm, don't write plans, don't ask questions, and never spawn another agent.

## Project
boxes-oss is an open-source self-hosted sandbox platform for AI agents, running all boxes on one VM.
This edition has no extra nodes, node management, autoscaling, or cloud providers.
Gate: `go build ./... && go vet ./... && go test ./...`
