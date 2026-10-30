---
name: longarm
description: Run work on remote machines with the Longarm CLI (`longarm`) or its MCP server (`fleet_*` tools) - offload builds, tests and `claude -p` / `copilot -p` agents to workers, attach to or steer remote terminal sessions, move (hand off) a Claude Code / Copilot CLI conversation and its uncommitted code to another machine, connect workers' ports to each other or to localhost, and diagnose sign-in, hub or worker problems. Trigger on "longarm", "worker", "run it on <machine>", "dev box", "offload", "hand off / migrate this session", "port link", "forward a port from a worker". Not for local-only commands or for developing Longarm itself.
---

# longarm

`longarm` is the client. A **hub** (`longarm-hub`: sign-in, CA, node list, job index, scheduler, link grants) is the control plane only; **workers** (`longarmd`) run jobs and sessions in shim processes that survive disconnects and daemon restarts. Clients reach workers directly (tcp, tailnet, or longnet hole punching; relays carry only ciphertext), so workers keep working while the hub is down. Windows machines may also have `<worker>-admin` (runs as SYSTEM).

- **Worker**: a name, node ID / unique ID prefix (4+ hex), `id:<prefix>` or `name:<name>`. You see your own workers, workers shared with you, and fleet workers.
- **Job**: its ID, a unique prefix/suffix (case-insensitive), or `last`.
- Relative worker paths are relative to the worker account's home. Windows workers run pwsh (attach) / `cmd.exe /c` (ssh exec).
- Check features before relying on a flag: `longarm version --json` returns a `features` list (and `longarm info <w>` the worker's).

## Choose a worker and dispatch

```sh
longarm ls -o wide                     # STATE CPU% MEM JOBS LABELS + agent sessions, disk, uptime, top procs, path
longarm exec <w> -- git status         # short sync command; --timeout 1m default; exit code mirrored
longarm run --repo . --name build -- make          # job; no worker = hub places it; prints the job id on stdout
longarm run <w> --repo . --ref <full-sha> -- make  # explicit worker; fresh worktree of the checkout incl. uncommitted+untracked
longarm logs <id> -f                   # stream (reconnects); --from N / negative = from the end
longarm wait <id>                      # exit = job's
longarm jobs --json --id <id>          # state, exitCode, reason
longarm kill <id>
```

- Use a named worker when the work depends on that machine's state (files, a running session, OS, GPU). Otherwise omit it and use `--require os=linux,pool=x`, `--type build|test|claude|copilot` (memory reservation), and `--no-queue` (fail with 75 instead of queueing).
- Without `--repo`/`--cwd`, jobs run in the worker's home directory, not in your checkout.
- Secrets: `--secret-env NAME` (memory-only, redacted). `-e K=V` is stored in job.json.
- Use separate worktrees (`--repo`) for parallel agents.

Details: [reference/jobs.md](reference/jobs.md).

## Sessions: attach, steer, take over

```sh
longarm session start <w> -s fix --repo . --json -- claude   # detached, idempotent by name
longarm session send <w> -s fix --text "run the tests" ; longarm session send <w> -s fix --key enter
longarm logs -s fix -f                 # transcript (raw terminal output, secrets redacted)
longarm sessions --json                # all workers; lastInput/lastInputSource/inputSeq show whether a human typed
longarm attach <w> -s fix              # a human takes over (detach: Ctrl-] d); several clients may attach
longarm session stop <w> -s fix --grace 10s
```

Use `--if-idle-ms 2000` so you don't type over a human (exit 75 = they typed recently). Details: [reference/sessions.md](reference/sessions.md).

## Hand off a Claude Code / Copilot CLI session

```sh
longarm agent-sessions [<w>|local] [--repo-path DIR] --json
longarm handoff --to <w> --latest                     # this checkout's newest session -> <w>, then attach
longarm handoff --from <w1> --to <w2> --session <id> --no-attach --git-auth forward
longarm handoff --from <w> --to local --session <id>  # new worktree next to your checkout; agent runs here
```

Moves the HEAD plus uncommitted and untracked files (a snapshot commit; your branch, index and files are untouched) and the transcript, then resumes with `--resume <id>`. Running processes are not carried over. Exit 75 means the agent still has the session open locally (exit it first) or the source session did not go idle. Only your own machines. For `--via remote` (a temporary branch on the git remote) and the safety rules, see [reference/handoff.md](reference/handoff.md).

## Ports between workers / to localhost

```sh
longarm forward <w> 3000 [localPort] [--bg]           # worker's 127.0.0.1:3000 on your localhost (--list, --stop P|all)
longarm link create <wb>:8080 --on <wa> --port 18080 [--ttl 8h] [--job <id>]   # wa's 127.0.0.1:18080 -> wb's 8080, worker to worker
longarm link ls --json ; longarm link rm <link-id>
```

Jobs started on `wa` afterwards get `LONGARM_LINKS` and `LONGARM_LINK_<WORKER>_<PORT>`. E2E recipes: [reference/ports.md](reference/ports.md).

## Credentials for remote agents and git

- `--agent-auth auto|claude|copilot|gh|none` (run/exec/session start/attach): forwards this machine's agent credentials, memory-only. `auto` only looks at `argv[0]`. For `pwsh -c "claude ..."`, set `--agent-auth claude`.
- `--git-auth forward` (run/session start `--repo`, sync, handoff): the worker fetches from the https remote with your short-lived credential. Then only the unpushed delta is shipped.
- Unattended agents need permission flags (`claude -p --dangerously-skip-permissions`, `copilot -p ... --yolo --no-ask-user`). longarm doesn't add them.

Details: [reference/credentials.md](reference/credentials.md).

## Exit codes (automation contract)

| code | meaning | do |
|---|---|---|
| 0 | ok | |
| 2 | usage / invalid request / worker too old; nothing submitted | fix, don't retry |
| 3 | not found (job, session, file, worker) | |
| 69 | `jobs --hub-only`: no hub answer | retry / drop `--hub-only` |
| 75 | capacity, quota, rate; `--if-idle-ms` refused; handoff source busy | retry later |
| 77 | not allowed, sign-in expired, no credential for `--git-auth` | `longarm login` / fix rights |
| 137 / 254 | job killed / lost (`wait`, `--wait`) | |
| 255 | unreachable, timeout, internal | outcome may be unknown: `jobs --id`, or rerun with the same `--id` |

`exec`, `wait`, `run --wait/--follow` mirror the job's code, so a job can return these codes too. To get longarm's own code, use `run` without `--wait`. It prints the id on stdout exactly when the job was submitted.

## When something fails

`longarm doctor [--json]` prints a checklist, and each failing line ends with `next: <command>`. Show that command to the user; it may name another machine or need elevation. Also: `longarm whoami`, `longarm info <w>`, `longarmd doctor` on the worker, `longarm diag [--upload]`. See [reference/diagnostics.md](reference/diagnostics.md).

## MCP equivalents

`longarm mcp` exposes `fleet_status`, `fleet_exec`, `fleet_job_submit|wait|logs|kill`, `fleet_read`, `fleet_write`, `fleet_sync`, `fleet_forward`, and `fleet_link`. Prefer them when they are registered. The CLI is needed for sessions, handoff, attach, `doctor`, and `agent-sessions`. `secret_env` names only work if they are allowed in `LONGARM_MCP_SECRETS`.
