# Jobs: placement, workspaces, output

## Placement

- No worker (or `--on auto`): the hub picks the best-scoring worker that passes the gate. The gate needs free memory of at least min(20 GB, 25% of RAM) and CPU below 70%. The score counts free memory minus reservations, fewer jobs, an existing mirror of `--repo`, and warm dependencies. When no worker passes, the job queues; with `--no-queue` it fails with exit 75. Without a reachable hub, longarm picks from the known workers itself.
- `--mem-gb` reserves memory; the default depends on `--type`: claude/copilot 2, build 8, test 4, otherwise 2. `--cpus` and `--mem-limit 8G` cap the process tree (Windows; Linux with per-user isolation).
- `--require k=v[,k=v]`: labels (`longarm ls` LABELS, `longarm worker label <w> k=v` on your own workers).
- A worker the hub has seen offline for more than 2 minutes fails at once with exit 255. `--force-dial` dials it anyway. A user-session Windows worker runs only while its owner is logged on.

## Workspaces

| flag | effect |
|---|---|
| `--repo .` | fresh worktree of the local checkout (commits the worker lacks + uncommitted diff + untracked files); job stays `preparing` until ready |
| `--repo https://...` | worker clones/fetches it (no prompts; use `--git-auth forward` or a `GH_TOKEN` secret) |
| `--ref HEAD\|branch\|tag\|<full sha>` | short SHAs are refused (exit 2); uncommitted changes only travel with HEAD |
| `--lfs skip\|ship\|fetch` | LFS files are pointers by default |
| `--cwd DIR` | relative to the worktree with `--repo`, else absolute/home-relative |
| `-s/--session NAME` | run in that session's worker + working directory |

`longarm sync . <w>:<path>` (or `<w>:` = `<home>/longarm-sync/<repo>`) mirrors the checkout into a persistent worktree. It resets tracked changes and untracked files there and keeps ignored files (node_modules, build output).

Files: `longarm put <local>... <w>:<path>`, `longarm get <w>:<path> <local>` (both resumable and sha256-verified). `longarm tail <w> <path> -f --json` streams a growing file (one `{"offset","next","line"}` per line).

## Output and state

- `run` prints the job ID on stdout only when it submitted the job. Hints go to stderr, and only on a terminal.
- `--stdin-file F|-` sends at most 1 MiB as stdin. It is memory-only on the worker and never stored (good for prompts).
- `logs <id> [-f] [--from N]`: byte offsets. A negative offset counts from the end.
- `jobs [--state running] [--mine|--all] [--limit N] --json` returns `{"jobs":[{"worker","workerId","job":{spec,state,exitCode,reason}}],"source":"hub"|"workers","unreachable":[...]}`. A missing job is authoritative only with `"source":"hub"`. `--hub-only` exits 69 when the hub doesn't answer.
- Idempotent retry: `run --id <ulid>` with the same spec returns the existing job. A different spec under the same id gives exit 2 (`AlreadyExists`).
- If workspace preparation fails or the program doesn't start, the job ends `failed` with code 126 or 127. `run` itself still exits 0.
- `--share team|team:<name>` lets team members read the job.

## Unattended agent jobs

```sh
longarm run <w> --repo . --stdin-file prompt.md --wait --name fix-x -- \
  claude -p --dangerously-skip-permissions --output-format stream-json --verbose --session-id <uuid>
# continue that conversation: ... -- claude -p --resume <uuid> ...
longarm run <w> --repo . --wait -e COPILOT_AUTO_UPDATE=false -- \
  copilot -p "$(cat prompt.md)" --yolo --no-ask-user --no-color
```

Narrower permissions: `--permission-mode acceptEdits` or `--allowedTools ...` for Claude; `--allow-all-tools` or `--allow-tool`/`--deny-tool` for Copilot.
