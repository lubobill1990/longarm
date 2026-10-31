# Steerable runs (docs/steerable-runs.md)

## Per-user isolation workers

`Submit` refuses `steer` when the worker runs per-user isolation
(`internal/server/steer.go` `steerSpec`): the hooks run as the user's
account and can't append to `<job dir>/steer/events.jsonl` (job dirs are
not writable by the account), and the shim refuses `RunAs` too
(`internal/shim/steer.go` `prepareSteer`). Put the events file in a
directory the account owns (the space's tmp, `platform.SecureUserDir`) and
have the shim read it from there.

## Agents inside a wrapper script

`--steerable` needs the agent as the job's command (`steer.AgentOf`).
Scripts that set up a checkout and then run the agent (e.g. a dispatch
script around `claude -p`) can't use it. Options: split the script (setup
job, steerable agent job, verification job), or a worker-side command a
script can call (`longarmd steer -- claude ...` attached to the job's
terminal).

## MCP `run` tool

`longarm mcp`'s run tool has no `steerable` parameter; agents driving
Longarm through MCP can't start steerable runs. Add it next to
`agent_auth` (`cmd/longarm/mcp.go`) with the `steerable-run` feature check.

## Claude's own `--settings`

`steer.Interactive` refuses `--settings` because the run passes its hooks
file with it. Merge instead: read the user's settings file on the worker
and write one with both (the hooks appended to theirs).

## StopFailure payload

The failure reason of Claude's `StopFailure` hook is read from `error`,
`error_message`, `message` or `reason` (`steer.ParseEvent`) - not verified
against a real failed turn. Check with a turn that hits an API error and
keep only the field Claude sends.

## macOS / Linux agents

Verified on Windows only (ConPTY, hooks through cmd / PowerShell). On Unix
the hook commands run in bash / sh with the same double-quoted paths;
run a real Claude Code and Copilot CLI steerable job on a Linux worker.
