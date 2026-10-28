# Longarm usage reference

The full command and configuration reference. For an overview and a quick
start see the [README](../README.md); the architecture is in
[design.md](design.md).

## Build

```sh
go build -o bin/ ./cmd/...                       # longarmd, longarm, longarm-hub, longarm-relay, longarm-tray (.exe on Windows)
GOOS=linux GOARCH=arm64 go build -o bin/linux-arm64/ ./cmd/...
go test ./...                                    # unit + end-to-end (Windows: ~2.5 min)
```

Requires Go ≥ 1.27.3 (tailscale.com needs it; with the default
`GOTOOLCHAIN=auto` an older go downloads it automatically).
Regenerate gRPC code after editing `proto/`: `buf generate` (needs `buf`,
`protoc-gen-go`, `protoc-gen-go-grpc` from `go install`).

The web UI (`web/`, Vite + React + TypeScript) is embedded into `longarm-hub` and
`longarm-tray` from the committed build in `internal/webui/dist`, so `go build`
needs no node. After editing `web/`: `go generate ./internal/webui` (runs
`web/build.mjs`: `pnpm install --frozen-lockfile`, `vite build`, copy). Frontend
tests: `cd web && pnpm test` (vitest); browser e2e: see "Web UI" below.

`longarm-tray` uses Wails v3: on Windows it builds without cgo; on Linux/macOS a
plain `go build` (cross-compiled, `CGO_ENABLED=0`) gets a fallback that opens
the UI in the browser. For the real tray there, build natively with cgo:

```sh
# Linux (Wails v3 links GTK 4 + WebKitGTK 6.0 + libsoup 3, e.g. Ubuntu 24.04):
#   sudo apt install build-essential pkg-config libgtk-4-dev libwebkitgtk-6.0-dev libsoup-3.0-dev
#   distros without WebKitGTK 6.0: GTK 3 + WebKit2GTK 4.1, build with -tags traynative,gtk3
#   (sudo apt install build-essential pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev)
# macOS: xcode-select --install
CGO_ENABLED=1 go build -o bin/ ./cmd/longarm-tray                     # macOS
CGO_ENABLED=1 go build -tags traynative -o bin/ ./cmd/longarm-tray    # Linux (opt-in, so `go test ./...` works without GTK)
# the e2e tests build longarm-tray without cgo outside Windows (headless); LONGARM_TRAY_CGO=1 builds the native one
```

## Quick start (one admin machine, one worker)

```sh
# on the admin/client machine
longarm admin ca init                                     # ~/.config/longarm/ca
longarm admin issue --role admin --name me --install      # client cert for this longarm (admin role)
longarm admin issue --role worker --name w1 --out ./w1-tls
# copy ./w1-tls to the worker as <longarmd data dir>/tls  (default data dir: ~/.longarm; root: see "Data directory")

# on the worker
longarmd run --listen tcp://$(tailscale ip -4):7701       # foreground
# or as a service running as a normal user account (Windows, elevated prompt):
#   $env:LONGARM_SERVICE_PASSWORD = '...'; longarmd service install --user .\me --listen tcp://100.x.y.z:7701
#   longarmd service start
```

### Data directory

longarmd keeps its certificate (`tls/`), hub address (`hub.toml`), config
(`longarmd.toml`), jobs, sessions and mirrors in one data dir. `--data-dir`
and `LONGARM_DATA_DIR` always win; otherwise:

| longarmd runs as | default data dir |
|---|---|
| a normal account (any OS) | `~/.longarm` |
| root on Linux | `/var/lib/longarmd` (also the deb's `longarmd-peruser.service`) |
| root on macOS | `~/.longarm` (`/var/root/.longarm`; the pkg passes `--data-dir` explicitly) |
| LocalSystem on Windows | `C:\ProgramData\Longarm\longarmd` (also the MSI's `DATADIR`, which sets `LONGARM_DATA_DIR` system-wide) |
| an elevated administrator on Windows | `~\.longarm`, unless only `C:\ProgramData\Longarm\longarmd` holds an enrollment |

Don't put the data dir under a `.claude` directory (e.g. a project's
`.claude/worktrees/...`): Claude Code treats every path under `.claude` as
protected and denies writes there, so Claude Code jobs could not edit their
worktrees (`<data>/worktrees`). `longarmd join`, `longarmd login` and
longarmd's start warn about it.

So `sudo longarmd join ...` and `sudo longarmd service install ...` agree
without flags. Existing installs keep working: as long as `/var/lib/longarmd`
holds no enrollment (`tls/`, `hub.toml` or `longarmd.toml`) but root's
`~/.longarm` does (enrolled by an older longarmd), root's default stays
`~/.longarm` (the same for LocalSystem's profile on Windows). As root /
LocalSystem, "`~`" is the account's own home from the passwd database /
its profile, never `$HOME` / `%USERPROFILE%` (`sudo -E` keeps the invoking
user's).

The system dir is created locked down (Linux 0700 root; Windows owner
Administrators, a protected DACL for SYSTEM and Administrators). An
existing one is checked first - ProgramData lets every local user create
directories, so `C:\ProgramData\Longarm\longarmd` may have been pre-created
by somebody else: owned by another account (Linux: not root, or writable by
group/others) it is refused with a message naming it - check and remove it,
then retry. On Windows one that inherits its ACL or grants Everyone, Users
and the like more than traverse rights is locked down again (its contents
must be owned by SYSTEM, Administrators or the installing account, else it
is refused too); the same goes for the `...\Longarm` parent.

`longarmd service install` picks the service's dir like this (`--dry-run`
prints it and changes nothing):

- `--data-dir` / `LONGARM_DATA_DIR`: as given;
- `--isolation per-user`: the system dir (`/var/lib/longarmd`,
  `C:\ProgramData\Longarm\longarmd`, created locked down). If it holds no
  enrollment yet but the default dir or `~/.longarm` does (e.g. a `sudo
  longarmd join` by an older longarmd), the enrollment is **copied** over
  (tls/, hub.toml, longarmd.toml, ssh host key, authorized_keys, tailnet
  keys/state; files 0600, dirs 0700, on Windows with the system dir's ACL)
  and the old dir is named so you can remove it. The source and everything
  copied must be owned by root / SYSTEM / Administrators (or the installing
  account) and not be writable by group or others, else nothing is copied;
  symlinks are skipped. The copy is staged in the system dir and moved into
  place at the end, so a failed one leaves nothing behind and can be
  retried. An existing service of the same name must be uninstalled first;
- `--user NAME` as root on Linux/macOS (shared mode): NAME's `~/.longarm` -
  enroll as that account (`sudo -u NAME longarmd join ...`); an enrollment
  only root's dir holds is refused (the account could not read it). Windows
  keeps the invoking account's default;
- otherwise the default above.

Per-user isolation also needs every parent of the users dir (`<data>/users`
or `[isolation] users_dir`) traversable by the accounts (Unix `o+x`), which a
home like `/root` (0700) is not: `service install` refuses such a dir with a
message naming the blocking directory, and longarmd refuses to start on it.

`~/.config/longarm/workers.toml` on the client:

```toml
[[worker]]
name = "w1"                     # must match the worker certificate name (or set cert_name)
address = "100.64.0.11:7701"
transport = "tcp"               # or "tailscale" (embedded tsnet node; address = tailnet name)

# [tailscale]                   # only for transport = "tailscale"
# hostname = "longarm-client-mybox"  # auth key from $TS_AUTHKEY on first use
# control_url = ""              # Tailscale SaaS by default; Headscale URL possible
#                               # (longarm join with a hub that has a [tailnet] provider sets this up)
```

## Usage

```sh
longarm ls                                         # CPU / memory / jobs / claude+copilot sessions of all workers
longarm exec w1 -- git status                      # short synchronous command, exit code mirrored
longarm run w1 --name build --cwd C:/src/app --env CI=1 --secret-env GH_TOKEN -- pnpm test
longarm logs w1 <id> -f --from 0                   # follow from a byte offset; reconnects automatically
longarm wait w1 <id>                               # exit code = job's (killed 137, lost 254; see "Exit codes")
longarm kill w1 <id>                               # kills the whole process tree
longarm jobs w1 [--state running] [--json] [--id <id>] [--hub-only]
longarm run w1 --stdin-file prompt.txt -- claude -p # the file is the job's stdin, never stored
longarm version --json                             # version + features (see "Capabilities")
longarm session start w1 -s a1 --json -- claude    # detached session for programs (see "Sessions for programs")
longarm tail w1 path/to/events.jsonl -f --json     # stream a growing file
longarm admin shutdown w1                          # admin role only; jobs keep running
longarm logs last -f                               # your most recent job; a job is also a unique prefix or
longarm kill 7K3M9Q2A                              #   suffix of its id (the tray shows the last 8 characters)
longarm logs -s a1 [-f]                            # a session's transcript (also: logs w1 a1 --session)
longarm jobs [--mine | --all] [--limit N]          # the newest 20 (0 = all); admins see their own unless --all
longarm doctor [--json]                            # checklist: sign-in, hub, daemon, firewall, your workers
```

A job id on the command line (`logs`, `wait`, `kill`, `jobs --id`) is the
full id, a unique prefix or suffix of it (case-insensitive), or `last` - your
most recent job. A short id that matches several jobs is an error listing
them (exit 2); one that matches none is used as given (so an unknown id is
still exit 3). A full ULID (what `run` prints) is used as is, without a
lookup.

`run` on a terminal says how to follow the job (`longarm logs <id> -f`) and,
when started inside a git checkout without `--repo` or `--cwd`, that the job
runs in the worker's home directory (`--repo .` runs it in a copy of the
checkout). These hints go to stderr, and only on a terminal.

A worker the hub reports offline for more than two minutes fails at once
(exit 255): `worker pc1 is offline (the hub last saw it 2h ago)`, plus, for a
Windows worker of one's own, that a user-session worker runs only while its
owner is logged on. `--force-dial` (or `LONGARM_FORCE_DIAL=1`) dials it
anyway; a worker that went away just now is still dialed, and when no leg
connects the error starts with the same note.

`tail --from N` is `tail --from-offset N` (like `logs --from`); `worker rm`
is `worker remove`; `exec` / `run --session` also take `-s`.

Secrets (`--secret-env NAME` or `NAME=LOCALVAR`) are read from longarm's own
environment, sent over mTLS, kept only in the job's memory/env, and redacted
from the job log.

### Exit codes

Programs driving longarm can rely on these exit codes:

| code | meaning | what a client does |
|---|---|---|
| 0 | success | |
| 2 | usage error or a request that can't succeed as given, so nothing was submitted: unknown flag or command, wrong arguments, a short job id that matches several jobs, local validation (`--env` without `=`, a missing or too large `--stdin-file`, `--ref` a short commit id / a ref that isn't HEAD, a branch, a tag or a full commit id / a commit not in the checkout, `--lfs` other than skip/ship/fetch, `--lfs fetch` with a local checkout, `--git-auth` other than forward/none, `--git-auth forward` without `--repo` or with a URL that isn't https), and the hub's / worker's `InvalidArgument`, `FailedPrecondition`, `AlreadyExists` (a job id in use with another spec), `OutOfRange`, `Unimplemented`, a worker too old for a flag or command; `session send` to a session that has ended | fix the request; retrying it unchanged fails again |
| 3 | not found: the hub or the worker answered that the job / file / session does not exist, an unknown worker name, a job that every known worker answered it doesn't have (when some did not answer: 255), `jobs --id` of an unknown job, `sessions -s` / `session send` / `session stop` of an unknown session, `tail` of a missing file (without `-f`), `last` when you have no jobs | |
| 69 | `jobs --hub-only`: no hub configured, or it did not answer | retry later, or ask the workers (without `--hub-only`) |
| 75 | refused for capacity, quota or rate (`ResourceExhausted`): no worker passes the gate with `--no-queue`, a quota, too many attempts; `session send --if-idle-ms`: input arrived within the window (nothing sent); too many `tail`s open on the worker | retry later |
| 77 | not allowed: `PermissionDenied`, `Unauthenticated`, a refusal of end-to-end trust, an expired sign-in (`your sign-in expired on <date>: run longarm login`); `--git-auth forward` with `--repo <url>` (`run`, `session start`): no credential for the URL could be obtained on this machine (nothing was submitted) | fix the credentials or the rights |
| 137 | `wait` / `run --wait`: the job was killed; `session start`: the session was killed before it ran | |
| 254 | `wait` / `run --wait`: the job was lost (its supervisor vanished; outcome unknown); `session start` likewise | |
| 255 | anything else: worker or hub unreachable (also a worker the hub reports offline), timeouts, `Aborted`, internal errors | the outcome may be unknown: look the job up (`jobs --id`) or retry with the same `--id` |

`FailedPrecondition` is 2 because the hub and the worker use it only for
conditions that hold until someone changes the setup (a path that is a
directory, a feature the hub has off, a node that isn't approved); nothing
on the `run` path returns it once a job was accepted. Passing failures -
the worker importing or checking out an uploaded `--repo` (a git lock, a
full disk), a `put` resuming against a partial file another upload changed -
are `Aborted` (255). Problems after a job was accepted (workspace
preparation, a program that doesn't start) are the job's: `run` exits 0 and
the job ends `failed` with a reason (exit code 126 / 127).

Commands that mirror a job - `exec`, `run --wait` / `--follow`, `wait` - exit
with the job's own exit code once it ran, so there 2, 3, 69, 75 and 77 can
also be the job's. To tell them apart, don't mirror: `run` without `--wait`
exits with longarm's own code only. It prints the job id on stdout exactly
when the job was submitted (exit 0); a non-zero exit with nothing on stdout
means this call submitted nothing, and only 255 leaves it open whether the
request reached the worker. Then look the job up with `jobs --id`, or run
it again with the same `--id`: the same id and spec return the existing job
(also when the hub placed it already). With a local `--repo` every `run`
uploads the working tree anew; the upload is no part of the comparison -
what the workspace resolves to is (repository, URL, `--ref`, the commit, a
digest of the uploaded uncommitted and untracked files, `--lfs fetch`) - so
a retry with the same content returns the existing job (the new upload is
discarded by the worker's GC) and replaces a job whose stdin payload was
lost before it started, while other uncommitted or untracked content is
another job (`AlreadyExists`, exit 2), never the old job's result. Whether a credential
was forwarded (`--git-auth`) is no part of it either.
The job's own outcome comes from `jobs --json` (`state`, `exitCode`,
`reason`) or `wait`.

### Capabilities: `longarm version --json`

```json
{"version": "v1.8.0", "features": ["exit-codes", "jobs-source", "jobs-hub-only", "stdin-file", "lfs", "ref-sha", "worker-label", "git-auth-forward",
  "session-start", "session-send", "session-send-if-idle", "session-stop", "sessions-json-v2", "tail", "run-session",
  "job-id-short", "jobs-mine", "doctor-json"]}
```

(indented in the real output). A program checks `features` instead of
comparing versions; names are lowercase-hyphenated and keep their meaning.
A longarm without the `version` command (older releases: "unknown command",
exit 255) has none of these features.

| feature | what it means |
|---|---|
| `exit-codes` | the exit codes above |
| `jobs-source` | `jobs --json` has `source` (and `unreachable`) |
| `jobs-hub-only` | `jobs --hub-only` and `jobs --id` |
| `stdin-file` | `run --stdin-file` (the worker must also support it, see below) |
| `lfs` | `run` / `sync --lfs skip\|ship` |
| `ref-sha` | `run --repo <path> --ref <full commit id>` |
| `worker-label` | `longarm worker label` for owners |
| `git-auth-forward` | `run` / `sync --git-auth forward`, `run --repo <url> --lfs fetch`; the worker must also support it (`git-auth-forward` in its `Node.Info` features), see [Fetching with your credential](#fetching-with-your-credential---git-auth-forward) |
| `session-start` | `longarm session start` (detached, idempotent by name, `--repo` worktree, auto placement) |
| `session-send` | `longarm session send` (`--text`, `--stdin-file`, `--key`, `--raw`) |
| `session-send-if-idle` | `session send --if-idle-ms` (exit 75) |
| `session-stop` | `longarm session stop --grace` |
| `sessions-json-v2` | fleet-wide `longarm sessions`, `sessions -s`, `sessions --json` with `source`, `worker`, every field and the activity fields |
| `tail` | `longarm tail` |
| `run-session` | `run` / `exec --session` |
| `job-id-short` | `logs` / `wait` / `kill` / `jobs --id` take a unique prefix or suffix of a job id, or `last` |
| `jobs-mine` | `jobs --mine` / `--all` (the table's defaults: 20 rows, admins their own jobs) |
| `doctor-json` | `longarm doctor --json` / `longarmd doctor --json` (see [`doctor --json`](#doctor---json)) |
| `node-ids` | node IDs: a worker target may be a name, a node ID, a unique ID prefix, `id:<prefix>` or `name:<name>`; `longarm worker rename`, `longarm admin rename`; IDs in listings and JSON (`workerId`, `id`) - see [Node IDs](#node-ids-and-targets) |

The session and tail features also need the worker's (`session-start`,
`session-send`, `session-send-if-idle`, `session-stop`, `session-info`,
`file-tail` in `Node.Info` features; see "Sessions for programs").

### `jobs --json`

`longarm jobs --json` prints one object:

```json
{
  "jobs": [ { "worker": "w1", "workerId": "3f2a9c1e-...", "job": { "spec": { "id": "...", ... }, "state": "JOB_STATE_RUNNING", ... }, "workerOnline": true } ],
  "source": "hub",
  "unreachable": ["w2"]
}
```

`jobs` is `FleetJobsResponse.jobs` as before (protojson field names; 64-bit
numbers are strings; fields at their zero value are left out); it is `[]`
when empty (the key used to be left out). `workerId` is the worker's node
ID (left out for a worker the hub doesn't know); `worker` its name. `source` says who answered:
`"hub"` (its fleet-wide job index) or `"workers"` (the hub was unreachable
or none is configured: longarm asked the known workers directly, with a
note on stderr). `unreachable` (only with `"source": "workers"`, left out
when empty) lists the workers that did not answer. Only with `"source":
"hub"` is a missing job authoritative; from the workers it means "none of
the workers that answered has it".

- `--id <job>`: only that job. Not known: exit 3, with the (empty) list on
  stdout for `--json`. Known but not in `--state`: exit 0 with an empty
  list - exit 3 always means the job does not exist.
- `--mine` / `--all`, `--limit N`: only your own jobs / everything you may
  see, at most N. Without them `--json` lists every job you may see, as
  before; the table (without `--json`) shows the newest 20, and for an admin
  certificate only the admin's own jobs.
- `--hub-only`: ask the hub only, never the workers: exit 69 when no hub is
  configured or it does not answer (it is asked even if an earlier longarm
  found it down).

So `longarm jobs --hub-only --json --id <job>` answers "unknown job" (exit 3)
and "hub unreachable" (exit 69) apart. Workers report their jobs to the hub
as they change; a job submitted a moment ago can be missing from the index
for that moment (the `run` that printed its id is the confirmation).

### Job stdin payload (`--stdin-file`)

`longarm run --stdin-file <file>` (`-` = longarm's own stdin) sends the
file's content as the job's standard input: the job reads it, then end of
file. Unlike the command line (`argv`) and `--env`, which are stored in the
job's `job.json`, the hub's job index, the worker's audit log and its logs,
the payload is never written anywhere by longarm:

- longarm sends it only in the worker-direct submit, inside the mTLS
  connection to the worker (a longnet relay forwards only TLS records). The
  hub never gets it: placement - also when the hub queues the job - only
  picks the worker. A queued `run` waits for its placement and holds the
  payload in its own memory meanwhile; if it ends before (Ctrl-C, killed),
  nothing was submitted.
- longarmd keeps it in memory until the job's shim starts (with `--repo`,
  while the workspace is prepared) and writes it to the shim; the shim
  writes it into the job's stdin pipe. Each copy is zeroed once handed on:
  longarm's after the submit, longarmd's once the shim has it, the shim's
  once the job's pipe took it (best effort: the Go runtime and gRPC may hold
  transient buffers until they are reused).
- Job records (`job.json`, the API, `jobs --json`), the hub's index and the
  audit log carry only its size (`stdinBytes` / `stdin_bytes`); telemetry
  only whether there was one. It is never in a log, a diagnostic bundle or a
  crash report (those hold stacks, not data). What the job does with it
  (prints it, writes it to a file) is the job's business: the job's output
  is in its log and in `longarm diag` bundles.

Limits: at most 1 MiB (larger: exit 2, nothing sent). Pipe jobs only (not
sessions). The worker must announce the `job-stdin` feature
(`Node.Info`): an older longarmd is refused with exit 2 before anything is
submitted.

Failure and retries (same `--id`):

- **Payload lost before the hand-off** (longarmd restarted or the shim
  failed to start after the submit was accepted, e.g. while the workspace
  was prepared, or the shim got fewer bytes than were submitted): the job
  never runs - not with an empty or partial stdin. It ends `failed`, exit
  code 127, reason `stdin payload lost before start; resubmit`.
- **Resubmitting the same `--id` with `--stdin-file`** (same command, same
  payload size):
  - the job exists and has started, or is still preparing / running: `run`
    prints its id (exit 0); the new payload is dropped, the job keeps the
    one it got;
  - the job took a payload and ended without starting (the case above, or
    its program didn't start): the new submit replaces it - a new attempt
    with the new payload, same id;
  - another command, or a payload of another size: `AlreadyExists`, exit 2.
- Resubmitting without `--stdin-file` a job that had one: another spec,
  exit 2.

Detached jobs work as usual: `run` returns once the job was submitted.

### Interactive sessions (tmux-like)

```sh
longarm attach w1                                  # session "default": your shell (pwsh on Windows)
longarm attach w1 -s dev --secret-env GH_TOKEN -- claude   # named session running a command
#   detach: Ctrl-] then d   (Ctrl-] Ctrl-] sends a literal Ctrl-])
#   closing the terminal or losing the network also just detaches
longarm attach w1 -s dev                           # re-attach: replays recent output, then live
longarm sessions w1 [--all]                        # NAME STATE EXIT PID ATTACHED ...
longarm logs w1 dev --session [-f]                 # transcript (raw terminal output, secrets redacted)
longarm attach w1 -s dev --kill                    # kill the session's process tree
```

Sessions live in their own shim process on the worker: they survive client
disconnects and longarmd restarts/updates. Several clients can attach to one
session at once; the window size follows the most recent resize. `longarm attach`
exits with the session's exit code when its process ends. Piped (non-tty)
input is sent with newlines as Enter (CR). Programs drive sessions with
`longarm session start|send|stop` (next section).

### Sessions for programs

A program (an IDE, a desktop app) can run an interactive program - Claude
Code, Copilot CLI, a shell - in a session, drive it without a terminal, and
let a human attach to the same session at any time (`longarm attach`).

```sh
longarm session start w1 -s agent1 --repo . --json -- claude      # detached; prints {"worker","session","cwd","created","state"}
longarm session start -s agent1 --require os=windows -- claude    # no worker / auto: placed like run (or the running one is found)
longarm session send w1 -s agent1 --text "fix the tests" --key enter
longarm session send w1 -s agent1 --key esc                      # keys: enter tab shift-tab esc backspace space up down left right home end pageup pagedown delete ctrl-<letter>
longarm session send w1 -s agent1 --text "y" --if-idle-ms 2000   # only if nobody typed in the last 2 s (else exit 75)
longarm sessions --json                                           # all your workers; sessions w1 -s agent1 --json: one session
longarm run --session agent1 -f -- git status                    # one-shot command in the session's worktree
longarm tail w1 agent1-worktree/.tmia/agent-events.jsonl -f --json  # (any path get may read)
longarm logs w1 agent1 --session -f                               # the transcript
longarm session stop w1 -s agent1 --grace 10s                    # Ctrl-C, then kill after 10 s
```

**`longarm session start [<worker>|auto] -s NAME [--cwd DIR | --repo PATH|URL [--ref REF] [--lfs skip|ship|fetch] [--git-auth forward]] [--env K=V] [--secret-env NAME] [--require K=V] [--no-queue] [--type T] [--cols N --rows N] [--json] [-- command...]`**

- Creates the session without attaching (default command: the user's
  shell) and prints `<session> <worker>`; `--json` prints
  `{"worker": "w1", "session": "agent1", "cwd": "<dir on the worker>", "created": true, "state": "running"}`
  (`cwd`: the session's working directory as the worker resolved it - the
  worktree with `--repo`; `""` = the home directory).
- Idempotent by name: when a session of that name runs with the same
  command, `--cwd`, `--env`, `--secret-env` names and workspace (repo, url,
  ref, commit), start reports it with `"created": false`, exit 0. Another
  spec: `AlreadyExists`, exit 2. An ended session of that name is replaced
  (`"created": true`). With a local `--repo` the checkout's commit is part
  of the spec (uncommitted changes are not compared).
- No worker or `auto`: if a session of that name runs on one of your
  workers that answer, start uses it; otherwise the hub places the session
  like `run` (labels with `--require`, the gate, `--no-queue`: exit 75 when
  no worker passes). The memory reservation of the placement (`--type`,
  from the command) holds for the hub's reservation TTL only: the hub's job
  index does not track sessions.
- `--repo` prepares a git worktree like `run --repo` (a local checkout is
  synced; a URL is fetched by the worker, with the `GH_TOKEN` secret if
  given) at `<data>/worktrees/s.<name>.<generation>` (in the user's space
  with per-user isolation; the generation keeps a session that replaces an
  ended one of the name off the old worktree, which is removed meanwhile).
  It lives as long as the session's data: never removed while the session
  runs; removed when the ended session is collected (`[gc] keep`, `longarm
  admin gc`) or replaced by a new session of the name.
- `--git-auth forward` and `--lfs fetch` work as for `run` (see [Fetching
  with your credential](#fetching-with-your-credential---git-auth-forward)):
  with `--repo <https url>` the worker fetches with a short-lived credential
  of yours, forwarded worker-direct with the start, memory-only, used for
  that fetch only and never seen by the session's program (`sessions
  --json` shows `"gitAuth": true` only); with a local checkout the remote's
  commits are fetched by the worker and only the unpushed delta is shipped.
  No credential here: exit 77; a worker without `git-auth-forward`: exit 2.
- start returns when the session runs. If it ended before - workspace
  preparation failed (126), the program did not start (127) or it exited
  at once (its code) - start exits with that code (killed 137, lost 254)
  and the reason on stderr; `--json` still prints the object.
- `--secret-env` values are memory-only on the worker and redacted from the
  transcript (as for `attach`); `--env` is stored in the session's metadata.
  Sessions take no stdin payload: input goes through `session send`.

**`longarm session send [<worker>] -s NAME (--text TEXT | --stdin-file FILE|-) [--key KEY]... [--raw] [--if-idle-ms N] [--json]`**

- Types the bytes into the session's terminal: `--text` / `--stdin-file`
  first, then the `--key`s in order. A newline (LF or CRLF) becomes Enter
  (CR), like piped `attach` input; `--raw` sends the bytes unchanged. At
  most 256 KiB. Without a worker, the one worker with a running session of
  that name.
- Atomic and ordered: each send is one contiguous write to the terminal.
  The session's shim serializes every write - an attached client's
  keystrokes (each read from its terminal), other sends, `session stop`'s
  Ctrl-C - so a send never interleaves with a human typing; writes reach the
  terminal in the order the shim receives them. Note that a TUI may treat
  one large write as a paste (bracketed paste): send the Enter as a separate
  `--key enter` send if the program should submit.
- `--if-idle-ms N`: written only if no input (from any attached client or
  any send) reached the terminal in the last N ms - checked and written
  atomically by the shim; otherwise nothing is written and longarm exits
  **75** (retry later). Sessions started by a longarmd before this feature
  (its shim lives on across updates) refuse `--if-idle-ms` with exit 2.
- `--json` prints `{"worker", "session", "inputSeq"}` (the session's input
  sequence number after this write).
- The input is never stored by longarm: not in the session's metadata
  (`job.json`, `status.json`), not in the audit log (its size only), not in
  longarmd's logs. The terminal transcript (`logs --session`) holds what the
  terminal shows - which includes input the program echoes, as TUIs and
  shells do.
- Exit codes: 0 written (all of it); 2 the session has ended (or usage,
  or a worker without `session-send`); 3 no such session; 75
  `--if-idle-ms` refused. A terminal write that fails or is short (the
  program is exiting) is never reported as written: exit 2 when the
  session has ended meanwhile, else 255 (some bytes may have arrived).
- In shared isolation with users separated, session names are one
  namespace: a name another user holds is `AlreadyExists` (start) or not
  found (send, stop, sessions), never that user's session.

**`longarm session stop [<worker>] -s NAME [--grace 10s] [--json]`**

longarmd types Ctrl-C into the terminal - twice, a second apart (Claude
Code and similar TUIs exit on the second; a Unix terminal in cooked mode
turns it into SIGINT for the foreground process group; on Windows ConPTY
delivers it as a Ctrl-C event to console programs) - waits up to `--grace`
for the program to exit, then kills the whole process tree like `attach
--kill` (state `killed`, exit code 137). `--grace 0` kills at once. The
worker carries this out even if longarm goes away meanwhile (grace at most
10 minutes). Prints `<session> <state>` (`--json`: the session as in
`sessions --json`); an ended session is reported as it is (exit 0); exit 3:
no such session. Every step is bound to the session found first: if it
ends and a new session takes the name meanwhile (a concurrent `start`), the
new one is never sent Ctrl-C or killed, and stop reports the original as
`lost` ("ended and was replaced ... before stop saw its outcome").
`attach --kill` stays (no grace).

**`longarm sessions [<worker>] [--all] [-s NAME] [--json]`**

Without a worker: every known worker (workers.toml and the hub's list),
asked in parallel. <a id="sessions---json"></a>`--json` prints:

```json
{
  "sessions": [
    {"worker": "w1", "name": "agent1", "state": "JOB_STATE_RUNNING", "argv": ["claude"], "cwd": "", "dir": "C:\\longarm\\worktrees\\s.agent1",
     "workdir": "C:\\longarm\\worktrees\\s.agent1", "workspace": {"repo": "app", "url": "", "ref": "HEAD", "commit": "4f3c...", "stagingId": ""},
     "pid": 4242, "attached": 1, "hasExitCode": false, "exitCode": 0, "reason": "",
     "createdAt": "...", "startedAt": "...", "endedAt": null, "logSize": "18231",
     "lastOutput": "...", "lastInput": "...", "inputSeq": "57", "lastInputSource": "attach", "lastActivity": "...",
     "createdBy": "client/alice", "owner": "alice"}
  ],
  "source": "workers",
  "unreachable": ["w2"]
}
```

Every field of the session is present, also at its zero value (protojson
names; 64-bit numbers - `logSize`, `inputSeq` - are strings; times RFC 3339
or `null`), plus `worker`. `exitCode` counts only with `hasExitCode`.
`dir` is the working directory (the worktree with `--repo`), `workdir` /
`workspace` the worktree and what it was made from. Activity:
`lastOutput` (the terminal's last output), `lastInput` and
`lastInputSource` (`"attach"`: a human's keystrokes, `"send"`: `session
send`, `"stop"`), `inputSeq` (writes to the terminal so far; it only grows
while the session lives, so a controller that remembers it sees whether a
human typed since), `lastActivity` = the later of the two. Ended sessions
and sessions whose shim predates this have `lastOutput` from the
transcript's modification time and no input fields. `-s NAME`: that session
only (running; with `--all` in any state); none: exit 3 with the empty list.

The hub does not index sessions - each worker is the authority for its own
(`"source": "workers"`, always). With a `<worker>` argument its answer is
authoritative (`-s NAME` exit 3 = the worker has no such session); fleet-wide
a missing session means none of the workers that answered has it; those
that did not are in `unreachable` (and on stderr). There is no
`--hub-only`. (A hub index would need workers to report sessions like jobs:
more traffic for state - activity - that is stale the moment it is
indexed, while the worker-direct answer is current.)

**`longarm run|exec [<worker>] --session NAME [--cwd REL] -- command`**

Runs a one-shot job (or `exec` command) on the session's worker in the
session's working directory (`--cwd` relative to it; not with `--repo`).
The same as `run <worker> --cwd <dir>` with the `cwd` that `session start
--json` printed. Ended sessions keep their worktree until collected.

**`longarm tail <worker> <path> [-f] [--from-offset N] [--json]`**

Prints a worker file from a byte offset (negative: before the end) and,
with `-f`, what is appended until interrupted: a file that does not exist
yet is waited for, one that was truncated or replaced is read again from 0
(a notice on stderr), and longarm reconnects after network errors resuming
at the last byte. Paths and access are those of `longarm get` (relative to
the home directory of longarmd's account; with per-user isolation read as
the user's account); one audit record per `tail`. `--json` prints one
object per line of the file: `{"offset": 0, "next": 8, "line": "{\"e\":1}"}`
(`next` is where `--from-offset` resumes after it); a truncation gives
`{"offset": 0, "next": 0, "line": "", "truncated": true}`; without `-f` an
unterminated last line comes with `"partial": true`. A line longer than
1 MiB comes as `"partial": true` fragments of at most 1 MiB (cut between
characters, offsets contiguous), then its last part; longarm's memory stays
bounded whatever the file holds. Exit 3: no such file (without `-f`; with
`-f` when it has not appeared for an hour). Exit 75: too many tails are
open on the worker - at most 8 per user and 64 in total by default (with
per-user isolation each holds a helper process), `[files] max_tails_per_user`
/ `max_tails` in longarmd.toml (-1 = no cap).

**Terminals and attaching.** `longarm attach` works with its stdin/stdout
on a pseudo terminal - also a ConPTY on Windows, as when a desktop app runs
it in portable-pty / xterm.js: it switches the terminal to raw mode (VT
input), creates the session with the terminal's size and sends resizes
(SIGWINCH on Unix; on Windows it polls the size every 300 ms). Several
clients can attach at once (the window size follows the most recent
resize); `session send` input arriving meanwhile is serialized with theirs
as described above. longarm drops the win32-input-mode request
(`ESC[?9001h`) a Windows worker's pseudo console sends: forwarded to a
terminal that is itself a ConPTY, it made that console swallow plain
keystrokes (input stopped); plain VT input works with every terminal.

**Transcript retention.** The transcript (`longarm logs <worker> NAME
--session`, raw terminal output, secrets redacted) is `out.log` in the
session's directory, capped at longarmd's `max_log_bytes` (it rotates to
`out.log.1`; `logs` notes bytes rotated away). An ended session's
directory, transcript and worktree are kept for `[gc] keep` (then removed
by the periodic gc or `longarm admin gc`, oldest first under disk
pressure) or until a new session takes the name; a running session is never
collected.

Capabilities (`longarm version --json`): `session-start`, `session-send`,
`session-send-if-idle`, `session-stop`, `sessions-json-v2`, `tail`,
`run-session`. Workers announce theirs in `Node.Info` (`longarm info
<worker>`): `session-start`, `session-send`, `session-send-if-idle`,
`session-stop`, `session-info` (the activity and worktree fields),
`file-tail`. Against a longarmd without them `session start|send|stop` and
`tail` exit 2 ("does not support it; update longarmd") before doing
anything; `sessions` lists such a worker's sessions without the new fields.

### Port forwarding, files, code sync

```sh
longarm forward w1 3000                            # 127.0.0.1:3000 -> w1's 127.0.0.1:3000 (foreground)
longarm forward w1 5432 15432 &                    # local port 15432
longarm forward --list ; longarm forward --stop 15432   # (or --stop all)

longarm put ./dist w1:C:/www/                      # files/dirs; resumable, sha256-verified, skips identical
longarm get w1:logs/app.log .                      # relative worker paths = worker account's home

longarm sync . w1:C:/work/proj                     # commits (bundle of what the worker lacks) +
                                              # uncommitted diff + untracked files -> git worktree
longarm run w1 --repo . -f -- pnpm test            # job in a fresh worktree of the local checkout
longarm run w1 --repo . --ref 3f2a...c9 -- make    # a full commit id (40/64 hex) of the checkout, detached
longarm run w1 --repo . --lfs ship -- make         # also the commit's Git LFS objects (default: skip)
longarm run w1 --repo https://github.com/o/r --ref main --secret-env GH_TOKEN -- pnpm test
longarm run --repo https://dev.azure.com/org/p/_git/r --ref <full sha> --git-auth forward -- make
                                              # the worker fetches with your credential (below)
longarm run w1 --repo . --git-auth forward -- make  # remote first: only the unpushed delta is shipped
```

The worker keeps one bare mirror per repository (`<data>/mirrors/<repo>.git`)
and a worktree per sync target or job (`<data>/worktrees/<job-id>`); a job
with `--repo` stays `preparing` until its worktree is ready. Syncing into an
existing worktree resets it: tracked changes and untracked files there are
discarded, ignored files (node_modules, build output) are kept.

`--ref` (run and sync) takes `HEAD`, a branch, a tag or a **full** commit id
of the local checkout - a commit on no branch is shipped too (through a
temporary ref, removed afterwards) and checked out detached; a short commit
id is refused with the full one in the message. Uncommitted changes travel
with `HEAD` only.

**Git LFS.** A shipped checkout's LFS content is not in its git objects, and
the worker's mirror has no LFS server to fetch it from, so worktrees are
checked out with the LFS smudge skipped: LFS files are **pointer files** by
default (`--lfs skip`; longarm notes it when the commit uses LFS, and the
job's `out.log` says so). `--lfs ship` (needs git-lfs on both sides) also
sends the commit's LFS objects the worker lacks from the local LFS store
(`git lfs ls-files` of the commit; objects not fetched locally are reported
and stay pointers) into the mirror's LFS store, and `git lfs checkout` fills
in the files. LFS attributes count in any `.gitattributes`, nested ones
included. The mirror's LFS filter is set to skip the smudge for the
job's own git too (a later checkout doesn't fail on missing content);
`git status` stays clean. With `--repo <url>` LFS files are pointer files
unless `--lfs fetch`: the worker fetches the commit's LFS objects from the
remote (`git lfs fetch`, with the forwarded credential if any; git-lfs on the
worker) and checks them out.
The worker accepts plain files named by their sha256 only (no sparse or
other special tar entries), verifies every object's hash and bounds each
upload: by default 4 GiB per object, 16 GiB and 50000 objects in total -
in longarmd.toml:

```toml
[workspace]
lfs_max_object_bytes = 4294967296
lfs_max_total_bytes = 17179869184
lfs_max_objects = 50000
# git_trace = true   # debugging: keep GIT_TRACE* / GIT_CURL_VERBOSE from longarmd's
#                    # environment for its git runs (default: removed; credential
#                    # header lines are dropped from job logs either way)
```

(per-user isolation's helpers use the defaults).

**Clone URLs** are fetched by the worker as its own account, without any
prompt (`GIT_TERMINAL_PROMPT=0`, `GCM_INTERACTIVE=never`): a remote that
needs an interactive sign-in - e.g. Azure DevOps through Git Credential
Manager - fails fast with "the worker could not authenticate (... 401/403
...)". Credentials in a clone URL (`https://user:pass@`, `https://TOKEN@`)
are masked in the job's log and failure reason; prefer a credential on the
worker or `GH_TOKEN` over a URL with credentials, which is stored with the
job's spec. Ship the commits from your machine instead (`--repo .`, a local
checkout), forward your own credential for the fetch (`--git-auth forward`,
below), or give the worker account a credential that works without a
prompt (a PAT in its credential store, an ssh key; GitHub: the job's
`GH_TOKEN` secret).

#### Fetching with your credential (`--git-auth forward`)

```sh
longarm run --id <ulid> --repo https://dev.azure.com/org/p/_git/r --ref <full sha> \
  --git-auth forward --require pool=devbox --no-queue --stdin-file request.json -- <cmd>
longarm run w1 --repo . --git-auth forward -- make     # local checkout: remote first, then the delta
longarm sync . w1:C:/work/proj --git-auth forward      # the same for sync
```

The worker fetches the repository straight from its host (Azure DevOps,
GitHub, any https git server) with a short-lived credential **of yours**
that longarm gets on your machine and sends with this one job:

| host | credential | how longarm gets it |
|---|---|---|
| Azure DevOps (`dev.azure.com`, `*.visualstudio.com`) | Entra access token for Azure DevOps (resource / audience `499b84ac-1321-427f-aa17-267ca6975798`), sent as `Authorization: Bearer`; valid about an hour | the organization's tenant from the `X-VSS-ResourceTenant` header of an unauthenticated request to the organization URL (`https://dev.azure.com/<org>`, `https://<org>.visualstudio.com`), then `az account get-access-token --resource 499b84ac-1321-427f-aa17-267ca6975798 --tenant <that tenant>` - not az's default tenant, which may be another one. Without az, or when az has no token for that tenant (longarm notes `az login --tenant <t>`): `git credential fill` (Git Credential Manager gives an Azure DevOps token) |
| GitHub (`github.com`) | `gh auth token`, sent as basic auth (`x-access-token`). **Long-lived**: gh's OAuth token (or a PAT) stays valid after the job, unlike an Azure DevOps token | `gh auth token`, else `git credential fill` |
| any other https host | what your credential helper has, sent as basic auth | `git credential fill` |

Terminal prompts are off for `git credential fill`; a helper's own sign-in
window (Git Credential Manager) may appear. longarm notes on stderr which
source it used (`forwarding a credential for <host> from az (tenant ...)`),
never the credential.

- **`--repo <https url>`**: the credential goes in the worker-direct submit,
  like `--stdin-file`: inside the mTLS connection to the worker; the hub
  never sees it; a `run` that waits in the hub's queue holds it in memory
  until placement (an Azure DevOps token lives about an hour: a job queued
  longer fails its fetch). longarmd keeps it in memory, uses it for the
  workspace's fetch only - git and, with `--lfs fetch`, git-lfs - and zeroes
  it afterwards. The job never gets it. `--ref` may be a full commit id
  that is no branch tip: it is fetched by id (protocol v2, which GitHub,
  Azure DevOps and git's http-backend serve for any commit reachable from
  their refs; when a server refuses, the worker fetches the remote's
  branches and looks for the commit there). A commit the worker's mirror
  has already is not fetched again; on a shared-isolation worker the job
  still proves its own access first (one `git ls-remote`, no objects:
  [security](security.md#forwarded-git-credentials)). No credential to be had on this
  machine: exit 77, nothing submitted. Refused by the remote (expired, no
  access, wrong tenant): the job fails (exit code 126) with `the forwarded
  credential was refused (...)`.
- **`--repo <local checkout>`** (and `sync`): remote first. The worker
  fetches, with the credential, the commits of the remote that your commit
  builds on - the boundary of what is not on the remote's tracking
  branches (`git rev-list --boundary <commit> --not --remotes=<remote>`;
  the commit itself when it is pushed) - from the remote of the branch's
  upstream (else `origin`); ssh remotes of GitHub and Azure DevOps are
  rewritten to their https URL. The bundle that follows carries only what
  the worker still lacks: the unpushed delta (`longarm: the worker fetched
  1 of 1 base commit(s) from <host> ...; shipping the delta`). When the
  checkout has no such remote, no credential can be had, the worker is too
  old, or its fetch fails (remote unreachable from the worker, credential
  refused), longarm notes it and ships everything the worker lacks, as
  without `--git-auth`. The credential is sent in `Workspace.Fetch`,
  worker-direct, and zeroed after that fetch.
- Remote-first is **not** the default: a credential leaves your machine
  only when you ask for it with `--git-auth forward`.

How the worker uses it: git and git-lfs ask for it through a credential
helper - longarmd itself, which hands it out over a loopback connection
with a one-time key, for this fetch only and only for the exact protocol,
host and repository path of the fetched URL (`credential.useHttpPath`). It
is never on a command line, in git's or git-lfs's environment (git-lfs
writes that into `lfs/logs` on a fatal error), in a config file (the
mirror's or the worktree's) or in a remote URL. Sent as basic auth (an
Azure DevOps token as the password, as Git Credential Manager does).
Redirects are refused (`http.followRedirects=false`: "the remote
redirected ... use the final URL"), the LFS endpoint is the fetched URL's
(`lfs.url`; a repository's `.lfsconfig` can't send it elsewhere),
submodules are not fetched, git's traces are off and `lfs/logs` is
removed after an LFS fetch. The worker's own credential helpers are off
for that fetch, so a refused credential neither falls back to the
worker's own nor gets stored. It is masked in the job's log, the failure
reason and errors (Authorization header values are masked in any git
output). Job records (`job.json`,
`jobs --json`: `workspace.gitAuth: true`) and the worker's audit log
(`git_auth: true`) only say that there was one. The worker must announce
`git-auth-forward` (`Node.Info` features); against an older one `run --repo
<url> --git-auth forward` exits 2 before submitting, a local checkout is
shipped in full (with a note). Security:
[docs/security.md](security.md#forwarded-git-credentials).

### Port links: worker to worker

For joint debugging across workers - frontend and tests on worker A, backend
on worker B - a port link makes a port of B reachable on A. Traffic goes A
to B directly (or through the hub's relay), never through your machine:

```sh
longarm link create wb:8080 --on wa --port 18080      # wa's 127.0.0.1:18080 -> wb's 127.0.0.1:8080
#   [--ttl 8h] (default: the hub's [links] default_ttl, at most max_ttl)
#   [--job <id>]  the link goes when this job on wa ends
longarm link ls [--all] [--json]                      # state (listening | error | offline), path, connections, bytes
longarm link rm lk-0123456789ab                       # cuts open connections at once
```

- **Who can use it.** You own the links you create; other users neither see
  nor remove them (NotFound) nor use them. On a worker with per-user
  isolation, only processes of your account (`la-<you>`) may connect to the
  listener on A (longarmd checks the connecting socket's owner: Linux via
  `/proc/net/tcp`, Windows via the owning process); on B the port must be
  listened on by a process of your account, like `longarm forward`. On a
  worker in shared isolation every local process can connect to the
  listener (it is a loopback port like any other). So when users are
  separated, only admins may link from a shared-isolation worker to a
  per-user one (it would expose the owner's private port there).
- **Workers decide too.** Both ends must be workers you may use
  (user-owned workers: yours or shared with you - admins included), and
  each worker applies its own policy to you on every connection: its
  `[access] users`, its owner and shares, disabled users and, with
  end-to-end trust, your pinned user key **and an endorsement of the other
  worker by your key** - link workers you endorsed yourself. A link a
  worker refuses shows `error` ("refused by this worker's policy") on A, or
  its connections fail with `PermissionDenied` from B
  ([security](security.md#port-links)).
- **Signed links (end-to-end trust).** On a device endorsed by your user
  key (`longarm trust init` / `endorse`), `longarm link create` signs the
  link with the device's key; workers that pin your key accept only links
  you signed this way ("no owner's consent" otherwise). A signed link's
  `--ttl` defaults to 8h and must be within the hub's `max_ttl`.
- **Discovery.** Your jobs, sessions and `longarm exec` on A get
  `LONGARM_LINKS=wb:8080=127.0.0.1:18080,...` and
  `LONGARM_LINK_WB_8080=127.0.0.1:18080` (worker name upper-cased, other
  characters `_`) - variables you set yourself win. Jobs keep what they got
  at start.
- **Lifetime.** A link ends after its TTL, when its `--job` ends, with
  `longarm link rm`, when its owner is removed or a worker is revoked;
  both workers then close the listener and every open connection. The hub
  keeps links across restarts; workers keep their last list, so links work
  while the hub is down: A ends a link at its TTL and when its `--job` ends
  by itself, B at its TTL (a link removed meanwhile only ends when the hub
  is back or its TTL runs out).
- **Paths.** A dials B like a client does: the transport race over B's
  advertised endpoints (tcp, tailscale, longnet), with A's own longnet and
  tailscale nodes. Tailnets need a rule for worker -> worker tcp:7701
  ([deploy/hub/README.md](../deploy/hub/README.md)); the hub's relay carries
  worker <-> worker traffic only for pairs with an active link (a
  standalone `longarm-relay` never does).
- **Limits.** `[links]` in hub.toml: `max_per_user` (20), `default_ttl`
  (8h), `max_ttl` (168h), `disabled`. Listen ports are 1024-65535 and
  unique per worker. TCP only; the listener binds 127.0.0.1 and ::1 (so that `localhost` can't
  reach another process holding the port on one of them); while the port is
  taken by another process the link shows `error` and is left out of the
  job variables. `max_ttl` is at most 720h.
  In longarmd.toml, `[links] max_conns` (64, -1 = no cap) caps each
  link's open connections on both workers; more are refused.
- **Panel.** "Port links" lists your links (admins: everybody's) with a
  Remove button. MCP: `fleet_link` (create / list / remove).

How it is authorized: the hub signs a grant per link (link id, owner, A, B,
ports, expiry) with the key of its hub certificate, which the fleet CA issued,
and pushes every worker the full list of links naming it. A presents the
grant to B in `LinkTunnel.Dial` - the one RPC a worker certificate may call
on another worker; B accepts only a grant for its own name from the worker
it names, unexpired, for a link still in B's list, and checks the target port
for the owner like `Forward.Open`. Creation, removal (with the reason) and
every connection B accepts or refuses are audited (`longarm admin audit
--node hub|wb`). See [security.md](security.md#port-links).

### SSH / scp / VS Code Remote-SSH

```sh
longarm ssh-config >> ~/.ssh/config                # Host longarm-<worker> entries + known_hosts from mTLS
ssh longarm-w1                                     # interactive shell
ssh longarm-w1 git -C C:/work/proj status          # exec (cmd.exe /c on Windows workers, like OpenSSH)
scp big.zip longarm-w1:C:/tmp/ ; sftp longarm-w1
```

longarmd embeds an sshd that, by default, is reachable only through
`longarm proxy <worker>` (the generated `ProxyCommand`), i.e. through the
mTLS-authenticated gRPC channel; such connections need no ssh keys. It
supports exec/shell with or without a pty, the sftp subsystem (scp uses it),
and `-L`/`-D` forwarding (VS Code Remote-SSH). Forwards (`longarm forward`, ssh
`-L`/`-D`, port links) reach the worker's own loopback ports only, unless
`[forward] targets = "any"` (a breaking change for forwards to other hosts:
see [CHANGELOG.md](../CHANGELOG.md#upgrading)). Optionally longarmd also listens
directly, accepting the public keys in `<data>/authorized_keys`:

```toml
# <data>/longarmd.toml
listen = ["tcp://100.64.0.11:7701"]
max_log_bytes = 1073741824      # out.log cap per job/session; rotates to out.log.1 (0 = off)

[ssh]
# disabled = true
listen = ["tcp://100.64.0.11:2222"]   # optional direct sshd (authorized_keys)
# shell = ["pwsh", "-NoProfile", "-Command"]   # exec shell (default cmd.exe /c | $SHELL -c)

[forward]
# targets = "loopback"          # what forwards (longarm forward, ssh -L/-D, port links) may reach:
#                               # loopback (default) = this worker's 127.0.0.1/::1 ports only;
#                               # "any" = any host:port the worker reaches (LAN, other machines)

[links]
# max_conns = 64                # open connections per port link through this worker (-1 = no cap)

[gc]
# disabled = true
keep = "168h"                   # remove terminal jobs/sessions (+ worktrees) older than this
min_free_percent = 10           # then oldest first while the disk is fuller than this
interval = "1h"

[audit]                         # copies of audit.log for the machine's owner (docs/security.md#audit-trail)
# file = "/mnt/usb/longarm-audit.log"       # extra append-only copy (the same chained lines)
# syslog = "udp://logs.example.com:514"     # or tcp://HOST:PORT (RFC 5424, octet counting); "local" (not Windows)
# webhook = "https://audit.example.com/ingest"  # POST application/x-ndjson batches (http only to localhost)
# webhook_token_file = "/etc/longarmd/audit-token"  # optional: Authorization: Bearer <file content>
```

### Operations

```sh
longarm admin gc w1 [--keep 72h] [--dry-run]       # admin role; same as the hourly automatic run
longarmd gc --data-dir D [--keep 0s] [--dry-run]   # offline (refuses while longarmd runs)
longarmd audit verify [--file PATH]                # check audit.log's hash chain (or a sink's copy); exit 1 if broken
```

longarmd reloads `<data>/tls` (ca.crt, node.crt, node.key) within ~5 s when the
files change: re-issue the worker certificate in place, no restart needed.

Telemetry ([docs/telemetry.md](telemetry.md)): only when the hub has
`[telemetry] connection_string`; scrubbed on the machine before it is queued.

```sh
longarm telemetry status | show | flush   # on or off and why, pseudonyms, queue; queued items as JSON; send now
longarm telemetry off | on                # this machine (also LONGARM_TELEMETRY=0)
# worker: [telemetry] enabled = false in <data>/longarmd.toml
```


### Doctor: `longarm doctor`, `longarmd doctor`

A checklist of this machine's setup; every line that isn't ok ends with the
exact command to run next:

```text
$ longarm doctor
longarm doctor (v0.1.3)
  ok    signed in      as alice (device alice-pc, client certificate)
  ok    certificate    valid until 2026-11-27 09:30 (29d left)
  ok    hub            longarm.example.com:7443 reachable, version v0.1.3
  warn  client daemon  not running (background port forwards, faster connections); next: longarm daemon start
  ok    firewall       inbound allow rule for C:\Program Files\Longarm\longarm.exe
  ok    worker devbox  online, via longnet/direct 3.1ms [longnet: won, tcp: refused]
  FAIL  worker pc2     is offline (the hub last saw it 2h ago); a user-session worker runs only while its owner (alice) is logged on; next: on pc2: log on, or run longarmd session-mode start (a service: longarmd service start)
1 problem: run the command after "next:" on the FAIL line
```

`longarm doctor` checks: signed in (and as whom), the certificate's
validity, the hub (reachable, its version), the client daemon, the Windows
Firewall rule for hole punching, and for each of your own workers whether
the hub sees it online (or when it last did) and which transport legs
connect from here (the probe of `longarm diag`).

`longarmd doctor [--data-dir D]` checks the worker on this machine: enrolled
and the certificate's validity, longarmd running (local control API) and
its hub link, how it starts (the user-session scheduled task or the
service, and whether that is registered), its log files, and ERROR lines of
the last 24 hours in longarmd.log.

Both exit 0 without failures (warnings allowed), 1 with failures. When
longarmd is not running, `longarmd status` (and the other local commands)
say how to start it in its run mode and where its logs are.

### `doctor --json`

`longarm doctor --json` and `longarmd doctor --json` print one object (for
the tray and other programs):

```json
{
  "schema": 1,
  "tool": "longarm",
  "version": "v0.1.3",
  "time": "2026-10-28T12:00:00Z",
  "ok": false,
  "checks": [
    {"id": "signed-in", "title": "signed in", "status": "ok", "detail": "as alice (device alice-pc, client certificate)",
     "data": {"user": "alice", "device": "alice-pc", "role": "client"}},
    {"id": "worker:pc2", "title": "worker pc2", "status": "fail", "detail": "is offline (the hub last saw it 2h ago)",
     "fix": "on pc2: log on, or run longarmd session-mode start (a service: longarmd service start)",
     "data": {"online": false, "last_seen": "2026-10-28T10:00:00Z"}}
  ]
}
```

- `status`: `ok`, `warn` (works, but something should be done), `fail`
  (broken), `skip` (not applicable or not checkable here; `detail` says
  why), `info` (facts such as log paths). `ok` is false exactly when some
  check is `fail`.
- `fix`: the next command for `warn` and `fail` (left out when there is
  nothing to run). It is meant for people: show it, don't run it - some
  name another machine ("on pc2: ..."), some need elevation (noted in the
  text).
- `id` is stable: `signed-in`, `certificate`, `hub`, `daemon`, `firewall`,
  `workers` (you have none of your own), `worker:<name>` (longarm);
  `signed-in`, `certificate`, `running`, `hub`, `run-mode`, `logs`,
  `errors` (longarmd). `data` holds optional machine-readable facts
  (`not_after`, `last_seen`, `legs`, `address`, `version`, `mode`,
  `files`, `lines`, ...).
- Checks and fields may be added within `schema` 1; a change of meaning
  bumps it.

### Shell completion

`longarm completion bash|zsh|fish|powershell` prints a completion script
(`longarm completion <shell> --help` says where to put it), for example:

```sh
longarm completion bash > ~/.local/share/bash-completion/completions/longarm
longarm completion powershell | Out-String | Invoke-Expression   # in $PROFILE
```

Worker names complete from workers.toml and the hub's cached list (no
network); job ids (and `last`) from the hub's recent jobs, and session names
(`-s`) from the worker - each within 3 seconds.

### Signing out

`longarm leave` signs this machine out: the hub revokes its certificate and
longarm deletes it here (an expired one too). To use Longarm again: `longarm
login`. `longarm logout` only points at `leave`, because signing out
revokes this machine's sign-in.

### Bug reports / diagnostics

One command collects what a maintainer needs - logs, versions,
configuration, state - with personal data and secrets removed, shows what it
collected, writes a zip and (optionally) uploads it through the hub:

```sh
longarmd diag [--upload] [--yes] [--since 24h] [--note "..."] [--out F.zip]   # on a worker (as longarmd's account / admin)
         [--include-audit] [--include-job ID ...] [--log-file PATH ...] [--data-dir D]
longarm diag  [--upload] [--yes] [--since 24h] [--note "..."] [--out F.zip] [--no-probe]   # on a client device
longarm exec w1 -- longarmd diag --upload --yes   # a worker's bundle, from a client (longarm diag --worker w1 says so)
longarm admin diag ls [--since 168h]               # admins: uploaded bundles
longarm admin diag get LAR-20261027-7F3K9Q [--out F.zip]
```

The zip is always written locally (default `longarmd-diag-<time>.zip` /
`longarm-diag-<time>.zip` in the current directory) and the command first
prints every file with its size and why it is there; look inside before
you send it anywhere. `--upload` asks before uploading (`--yes`: don't),
then prints a **report id** like `LAR-20261027-7F3K9Q` on stdout (and the
blob path on stderr): put it into the bug report. A hub without
`[diagnostics]` refuses uploads; attach the zip by hand then.

**Included** (each a file; `manifest.json` lists them with size, reason and
a count of replacements per kind):

| | worker (`longarmd diag`) | client (`longarm diag`) |
|---|---|---|
| versions, OS / kernel, uptime, memory, free disk | `system.json` | `system.json` |
| process logs of `--since` (rotated and `.gz` copies too, newest first, 20 MiB in all) | `<data>/longarmd.log`, `update-watch.log`, `--log-file` | `<config>/daemon.log`, `tray.log` |
| configuration, values of secret-holding keys (password, token, secret, key, webhook ...) removed after parsing the TOML | `longarmd.toml`, `hub.toml` | `workers.toml`, `hub.toml` |
| certificate metadata: identity (pseudonyms), role, serial, expiry, issuer fingerprint, trust extension status | `certs.json` (`tls/`) | `certs.json` (`tls/`), `ca-certs.json` |
| policies | `trust.json`, `policy.json` (access, isolation, hub settings, tenancy summary) | `trust.json` |
| state | `status.json` (the running longarmd), `jobs.json` (ids, states, times, exit codes), `update.json`, drain / limits / endpoints, revocation list size | transport settings and cache, hub state |
| connectivity | | `connectivity.json`: hub reachable?, the transport leg each known worker answered on |
| crash reports | `<data>/telemetry/crash-*.txt` | `<config>/telemetry/crash-*.txt` |

**Never included**: private keys (`node.key`, `user.key`, `ca.key`, the SSH
host key), token and tailnet state files, environment values; binary files.
**Only on request**: the audit log (`--include-audit`; it contains command
lines) and a job's command line, environment names and the last 256 KiB of
its output (`--include-job ID`, after a warning: output can hold secrets
the scrubber doesn't recognise).

Every text file goes through the same scrubber as telemetry
(`internal/pii`): names of people and machines known locally (node, users,
workers, host, OS user, the hub's host) become pseudonyms like
`<node:3f2a9c>` - the same in every bundle of the fleet (salted with the
fleet CA), so a maintainer can follow one machine across reports without
learning its name - and secrets, e-mail and IP addresses (except loopback)
become tags. Scrubbing free text is best effort: check the zip.

**Upload**: any enrolled node (worker, client, admin) may upload; the hub
checks the size (`max_mb`, default 50) and a rate per node
(`per_node_per_hour`, default 10), streams the zip to
`<container>/<yyyy>/<mm>/<dd>/<report-id>.zip` with metadata (component,
version, node pseudonym, uploader role, the scrubbed `--note`) and logs one
line per upload (report id, size, uploader pseudonym). hub.toml:

```toml
[diagnostics]
blob_url = "https://<account>.blob.core.windows.net/diagnostics"   # env LONGARM_HUB_DIAG_BLOB_URL; or file:///var/lib/longarm-hub/diagnostics
max_mb = 50
per_node_per_hour = 10
```

Azure Blob Storage is used through its REST API with a Microsoft Entra ID
token of the hub's managed identity (`AZURE_CLIENT_ID`; role **Storage Blob
Data Contributor** on the container) - no account keys, no SAS; users need
no Azure credentials. deploy/azure (`storageAuth=identity`) creates the
container `diagnostics`, the role assignment and a lifecycle rule deleting
bundles after 30 days. A `file://` directory serves self-hosted hubs and
tests. Who can read bundles: docs/security.md "Diagnostic bundles".

## Fleet with longarm-hub

`longarm-hub` is the control plane: CA and enrollment with one-time join codes,
node registry and heartbeats, a job index rebuilt from worker reports, fleet
events, the scheduler (`longarm run` without a worker), audit storage, toolchain
profiles, the longnet relay, and an HTTP API/panel with install scripts. It is
never on the data plane: when it is down, workers keep running jobs and `longarm`
talks to workers directly (cached addresses).

Deployment (Ubuntu VM, docker compose or systemd): see
[deploy/hub/README.md](../deploy/hub/README.md). Local try-out:

```sh
longarm-hub --data-dir ./hub ca import ~/.config/longarm/ca   # keep certificates issued so far
longarm-hub --data-dir ./hub serve                             # :7443 gRPC+relay, 127.0.0.1:7480 HTTP
longarm-hub --data-dir ./hub ca show                           # CA fingerprint for --ca-fingerprint
longarm-hub --data-dir ./hub join-code --role admin            # bootstrap the first admin (or longarm-hub issue)
longarm join hub.example.com:7443 K7P-Q2M --ca-fingerprint <fp>  # this longarm: client/admin cert + hub.toml
longarm login hub.example.com                                  # or: sign in with Microsoft Entra ID (see below;
                                                               # a hub with an HTTPS panel needs no port, no fingerprint)
```

Adding machines:

```sh
longarm admin join-code [--role worker|client] [--ttl 10m] [--labels region=eu,os=windows] [--profile dev]
# labels: KEY=VALUE, comma-separated and/or repeated; also longarm admin label w3 region=eu,pool=big (KEY= deletes)
# worker (installs nothing else; the service is separate):
longarmd join hub.example.com:7443 K7P-Q2M --ca-fingerprint <fp> [--name w3] [--no-wait]
#   run it as the account the service runs as (root: /var/lib/longarmd; see "Data directory")
longarm admin nodes                      # w3 is "pending"
longarm admin approve w3                 # (hub.toml auto_approve = true skips this)
# or the one-liners (download from the hub's release store, verify sha256, join, install the service):
#   $env:LONGARM_JOIN='K7P-Q2M'; irm https://hub.example.com/install.ps1 | iex
#   curl -fsSL https://hub.example.com/install.sh | sh -s -- --join K7P-Q2M
#   ... --login instead of a code: a worker of your own via your Microsoft account ("Sign in with
#   Microsoft Entra ID" below); with --login the role defaults to both: also your client device, one
#   approval for both (--role worker: the worker only; a join code defaults to worker)
longarm-hub release add ./dist --version 0.2.0   # files longarm-<os>-<arch>[.exe], longarmd-..., packages; see "Self-update" below
```

Certificates renew automatically before expiry (workers in their hub session,
`longarm` when it talks to the hub). `longarm admin revoke <node>` revokes a node; every
worker gets the revocation list with its next heartbeat and refuses that
certificate. Workers that held a hand-issued certificate register themselves
when they first connect (add `[hub] address = "hub:7443"` to longarmd.toml).

With a hub, workers are discovered (no workers.toml needed) and:

```sh
longarm ls                               # from the hub (STATE online/offline/draining/pending); --direct probes workers
longarm jobs [w1] [--state running]      # fleet-wide job index (--json: "source": "hub"; --hub-only: no fallback)
longarm run -- pnpm test                 # placed by the scheduler (= --on auto)
longarm run --type build --mem-gb 8 --require os=linux --repo . -- make   # reservation, labels, repo cache affinity
longarm run --no-queue -- ...            # fail (exit 75) instead of queueing when no worker passes the gate
longarm exec -- hostname                 # short command on the least loaded worker
longarm wait <id>; longarm logs <id> -f; longarm kill <id>   # worker optional: found through the hub
longarm admin nodes | approve | revoke | drain | undrain | label <node> k=v | join-codes | audit
longarm admin tailnet status [--json] | rekey <node>   # tailnet provider + per-node state ("Tailnet provider" below)
longarm admin profile set dev git=latest node=22 pnpm=latest claude=latest pwsh=7
longarm admin profile assign w1 dev; longarm admin profile ls   # canary first, then the rest
```

Scheduling (DESIGN §5.2): gate free memory ≥ 20 GB and CPU < 70 %, score
`free − reserved − 6×running + 10×has_mirror + 5×warm`, memory reserved per
job type (claude/copilot 2 GB, build 8, test 4; `--mem-gb` overrides);
nobody passes → the job waits in the hub's queue (`longarm` shows the position).
Without a reachable hub, `longarm` picks among the known workers itself.

The hub is never in the way: `longarm` gives it 1.5 s to connect
(`LONGARM_HUB_TIMEOUT`) while `longarm ls` already asks the known workers (workers.toml
plus the hub cache `hub-cache.json`) in parallel. A hub that didn't answer is
remembered for 30 s in `hub-state.json` (`LONGARM_HUB_RETRY_AFTER`), so the next
commands skip it, and the longnet legs through its built-in relay, at once.
`--no-hub` / `LONGARM_NO_HUB=1` ignore the hub entirely.

Toolchain profiles: longarmd reconciles the assigned profile idempotently (winget /
npm on Windows, apt + official install scripts / npm on Linux, Homebrew on
macOS), only while no job runs (it drains meanwhile), and reports versions. A
changed profile goes to one canary worker first; the rest follow when it
reports ok, a failure halts the rollout.

### Users, teams and quotas (sharing a hub)

Several people can share the hub and its workers; [security.md](security.md)
has the threat model, what is isolated and what isn't. Users are separated
as soon as one exists (or a worker runs with per-user isolation, or
`[tenancy] enabled = true` in hub.toml):

```sh
longarm admin user add alice [--team web]... [--max-jobs 8] [--max-mem-gb 64] [--max-jobs-per-worker 2] \
    [--max-mem-gb-per-worker 32] [--max-sessions-per-worker 4] [--job-mem-gb 4] [--max-job-mem-gb 16] \
    [--job-cpus 4] [--max-job-cpus 8] [--device alice-laptop] [--admin] [--ttl 24h] [--panel] [--no-code]
#   prints a client join code bound to the user (the device's certificate names the user);
#   --panel also creates a web panel login (role user: "My jobs", nodes, "Me") and prints
#   its generated password and TOTP secret once
longarm admin user code alice [--device alice-desktop]     # a join code for another device
longarm admin user set alice --team web --team infra       # replace teams; quota flags change the quota
longarm admin user set alice --clear-quota                 # back to the default quota
longarm admin user ls [--json]                             # devices, teams, usage, quotas
longarm admin user disable|enable alice                    # refused on the hub and every worker
longarm admin user remove alice [--purge]                  # revoke all of alice's devices; --purge: workers
                                                           # delete her jobs, data and OS account
longarm admin team add web alice bob; longarm admin team rm web bob; longarm admin team ls
longarm admin join-code --role client --user alice --name alice-2   # the same, by hand
longarm admin issue --role client --name alice-pc --user alice --out DIR   # without a hub
longarm whoami                                             # this device: user, teams, quota, usage
longarm run --share team -- make test                      # team members may read it (team:<name> = one team)
longarm run --cpus 4 --mem-limit 8G -- make                # per-job limits (capped by the user's quota)
```

`hub.toml`:

```toml
[tenancy]
# enabled = true                     # separate users before the first `longarm admin user add`
[tenancy.default_quota]              # 0 = no limit
max_jobs = 16                        # active jobs per user in the fleet (the scheduler queues beyond)
max_mem_gb = 128                     # memory reserved by a user's active jobs in the fleet
max_jobs_per_worker = 4              # enforced by every worker
max_mem_gb_per_worker = 48
max_sessions_per_worker = 4
job_mem_gb = 8                       # default memory limit of a job (Job Object / cgroup)
max_job_mem_gb = 32
job_cpus = 4                         # default CPU limit (cores)
max_job_cpus = 16

[links]                              # port links between workers (longarm link)
# max_per_user = 20                  # active links per user
# default_ttl = "8h"
# max_ttl = "168h"
# disabled = true                    # refuse new links
```

Workers that several people use run with **per-user isolation**: longarmd as
LocalSystem / root, every user's jobs, sessions, shells, `exec`, sftp, file
transfers and git as a local account `la-<user>` (created on first use),
data in `<data>/users/la-<user>` (private), mirrors per user, forwards only
to the caller's own loopback ports, process lists hidden from users.
Windows and Linux; macOS refuses the mode.

```toml
# longarmd.toml
[isolation]
mode = "per-user"                    # shared (default) | per-user
# users_dir = "/var/lib/longarm-users"   # default <data>/users (Unix: parents need o+x)
# account_prefix = "la-"
# cgroup_root = "/sys/fs/cgroup/longarm" # Linux; "off" = process groups only
# loopback = "isolate"                   # Linux: isolate (default) | require | allow
```

Install: `sudo longarmd join <hub> <code>` then `sudo longarmd service install
--isolation per-user` (no `--user`; data in `/var/lib/longarmd`, Windows
`C:\ProgramData\Longarm\longarmd`, see [Data directory](#data-directory)), MSI
`ISOLATION=per-user`, deb `LONGARM_ISOLATION=per-user` (`longarmd-peruser.service`,
data in `/var/lib/longarmd`). On Linux mount `/proc` with `hidepid=invisible`
and install `nftables`.

`loopback` (Linux): with `isolate` longarmd installs the nftables table
`inet longarm` so that one account's processes cannot connect to another
account's loopback ports (`127.0.0.1`, `::1`, the machine's own addresses);
connections within one account, from root (Longarm's forwards) and from
other system users keep working. Without `nft` longarmd logs a warning and
runs on; `require` makes it refuse to start instead; `allow` turns it off.
The table stays while longarmd is stopped (jobs outlive it) and is removed
by `longarmd leave --purge`, uninstalling and `loopback = "allow"`. Details
and limits: [security.md](security.md#loopback-between-users-linux).

The **owner of a worker machine** decides locally what the hub's admins and
users may do on it - the hub cannot change this
([security.md](security.md#worker-side-access-policy)):

```toml
# longarmd.toml
[access]
users  = ["alice", "bob"]   # who may use this worker at all (default: everybody the hub admits);
                            # others only get Node.Info (longarm ls / info), every refusal is audited
owners = ["alice"]          # full rights for their role, whatever `admins` says; users implicitly
admins = "full"             # what OTHER admin certificates may do here: full (default) |
                            # own (like a client of their user: own jobs and sessions only, no
                            # admin gc/shutdown, no process lists) | observe (node info and metrics
                            # without process lists) | none (node info)
hub_job_details = true      # false: job reports, forwarded audit lines and process lists sent to
                            # the hub carry no command lines, env names, directories, paths or
                            # process names (the local audit.log keeps everything)
```

Users are the users of the certificates (`?user=`), or the certificate name
for certificates without one. A restricting `[access]` (users or owners set,
or `admins` other than full) turns on ownership checks for everybody but the
owners, who keep acting on everything as before; with `users` set or
`admins` other than full, `[hub] remote_jobs` defaults to `none`. longarmd
refuses to start with an unknown `admins` value or an invalid user name and
logs the effective policy at start (`access policy ...`).

### Node IDs and targets

Every node has an immutable ID (a UUID) next to its name, which can change
(`longarm worker rename`, `longarm admin rename`) and, once the node is
revoked, be taken by a new node. Listings show the shortest unambiguous ID
prefix (8+ characters) next to names (`longarm ls`, `worker ls`, `admin
nodes`, `jobs` and `sessions`: NODE). Wherever a worker or node is named, the
target may be:

```sh
longarm exec box -- hostname                # a name
longarm exec 3f2a9c1e -- hostname           # a unique ID prefix (4+ hex digits), or the full ID
longarm exec id:3f2a -- hostname            # explicitly an ID prefix
longarm exec name:box -- hostname           # explicitly a name
```

A string of hex digits and dashes (4+ characters) is always an ID: names
never look like that. A prefix that matches several nodes is an error listing
them (ID, name, owner, state); nothing is guessed. See docs/security.md
"Node identity" for the rules, revocation by ID and the migration.

### Your own workers

Any user can register their **own** machines as workers and use them from
their client devices - by name (`longarm run --on`/`longarm exec <worker>`) or by
automatic placement, which picks the least loaded worker the user may use.
A worker a user registers is usable **by that user only**: other users don't
see it (`longarm ls`, the panel, events, metrics history), can't call it, and
the scheduler never places their jobs on it. Workers registered by admins
(join codes without owner) stay the shared fleet, as before.

```sh
# the easy way, on the new machine: install the MSI and choose "This machine runs jobs" (the tray
# signs it in), or:
longarmd login <hub> [--name <name>] [--label k=v]...   # on the new worker, no code to copy: sign in with
                                          # your Microsoft account (a code to approve in any browser; see
                                          # "Sign in with Microsoft Entra ID"); --label as worker add's
# where nobody can sign in with a browser (a headless box): a join code
longarm worker add [<name>] [--ttl 24h] [--label k=v]...   # prints a join code (stdout) and the commands:
#   longarmd join <hub> <code> --ca-fingerprint <fp> --name <name>   # on the new worker, then
#   longarmd session-mode install    (Windows: runs while you are logged on, no password)
#   or longarmd service install ...  (runs with nobody logged on; see "Install a worker")
#   the worker joins approved (no admin needed) and owned by you; at most 10 unused codes per user
longarm worker ls [--json]                # your workers, workers shared with you, the fleet (owner, shares, load)
longarm worker label <worker> pool=devbox [gpu-]...   # set (KEY=VALUE) / delete (KEY- or KEY=) labels
                                          # of your worker (admins: any worker); audited by the hub
longarm worker share <worker> [--user bob]... [--team web]... [--everyone]
longarm worker unshare <worker> [--user bob]... [--team web]... [--everyone]
longarm worker remove <worker>            # revoke it (owner or admin); also: worker rm
longarm worker rename <worker> <new-name> # rename it (owner or admin; it keeps its ID; audited)
longarm ls                                # an OWNER column appears once a worker has an owner
                                          # ("-" = fleet, "+shared" = shared by its owner)
```

**Labels** (scheduler constraints: `longarm run --require pool=devbox`) of
your own worker are yours to set: at registration (`longarm worker add
--label`, `longarmd login --label`, the install scripts' `--login --label
k=v` / `-Label 'k=v,k2=v2'` / `LONGARM_LABELS`, the MSI's `LABELS` with
`LOGIN=1`) and later with `longarm worker label` - the same rules as
`longarm admin label`, which stays the admins' tool for any node. Labels the
worker reports itself (`longarmd.toml` `[hub] labels`) are changed there;
the hub's win on a conflict. An older hub ignores sign-in labels
(`longarmd login` then prints the `longarm worker label` command to run).

**Sharing is off by default**: an admin lets a user share their workers,
and admins share and unshare any user's worker (`longarm worker share` with
an admin certificate):

```sh
longarm admin user set alice --can-share          # --can-share=false takes it back (existing shares stay;
                                                  # an admin unshares them)
longarm admin join-code --role worker --owner alice [--name alice-box]   # a worker code owned by alice
longarm whoami                                    # ... can share own workers: true|false
```

Who is shared with is shown to the owner and admins only. Sharing a worker
shares the machine, not the owner's work: on a shared worker everybody sees
and controls only their own jobs and sessions (an admin certificate shared
with gets no admin rights over the owner's jobs). **Admins are not exempt**:
an admin certificate whose user is neither the owner nor shared with is
refused on an owned worker like anybody else (`Admin.Shutdown`/`Gc`
included); admins still see it on the hub, share, unshare and remove it.
Removing a user (`longarm admin user remove`) also revokes the user's workers;
removing or disabling a user drops the user's unused worker codes. Owners
are users (`longarm admin user add`): codes for anybody else are refused.

The hub enforces this when listing and placing, and pushes owner and shares
to the worker (`<data>/worker-access.json`), which refuses everybody else
on every call (audited as `worker owner/shares`; `Node.Info` stays open); the
hub relays kill, logs and submit from the panel or `longarm admin` to it only
for users who may use it (admins included). The
worker's copy comes from the hub, so against the hub's operators pin it
locally too - `longarmd join` prints the hint: `[access] users = ["alice"]`,
`owners = ["alice"]` (plus whom you share with) and end-to-end trust
([security.md](security.md#user-owned-workers)). The hub's rule only ever
narrows what `[access]` and trust allow on the worker.

### Admin worker (Windows)

Jobs on your own worker run as you, without elevation - nobody is there to
click a UAC prompt - and a worker in your signed-in session is offline
after a reboot until you sign in again. For administrator tasks (install
software, change system settings) a Windows machine can also run an
**admin worker**: a second `longarmd` running as LocalSystem, enrolled
with the same sign-in as your worker (one code, one approval; the `/link`
page says it runs commands as SYSTEM), named `<worker>-admin`:

```bash
longarm run devbox-admin -- winget install --id Git.Git -e --silent
longarm exec devbox-admin -- powershell -c "Get-Service | ? Status -eq Stopped"
```

The hub keeps it yours alone: it can't be shared (`longarm worker share`
is refused), admin certificates of anybody else are refused (also with
`--owner`), the hub relays no commands to it (panel, `longarm admin run`),
and it is **never placed automatically** - `auto`, `--require`, the queue
and `session start auto` skip it; name it explicitly. Its label
`longarm.io/admin=true` is set by the hub and can't be changed. `longarm
ls` marks it `+admin` in the OWNER column, `longarm worker ls` as
`mine (admin)`, their `--json` has `adminWorker` / `admin_worker`, and the panel shows an
`admin` badge. Remove it like any of your workers (`longarm worker remove
<name>`).

Install it with the MSI's checkbox "Allow remote administrator commands on
this computer" (Default and Custom) or `ADMIN_WORKER=1` - off by default.
The MSI adds the service `longarmd-admin` (LocalSystem, no password; data
dir `C:\ProgramData\Longarm\longarmd-admin`); the tray's sign-in enrolls it
with the same code and approval as your worker. The tray's Account tab shows
its state and offers **Disable admin worker** (an administrator prompt:
`longarmd admin-worker disable`) - it leaves the hub, its service and data
go, and upgrades keep it off; `longarmd admin-worker status` shows it. The
MSI's uninstall (or an upgrade with `ADMIN_WORKER=0`) removes it too.
Being a LocalSystem service that runs remote commands, it is a
corporate-managed-device consideration: Defender / Intune may report or
block it (see [security](security.md#admin-worker-windows)).

### Sign in with Microsoft Entra ID

Instead of join codes and panel passwords, people can sign in with their
Microsoft Entra ID (Azure AD) account - clients (`longarm login`) and workers
(`longarmd login`) alike - and the web panel has a "Sign in with Microsoft"
link. The hub verifies Microsoft's token and maps the Entra user
(its object ID) to a Longarm user, which it **creates on first sign-in**
(named after the UPN's local part: `alice@example.com` -> `alice`; taken
names get a digit, `alice2`; the UPN is kept, see `longarm admin user ls`).
Like `longarm admin user add`, the first user turns multi-tenancy on.

**Onboarding in one line** (the install scripts need a release in the hub's
store, see "Self-update"; the hub's address and `--ca-fingerprint` are built
into them):

```sh
# this machine as your worker AND your client device: one code, one approval
curl -fsSL https://hub.example.com/install.sh | sudo sh -s -- --login --role both
$env:LONGARM_LOGIN=1; $env:LONGARM_ROLE='both'; irm https://hub.example.com/install.ps1 | iex   # elevated
# a worker of your own only (Linux / macOS; Windows: elevated PowerShell)
curl -fsSL https://hub.example.com/install.sh | sudo sh -s -- --login
$env:LONGARM_LOGIN=1; irm https://hub.example.com/install.ps1 | iex
# a client device only
curl -fsSL https://hub.example.com/install.sh | sh -s -- --login --role client
$env:LONGARM_LOGIN=1; $env:LONGARM_ROLE='client'; irm https://hub.example.com/install.ps1 | iex
# worker labels with the sign-in (as longarm worker add --label)
curl -fsSL https://hub.example.com/install.sh | sudo sh -s -- --login --label pool=devbox
$env:LONGARM_LOGIN=1; $env:LONGARM_LABELS='pool=devbox,os.flavor=ltsc'; irm https://hub.example.com/install.ps1 | iex
# Windows MSI, no code at install time: the tray it opens (or longarmd login) signs in afterwards
msiexec /i longarm-windows-amd64.msi /qn ROLES=both                  # the built-in hub (MODE=default)
msiexec /i longarm-windows-amd64.msi /qn MODE=custom ROLES=both HUB=hub.example.com LOGIN=1
#   [LABELS=pool=devbox] worker labels for that sign-in
# with longarm / longarmd installed already: the hub's domain is enough
longarmd login hub.example.com --role both     # worker + client, one approval (as the user who uses longarm)
longarmd login hub.example.com [--name w3] [--data-dir D]   # a worker only
longarm login hub.example.com                  # a client only
```

Each prints the hub's `/link` address and a code:

```
hub hub.example.com:7443: CA 3f9a...c1 pinned (from https://hub.example.com/ca.crt, checked with this system's HTTPS roots)
To link this machine as your device and your worker (one approval for both), sign in with your Microsoft account (or the hub's panel login) in a browser:

  Open https://hub.example.com/link and enter code BCDF-GHJK
```

**The hub's domain is enough.** `longarm login` / `longarmd login` take
`hub.example.com`, `https://hub.example.com` or `host:port` (the gRPC port;
default 7443). Without `--ca-fingerprint` they fetch the hub's CA from
`https://<host>/ca.crt` - the panel's Let's Encrypt certificate checked
with the system's HTTPS roots, like a browser opening the panel - and pin
its fingerprint (printed) exactly as `--ca-fingerprint` would; the gRPC
address comes from `https://<host>/.well-known/longarm-hub` (the hub's
`public_addr`). A hub without a public HTTPS panel (a private or LAN hub)
fails with a message asking for `--ca-fingerprint <fp>` (`longarm-hub ca
show`); a given fingerprint always wins and is still checked against the
hub. Trust model: [security.md](security.md#first-contact-domain-only-sign-in).
Without any hub, `longarm login` signs in to the hub its config dir already
names (pinning its CA), `longarmd login` to the one the data dir's
`hub.toml` names (e.g. written by the MSI with `LOGIN=1`), else to the hub
the MSI recorded in `HKLM\Software\Longarm`, else to the hub built into
the release (`longarm version --json` shows it as `default_hub`; see
[Built-in default hub](#built-in-default-hub)), and only then asks for one.

**One approval for both roles.** `longarmd login --role both` (what
`--role both` of the install scripts runs, and what the tray does) sends two
key requests - longarm's for the client device (`--config-dir`, default
longarm's; `--device-name`, default `<user>-<hostname>`) and longarmd's for
the worker (`--name`, default the host name) - with **one** code. The
`/link` page says that approving makes the machine both your client device
(acts as you) and your own worker (runs your jobs), and shows both names and
node keys. One approval enrolls both; if either half fails (a taken worker
name) neither is installed. Against a hub too old for this, login falls
back to two codes, one after the other.

Open the page in **any** browser - the machine itself needs none: a headless
Linux box over SSH is fine, approve on your laptop or phone. Sign in with
Microsoft, type the code, check what the page shows (worker or client, name,
host name, OS, the address the hub saw, the node key, when it started) and
approve. The device is enrolled as **yours**: a client gets a certificate
of your user (an admin certificate for the hub's Entra admins), a worker
joins approved and owned by you - usable by you only until you share it,
never with admin rights (as with `longarm worker add`). The page opens by
itself when the machine has a desktop (not over SSH; `--no-browser` or
`LONGARM_NO_BROWSER=1`: never); it never carries the code - you always type
it. Codes last 10 minutes and work once; Ctrl-C gives up. On a hub without
`[auth.entra]` the same works with a panel login of a Longarm user
(`longarm admin user add <name> --panel`); with `[auth.entra]` only a
Microsoft sign-in may approve (and its `web` must not be `off`).

`longarm login` asks the Azure CLI first: with `az login` done, it uses its
token and asks nothing else. It falls back to the code when az is not
installed or has no token for the hub (or with `--link`); `--az` uses az
only (fails instead). `longarmd login` uses the code by default; `--az`
uses the worker's own az sign-in instead (a user's - app-only tokens may
not enroll workers):

```sh
longarm login hub.example.com:7443 --ca-fingerprint <fp>   # client cert of your user (admin cert for
                                                           # the hub's Entra admins), hub.toml, like join
#   device name <user>-<hostname> (--name to choose); signing in again on the same
#   machine (expired certificate) replaces the device's certificate, no --force needed
#   --az: if az has no token for the hub it prints the exact command, e.g.
#     az login --tenant <tenant> --scope api://<client id>/longarm.connect
longarm login hub.example.com:7443 --link           # the code even with az signed in
longarm login hub.example.com:7443 --scope api://<client id>/.default   # another scope (rarely needed)
longarmd login hub.example.com:7443 --az            # worker: az's token, no browser, no code
```

`longarmd login` writes what `longarmd join` writes (certificate, `hub.toml`,
`worker-access.json` with you as the owner) and prints the same hint for
pinning the owner locally; install the service afterwards as usual (the
install scripts do; a service installed by the MSI with `LOGIN=1` waits for
the sign-in and starts serving by itself).

`longarm login` finds `az` on PATH (Windows: also under `Program Files\Microsoft
SDKs\Azure\CLI2\wbin`), or at `$LONGARM_AZ`. The token goes only to the hub,
over the TLS connection pinned to the hub's CA (`--ca-fingerprint`, or the
fingerprint fetched over HTTPS as above; login never trusts a CA on first
use); it is never printed or stored. The pairing runs over that connection
too.

`hub.toml` (every key also as an environment variable, for containers):

```toml
[auth.entra]
tenant_id = "<tenant id>"          # LONGARM_HUB_ENTRA_TENANT_ID (a GUID)
client_id = "<client id>"          # LONGARM_HUB_ENTRA_CLIENT_ID: the app registration; token audience
scope = "longarm.connect"          # LONGARM_HUB_ENTRA_SCOPE: the delegated scope tokens must carry
admins = ["*@example.com", "boss@example.com", "<object id>"]   # LONGARM_HUB_ENTRA_ADMINS (comma-separated):
                                   # admin certificates and the panel's admin role
allowed = []                       # LONGARM_HUB_ENTRA_ALLOWED: who may sign in (empty: anyone in the
                                   # tenant); same entries: UPN, *@domain or object ID, any case
apps = ["<app id>=ci"]             # LONGARM_HUB_ENTRA_APPS: app-only tokens (CI) of these apps, as user ci
web = "spa"                        # LONGARM_HUB_ENTRA_WEB: the panel's sign-in: confidential | spa | off
                                   # (default: confidential with a client credential below, else spa)
client_assertion = "managed-identity"   # LONGARM_HUB_ENTRA_CLIENT_ASSERTION (confidential): the hub's
                                   # managed identity (AZURE_CLIENT_ID) as a federated credential
client_secret_file = ""            # LONGARM_HUB_ENTRA_CLIENT_SECRET_FILE (confidential, hubs outside Azure)
# authority = "https://login.microsoftonline.com"   # national clouds: their login endpoint
```

**App registration** (Entra admin center -> App registrations, one per hub):

- **Expose an API**: Application ID URI `api://<client id>`; a delegated
  scope `longarm.connect` (admins and users may consent); under
  *Authorized client applications* pre-authorize the Azure CLI
  `04b07795-8ddb-461a-bbee-02f9e1bf7b46` for that scope, so `az login
  --scope` needs no consent prompt.
- **Manifest**: `"requestedAccessTokenVersion": 2` (`api.requestedAccessTokenVersion`
  in the Graph manifest): the hub accepts v2 tokens only (issuer
  `https://login.microsoftonline.com/<tenant>/v2.0`).
- **Authentication**, for the web panel, one of:
  - `web = "spa"` (no credential at all): platform *Single-page application*,
    redirect URI `<public_url>/auth/spa-callback`. The browser redeems the
    code (PKCE) and hands the ID token to the hub, which checks it like any
    other. Use this where the tenant's policy forbids client secrets,
    certificates and federated credentials on app registrations (e.g. the
    Microsoft corporate tenant: only spa works there).
  - `web = "confidential"`: platform *Web*, redirect URI
    `<public_url>/auth/callback`; the hub redeems the code with a client
    credential. In Azure: *Certificates & secrets -> Federated credentials ->
    Other issuer*, issuer `https://login.microsoftonline.com/<tenant>/v2.0`,
    subject identifier = the hub's managed identity's **principal (object)
    ID** (deploy/azure outputs it as `identityPrincipalId`), audience
    `api://AzureADTokenExchange`, and `client_assertion = "managed-identity"`.
    Elsewhere a client secret in `client_secret_file`.
- **CI** (`apps`): an app registration (or managed identity) for the pipeline, e.g. with a GitHub
  Actions federated credential. Any app of the tenant can get an app-only token for
  `api://<client id>/.default`, so the hub's `apps` list is what admits it (by its app
  ID, the token's `azp`). In the workflow `az login --service-principal ...
  --federated-token ...`, then `longarm login <hub> --ca-fingerprint <fp>`:
  with a service principal signed in it asks for `api://<client id>/.default`;
  the hub accepts that app-only token only for apps in `apps`, as the
  configured user (client certificates, never admin, created on first use;
  never in the web panel). End with `longarm leave` so runners don't pile up
  as devices.

The panel login of an Entra user is named like its Longarm user (role user,
or admin for `admins`; the user's own password login, if any, stays usable).
Notes: users are bound to their Entra object ID; a removed user who signs in
again gets a new user (a fresh name if the old one has history: devices, a
panel account), never another person's - to block someone
use `allowed`, or `longarm admin user disable`; a change to `admins` applies
at the next sign-in (panel sessions last `[web] session_ttl`, certificates
their TTL).

### End-to-end trust on workers

A worker can refuse every certificate the hub's CA mints on its own and
accept only devices endorsed by user keys its owner pinned
([docs/security.md](security.md#end-to-end-trust-the-hub-is-not-trusted-with-access)).
The state lives in `<data>/trust/` (never written by the hub); a running
longarmd picks up changes within seconds.

```sh
longarmd trust add-user <longarm-user-key line>   # trust a user's endorsed devices (the first one turns it on)
longarmd trust remove-user alice                   # the last one turns it off
longarmd trust deny-node <node key>                # a lost device, even though endorsed
longarmd trust request                             # this worker's endorsement request -> longarm trust endorse
longarmd trust accept <longarm-endorsement line>   # the certificate is renewed to carry it
longarmd trust status                              # policy, node key, endorsement, certificate
```

With trusted users the hub session's `[hub] remote_jobs` defaults to
`"none"`.

### End-to-end trust (client)

The hub's CA can issue a certificate for any name. With end-to-end trust a
certificate must also carry an endorsement by a **user key** that stays on
the user's devices ([security.md](security.md)): workers can refuse devices
that aren't endorsed by a user they trust, and a client can refuse workers.
The hub only copies the endorsement into the certificate when it renews it.

```sh
longarm trust init                      # first device: user key for this certificate's user, endorses
                                        # this device, renews the certificate through the hub; prints
                                        # the user key line for the workers' owners:
                                        #   longarmd trust add-user <line>     (on each worker)
longarm trust show                      # user key, node key, endorsement, certificate, policy
# a worker of yours: longarmd trust request (on the worker) ->
longarm trust endorse <request>         # prints the endorsement -> longarmd trust accept <endorsement>
# another device of yours:
longarm trust request                   # on the new device -> longarm trust endorse <request> on the first
longarm trust accept <endorsement>      # on the new device; renews the certificate
longarm trust export-key > key.pem      # or copy the (secret) user key instead ...
longarm trust import-key key.pem        # ... endorses this device with it and renews
longarm trust require on|off            # only talk to workers endorsed by a trusted user key
longarm trust add-user <user-key-line>  # also accept workers endorsed by this user (besides your own key)
longarm trust remove-user bob
longarm trust deny-node <node-key>      # refuse a node key (base64, trust show) even when endorsed
```

State lives in `<config dir>/trust` (`node.key`, `endorsement`, `user.key`,
`trust.toml`). `longarm join` puts the node key into the first certificate;
an endorsement reaches the certificate with the next renewal, which `init`,
`accept` and `import-key` do at once (if the hub is unreachable, the next
`longarm` command that reaches it, or `longarm daemon`, renews). With
`require on`, every connection to a worker (all transports, `longarm daemon`,
`longarm mcp`) checks the worker's endorsement in the TLS handshake; a
refused worker fails with `worker <w> refused by end-to-end trust: ...` and
the commands that fix it. `longarm daemon` and `longarm mcp` pick up changes
to `trust.toml` without a restart; a broken `trust.toml` makes longarm refuse
to connect at all.

### Local control API, longarm daemon

`longarmd` and `longarm daemon` serve a local-only control API (named pipe with an
owner-only DACL on Windows, 0600 unix socket elsewhere): gRPC plus a JSON/SSE
gateway (`GET /v1/status`, `POST /v1/drain`, `GET /v1/events`, ...) for the
tray/UI. Every call has a CLI equivalent:

```sh
longarmd status | drain [--reason R] | undrain | toolchain status|apply | events
longarmd limits [--max-jobs N] [--reserve-mem GB]  # local limits, honoured by the hub's scheduler
longarmd jobs | logs <job> [-f] | kill <job>        # this worker's jobs
longarmd leave [--reason R] [--force] [--kill-jobs] [--purge]   # unregister (hub revokes the certs); a running longarmd stops
longarm daemon start | stop | status | events     # client session daemon (also: tailscale/longnet legs of one-shot longarm)
longarm pool [--watch] [--json]                    # live pool view: metrics + this client's latency per leg
longarm transport [race|tcp|tailscale|longnet]       # transport policy (see "Transport legs and racing")
longarm forward w1 3000 --bg                       # forward hosted by longarm daemon; prints the URL
longarm forward --list; longarm forward --stop 3000|all
```

`longarmd.toml` additions:

```toml
[hub]
address = "hub.example.com:7443"     # usually written to hub.toml by longarmd join
labels = { region = "eu" }           # scheduler constraints (longarm run --require region=eu)
# advertise = ["tcp://100.64.0.11:7701"]   # endpoints clients get (default: the listeners)
# remote_jobs = "control"          # what the hub (web panel, longarm admin kill|logs|run) may do here:
#                                  # control = kill, logs, toolchain apply (default); all = also
#                                  # run commands submitted through the hub; none (the default
#                                  # when [access] sets users or limits admins)
[toolchain]
# backend = "system"                 # or "fake" (tests): fake_state = "path.json"
# interval = "30m"
```

### Web UI: hub panel and longarm-tray

One frontend, two shells. **The hub panel** is served on longarm-hub's HTTP
listener at `/` (the old server-rendered page moved to `/status`). Log in with
an account from `longarm-hub user add` + TOTP (panel administrators; logins of
Longarm users, created with `longarm admin user add --panel` or on the
"People" page, see only "My jobs", the nodes read-only and "Me"). Pages: nodes (online state, CPU and
memory sparklines, Claude/Copilot sessions, each client's path and RTT to the
worker, labels, approve pending, drain/resume, revoke, toolchain profile and
"update now"), jobs (filter, live logs, kill, run a command), "add machine"
(join code plus the one-line Windows/Linux install commands), toolchain
profiles with canary status, links (client → worker legs), audit, users.
Each action has a CLI equivalent:

```sh
longarm admin kill <job> | logs <job> [-f] | run <worker|auto> -- cmd ...   # through the worker's hub session
longarm admin toolchain-apply <node> ; longarm admin links
longarm-hub user add|ls|rm
```

**longarm-tray** (one desktop app for both roles; tabs by installed role) is a
tray icon with live per-worker CPU/memory columns and a popup
next to it (left click; hides on focus loss unless pinned; drag it by the
title bar). The popup shows every worker's load plus this client's link to every
worker: active leg and path (tcp, tailscale direct/peer-relay/DERP, longnet
direct/relay-udp/relay-tcp), NAT types, RTT with a 5-minute sparkline. Data
comes from `longarm daemon` (hub events when the hub is reachable, otherwise
streamed from the workers; the popup shows which). Tabs: workers, my jobs
(logs, kill), forwards, this worker (longarmd: drain, limits, toolchain, jobs,
leave; join with a code when not enrolled), settings (transport policy).
Notifications: worker offline/back, memory ≥ 90 %, link relayed > 5 min, my
job finished/failed.

```sh
longarm-tray                         # tray + popup (starts longarm daemon if needed)
longarm-tray --browser               # no webview: open the popup page in the browser
longarm-tray --serve 127.0.0.1:7490  # serve only (development: cd web && LONGARM_TRAY_URL=... pnpm dev)
longarm-tray --serve 127.0.0.1:0     # any free port; the first stdout line is the popup's URL with a one-time code (for apps embedding it)
longarm-tray --headless-smoke --out DIR   # check the local APIs, render icon + popup PNGs, exit
                                     # (popup via headless Edge/Chrome; LONGARM_TRAY_BROWSER=<path> or =none)
```

Browser e2e (Playwright, headless Chromium) against a real hub, workers,
`longarm daemon` and `longarm-tray --serve`; writes `docs/ui-screenshots/`:

```sh
cd web && pnpm install && npx playwright install chromium && cd ..
LONGARM_PLAYWRIGHT=1 go test -run TestUIPlaywright -v ./e2e
```

### MCP server for Claude Code / Copilot CLI

```sh
claude mcp add longarm -- longarm mcp
# Copilot CLI: ~/.copilot/mcp-config.json
#   {"mcpServers": {"longarm": {"type": "local", "command": "longarm", "args": ["mcp"], "tools": ["*"]}}}
```

Tools: `fleet_status`, `fleet_exec` (command on a chosen or `auto` worker;
large output comes back as head + tail with a job handle), `fleet_job_submit`,
`fleet_job_wait`, `fleet_job_logs`, `fleet_job_kill`, `fleet_read`,
`fleet_write`, `fleet_sync` (local checkout → worktree on a worker),
`fleet_forward` (via longarm daemon, returns the local URL), `fleet_link`
(port links between workers: create / list / remove). Secrets are passed by
name (`secret_env`) from the environment of `longarm mcp`, never through the model,
and only names the operator allows: `claude mcp add longarm -e LONGARM_MCP_SECRETS=GH_TOKEN -- longarm mcp`.

## Installers, transport racing, self-update

### Install

| Platform | Package | Unattended |
|---|---|---|
| Windows | `longarm-windows-<arch>.msi` (longarm, longarmd, longarm-tray if built; role dialog) | `msiexec /i longarm.msi /qn ROLES=worker HUB=hub:7443 JOIN_CODE=K7P-Q2M [CA_FINGERPRINT=..] [LISTEN=tcp://100.x.y.z:7701] [NODE_NAME=w3] SERVICE_USER=.\me SERVICE_PASSWORD=..` (or `SERVICE_ACCOUNT=LocalSystem`) |
| Linux | `longarm_<v>_<arch>.deb/.rpm` (longarm, longarmd, `longarmd@.service`) | `sudo env LONGARM_ROLES=worker LONGARM_HUB=hub:7443 LONGARM_JOIN_CODE=K7P-Q2M LONGARM_USER=$USER apt install ./longarm_*.deb` (or `/etc/longarm/join.env`) |
| macOS | `longarm-<v>.pkg` (packaging/macos, signed + notarized on a Mac); Homebrew cask | `defaults write /Library/Preferences/com.longarm ...` then `installer -pkg` |
| hub | `ghcr.io/<owner>/longarm-hub` image, `longarm-hub_<v>_<arch>.deb` | `docker compose up -d` (deploy/hub) |

**Default or Custom.** The MSI's first page offers **Default
(recommended)**: the hub built into the release, a sign-in with Microsoft
after the install, and only the role to pick, each with what it installs:

- **"This is my everyday computer: send jobs to workers"** (`ROLES=client`):
  the client - longarm and the tray. Nothing runs jobs here.
- **"This machine runs jobs (Dev Box, build machine)"** (`ROLES=both`): it
  becomes your worker, and you can also send jobs from it - the client and
  the [user-session worker](#the-user-session-worker-runsession-the-default)
  (runs as you, starts at your logon, no password).
- **"Also allow remote administrator commands"** ([admin
  worker](#admin-worker-windows)): only with "runs jobs" (disabled for the
  client; a silent `ROLES=client ADMIN_WORKER=1` is refused).

On a machine that looks like a Windows 365 Cloud PC or a Microsoft Dev Box -
the computer name starts with `CPC-`, or the Dev Box agent service
(`Microsoft.DevCenter.DevBoxAgent` / `DevBoxAgent`) exists - "runs jobs" is
preselected (interactive installs only; silent installs keep `ROLES=client`
unless given, upgrades keep the recorded roles). Next, Install: the Finish
page lists what was installed (and that the Windows 11 tray icon may be
under the ^ arrow next to the clock; a new terminal finds `longarm` on
PATH). The Longarm tray opens in your session (not elevated: the MSI starts
it through a one-off scheduled task) and **starts the sign-in by itself**:
the code shows large in the popup (and is copied to the clipboard, with a
notification), the browser opens the /link page - one code, one approval for
the device, the worker and the admin worker. The popup stays open until the
sign-in ends and then says exactly what the machine became: "Signed in as
alice@contoso.com: client device alice-box, worker box (user session), admin
worker box-admin (runs as SYSTEM, yours only)" (`longarm login` and
`longarmd login` print the same line).

**Changing roles later.** The tray's Account tab has "This computer: [x]
send jobs (client) [x] run jobs (worker) [ ] admin commands (admin
worker)". Apply asks once for an administrator (UAC) and runs the installed
`longarmd install-helper roles` elevated, which changes the machine and what
the installer recorded (`HKLM\Software\Longarm`: Roles, SessionUserSID,
AdminWorker) together - so a later upgrade, repair or uninstall acts on the
new roles; then one approval in the browser signs in only what was added.
Removing a role takes that node off the hub (a hub that can't be reached:
removed here, and the tray says to revoke it there with `longarm admin
revoke <name>`) and removes its task / service. The user-session worker is
the user's who adds it, one per computer: another user can add it only while
the installing user has neither a worker nor an admin worker here, and only
its own user removes it; the admin worker is for an administrator, with the
worker role. A worker installed as a Windows service (`RUN=service`) is
changed with the MSI. The MSI itself offers no "Change" in Apps: without the
tray, uninstall and install again with the new `ROLES` / `ADMIN_WORKER`. **Custom** asks for a hub, either "Sign in with Microsoft after the
install" (`LOGIN=1`) or a join code, an optional CA fingerprint, the roles
client / worker / both, and under Advanced the worker as a Windows service.
An MSI built without a default hub ([Built-in default hub](#built-in-default-hub))
offers Custom only. Silent properties (all in the comment at the top of
`packaging/windows/longarm.wxs`):

| property | values |
|---|---|
| `MODE` | `default` (the built-in hub, `LOGIN=1`; `JOIN_CODE` refused) or `custom`; default: `custom` when `HUB` or a join code is given or there is no built-in hub, else `default`; remembered |
| `ROLES` | `client`, `both`, `worker` (Custom) |
| `RUN` | `session` (default) or `service`; remembered - an install from before `RUN` that has a service keeps it |
| `HUB`, `LOGIN=1`, `JOIN_CODE`, `CA_FINGERPRINT`, `LABELS`, `NODE_NAME` | as before (`MODE=custom`) |
| `SERVICE_USER`, `SERVICE_PASSWORD`, `SERVICE_ACCOUNT`, `SERVICE_NAME`, `DATADIR`, `LISTEN`, `ISOLATION` | `RUN=service` only (`ISOLATION=per-user` implies it; `DATADIR` below `%ProgramData%\Longarm`) |
| `ADMIN_WORKER=1` | the admin worker (off by default; needs `ROLES=both` or `worker`; remembered, `ADMIN_WORKER=0` on an upgrade removes it) |
| `SESSION_SID` | the user of the user-session worker (default: the installing user, `UserSID`) |
| `LAUNCH_TRAY=0` | don't start the tray at the end |
| `KEEP_DATA=1` | uninstall: keep jobs and mirrors |

Repairing or changing the installed product needs an administrator, and
every value is checked before it reaches an elevated step ([security:
Installer](security.md#installer-windows-msi)).

```powershell
msiexec /i longarm-windows-amd64.msi /qn ROLES=both                                    # Default
msiexec /i longarm-windows-amd64.msi /qn ROLES=both ADMIN_WORKER=1                     # + the admin worker
msiexec /i longarm-windows-amd64.msi /qn MODE=custom ROLES=worker HUB=hub.example.com JOIN_CODE=K7P-Q2M
msiexec /i longarm-windows-amd64.msi /qn MODE=custom ROLES=worker RUN=service HUB=hub.example.com LOGIN=1 SERVICE_USER=.\me SERVICE_PASSWORD=...
```

The MSI adds inbound UDP+TCP firewall rules for longarm, longarmd and
longarm-tray, puts them on PATH, starts the tray at logon, records `HUB`
(the bare domain will do), `CA_FINGERPRINT`, `LOGIN`, `LABELS`, `MODE` and
`RUN` in `HKLM\Software\Longarm` (read by the tray, `longarm login` and
`longarmd login`, so none of them asks), and on uninstall makes the workers
leave the hub (`longarmd leave`) and removes everything (`KEEP_DATA=1` keeps
jobs and mirrors). The hub's `install.ps1` / `install.sh` use the MSI /
deb / rpm when the release has them (elevated / root), else they install the
bare binaries as before (`-NoMsi`, `--no-package`).

#### The user-session worker (`RUN=session`, the default)

longarmd runs **as the signed-in user, started at that user's logon** - no
Windows service and no account password, so it works on machines whose only
sign-in is Entra with Windows Hello or a security key (Microsoft Dev Box).
How:

- a scheduled task `LongarmWorker-<user SID>` per user (`longarmd
  session-mode install` registers it for you; the MSI for the installing
  user), "at logon of <user>", run with the user's interactive token (only
  while the user is logged on: no password stored), not elevated, no time
  limit, not stopped on battery, repeated every 5 minutes with "don't start
  a second instance" - a dead host comes back within minutes;
- the task starts a **host** - `longarm-tray.exe --worker-host`, a windowless
  binary (no console window pops up at logon), or `longarmd session-mode
  run` without the tray - that runs `longarmd run` hidden and restarts it
  when it exits: at once after a self-update's exit, else after 2 s, 4 s, ...
  up to 2 minutes (back to 2 s after 5 minutes of running). One host per
  user (a lock in the data dir); a longarmd already holding the data dir
  (started by hand) is watched, not doubled;
- the data dir is **yours**: `~\.longarm`, the default of `longarmd`,
  `longarmd login` and the tray for you - signing in needs no elevation. The
  host prepares it for a sign-in to the recorded (or built-in) hub; longarmd
  waits for the certificate and starts serving by itself.

Why a scheduled task and not `HKCU\...\Run` or the tray: the task survives
the tray's exit, brings a dead host back by itself, needs no password (unlike
a task that runs "whether the user is logged on or not"), and the MSI -
elevated, as SYSTEM - can register and start it for the installing user.

Limits: jobs and sessions run in your logon session. They **keep running
while the remote desktop connection is disconnected** (a Dev Box keeps the
session), but **stop at logoff or reboot** until you log on again (a
worker that must run with nobody logged on: `RUN=service`; administrator
tasks: the admin worker). Jobs run with your filtered (non-elevated) token.
One user-session worker per machine listens on 127.0.0.1:7701 (a second
user's needs `listen` in its `longarmd.toml`). Commands: `longarmd
session-mode status|start|stop|install|uninstall`; the host's log is
`~\.longarm\longarmd-session.log`, longarmd's `~\.longarm\longarmd.log`.

**Updates.** longarmd's self-update replaces its own binary, which needs a
binary dir the worker's account can write: under the MSI
(`C:\Program Files\Longarm`) a user-session worker can't, and
`longarmd update status` / its log say self-update is unavailable - update
with a newer MSI (or `install.ps1`), which restarts the task with the new
binaries. A user-session worker installed by `install.ps1` without the MSI
(into `%LOCALAPPDATA%\Programs\Longarm`) updates itself: it exits with code
75, the host starts the new binary at once, and the rollback watchdog, if the
new version fails its health check, restores the old one and leaves the
restart to the host (update mode `supervised`).

**From a service to the user-session worker.** An upgrade keeps the run mode
(a service stays a service). To switch: uninstall (`msiexec /x`, `KEEP_DATA=1`
keeps jobs), install again with `RUN=session` (or Default) and sign in again;
the service's data dir (`C:\ProgramData\Longarm\longarmd`) is not migrated
to `~\.longarm`.

#### The worker as a service (`RUN=service`, Custom > Advanced)

ROLES `worker` / `both` with `RUN=service`: the longarmd service (auto-start,
restart on failure; data dir `C:\ProgramData\Longarm\longarmd`, readable by
SYSTEM/Administrators/the service account only; `JOIN_CODE` = the worker's,
`CLIENT_JOIN_CODE` = the client's). It runs with nobody logged on. With
`LOGIN=1` the MSI writes the data dir's `hub.toml` and starts the service,
which waits for its sign-in: `longarmd login --role both` as the service's
account (or elevated) signs it in (the tray signs in only data dirs it can
write).

**The worker service runs as a user account** (DESIGN §3.1: jobs then find
that user's git / npm / claude settings), never silently as SYSTEM. The
interactive MSI asks for the account (default: you) and its password and
grants it "Log on as a service". A silent service install
needs `SERVICE_USER` + `SERVICE_PASSWORD`; `SERVICE_ACCOUNT=LocalSystem`
instead runs it as SYSTEM (warned about in the install log and in longarmd.log).
A wrong password fails the install (the service can't start) and rolls it
back. Upgrading with a newer MSI keeps the account but needs
`SERVICE_PASSWORD` again. `install.ps1` does the same with `-Run service`:
`-ServiceUser` (default you) with `$env:LONGARM_SERVICE_PASSWORD` or a prompt,
or `-ServiceAccount LocalSystem` / `$env:LONGARM_SERVICE_ACCOUNT`. By default
(`-Run session`) it installs the user-session worker, and `-Login` runs the
MSI with `LOGIN=1 RUN=session`, then signs in in the console.

#### Built-in default hub

A release carries a default hub: `version.DefaultHub` (and optionally
`version.DefaultHubCA`, a CA pin), set at build time with `-ldflags -X`
from the repository variables `LONGARM_DEFAULT_HUB` / `LONGARM_DEFAULT_HUB_CA`
(release.yml passes them to GoReleaser and to `build-msi.ps1 -DefaultHub
-DefaultHubCA`). It is the MSI's "Default" choice, and `longarm login`,
`longarmd login` and the tray use it when no hub is given - after the hub a
config / data dir names and the one the installer recorded. Its CA comes
from `https://<hub>/ca.crt` at sign-in (checked with the system's HTTPS
roots), and must match `LONGARM_DEFAULT_HUB_CA` when that is set. Without
the variable (forks, local builds) there is no default: the MSI offers
Custom only and `login` needs a hub. To change it - e.g. when moving to the
company's internal GitHub - set the variables in the new repository and cut
a release: `gh variable set LONGARM_DEFAULT_HUB --body longarm.example.com`
(and `LONGARM_DEFAULT_HUB_CA` with `longarm-hub ca show`'s fingerprint for
the pin). New installs use it; installed machines keep the hub they recorded
or are signed in to.

**Secrets stay out of MSI logs.** `JOIN_CODE`, `CLIENT_JOIN_CODE` and
`SERVICE_PASSWORD` are hidden properties. The custom actions that get the
join codes on their command line (`install-helper setup`, `longarm join`) have
`HideTarget`, so even a verbose log (`msiexec /l*v`) shows
`Target=**********` there. To verify an install log (`install.ps1` writes
`%TEMP%\longarm-msi.log`):

```powershell
Select-String -Path install.log -Encoding unicode -SimpleMatch -Pattern '<JOIN_CODE>','<SERVICE_PASSWORD>'   # must print nothing
```

Deregistering by hand: `longarmd leave` (through a running longarmd, which then stops;
`--kill-jobs` / `--purge` need it stopped; `--force` when the hub is
unreachable), `longarm leave`.

Building a release (packaging/): `pwsh packaging/build.ps1 [-Snapshot] [-Key longarm-release.key]`
(Windows: GoReleaser for all 6 targets, the MSIs, the flat signed directory
`dist/hub-release`), `sh packaging/build.sh` elsewhere; winget manifests:
`packaging/winget/render.ps1`.

### Transport legs and racing

A worker advertises every listener through the hub (tcp, longnet, tailscale
by tailnet IP). `longarm` dials **all legs at once and the first leg that
completes the mTLS handshake wins** (the others are cancelled); the winner is
remembered per worker (`transport-cache.json`) and gets a head start next
time, and every `[dial] reevaluate` (default 10m) all legs race again. A leg
that fails starts the others at once, so losing a network costs nothing.
With `longarm daemon` running (it measures every leg, see `longarm pool`), the sticky
winner loses its head start as soon as its path got worse than when it won
(direct -> relay/DERP/peer relay), it is down, or another leg is clearly
better (a better path kind unless slower, or >20 % and >5 ms faster); the
best measured leg then gets the head start. Without a daemon, a sticky leg
that still wins over a worse path makes the next dial race all legs.

The **transport policy** is the one setting for this: `race` (default) or
the transport to prefer (`tcp`, `tailscale`, `longnet`: the other legs start
after 2 s or when it fails). `longarm transport <policy>` saves it in
`settings.json` in the config dir; a running `longarm daemon` (and so longarm-tray's
Settings tab, which sets the same value) switches at once. `LONGARM_TRANSPORT` or
`--transport` override it for one command. `longarm pool` and the tray show the
leg longarm would use (the sticky winner while it is up and not clearly beaten,
else the best leg).

```sh
longarm ls --direct     # PATH column: winning leg + the race, e.g. "longnet/relay 3ms [tcp: cancelled, longnet 41ms won]"
longarm ls              # via the hub: "hub; last longnet/relay ..." (the cached winner)
longarm info w1         # path, policy, legs
longarm transport longnet                             # save the policy (race | tcp | tailscale | longnet)
longarm --transport tcp ...                         # this command only; env LONGARM_TRANSPORT
```

```toml
# workers.toml
[dial]
reevaluate = "10m"       # sticky winner lifetime ("0s" = always race)
[[worker]]
name = "w1"
address = "100.64.0.11:7701"                 # primary leg (tcp)
endpoints = ["longnet://relay.example.com:7443", "tailscale://100.64.0.7:7701"]   # more legs
```

Tailscale legs use the embedded tsnet node when this client has one
(the hub's tailnet, `$TS_AUTHKEY`, or earlier state), else the
system tailscaled when the address is a tailnet IP, else they are skipped.
`LONGARM_TSNET_LOG=1` shows tsnet's logs; `LONGARM_CONNECT_TIMEOUT` (default 10s) bounds
one connection attempt.

**Keep `longarm daemon` running when you use the tailscale or longnet legs**
(`longarm daemon start`; longarm-tray starts it). A tsnet node takes seconds to come
up, and longnet's hole punching needs a long-lived node. A one-shot `longarm` command
dials these two legs **through the daemon** over the local control channel,
using the daemon's nodes that are already up with their direct paths.
- The daemon only opens the byte stream. The mTLS handshake with the worker
  still runs in the `longarm` process, with the user's own certificate.
- `longarm info` shows such a path as `… via longarm daemon`.
- Measured on a local test tailnet, `longarm --transport tailscale info <w>` takes
  ~90 ms with the daemon, against several seconds without it.
- Without a daemon, `longarm` starts its own node. A cold node gets no head start,
  so the other legs race it at once, and `longarm` exits without waiting for the
  node to shut down.

### Tailnet provider: Tailscale (default) or Headscale

The embedded tsnet node of `longarmd` and `longarm` is the tailscale leg. No
`tailscaled` install is needed: the hub mints a **one-time, pre-authorized,
tagged key** for every node it enrolls (`longarmd join`, `longarm join`) and whenever a
node has no key or state for the hub's control server (`Fleet.TailnetKey`).
Workers are `longarm-<name>` with `tag:longarm-worker` and are never ephemeral, because
the hub advertises their tailnet IP. Clients (`longarm`, `longarm daemon`, `longarm mcp`) are
`longarm-client-<name>` with `tag:longarm-client` and are ephemeral by default. One
switch in the hub's hub.toml picks the control plane:

```toml
[tailnet]
provider = "tailscale"        # "tailscale" | "headscale" | "none"
ephemeral_clients = true      # longarm / longarm daemon / longarm mcp nodes; workers never
key_ttl = "1h"                # lifetime of a minted (one-time, pre-authorized) key
[tailnet.tailscale]           # the official service (free plan is fine)
oauth_secret_file = "/etc/longarm-hub/tailscale-oauth.secret"   # or env LONGARM_HUB_TAILSCALE_OAUTH_SECRET
# oauth_client_id = ""        # default: taken from the secret (tskey-client-<id>-...)
# tailnet = "-"               # the OAuth client's tailnet
# api_url = "https://api.tailscale.com"
# control_url = ""            # Tailscale's default (https://controlplane.tailscale.com)
[tailnet.headscale]           # self-hosted alternative (the old [headscale] section still works)
url = "https://hs.example.com"
api_url = "http://127.0.0.1:8080"
api_key_file = "/etc/longarm-hub/headscale-api.key"
```

- **Tailscale** needs a trust credential / OAuth client (admin console →
  Settings → Trust credentials) with only the **`auth_keys`** scope (write)
  and **only the tag `tag:longarm-hub`**. The policy's `tagOwners` make that
  tag an owner of the node tags:
  ```jsonc
  "tagOwners": {
    "tag:longarm-hub": ["autogroup:admin"],
    "tag:longarm-worker":    ["autogroup:admin", "tag:longarm-hub"],
    "tag:longarm-client":    ["autogroup:admin", "tag:longarm-hub"],
  },
  ```
  Tailscale lets a credential mint only keys with **exactly its own tags, or
  tags owned by them**. A credential tagged `tag:longarm-worker` + `tag:longarm-client`
  therefore can't mint a worker key (`tag:longarm-worker` alone): the API answers
  `400 requested tags are invalid or not permitted`. The hub log and `longarm admin
  tailnet status` then point to this setup ([deploy/hub](../deploy/hub/README.md),
  "Tailnet provider"). The hub gets an access token (client-credentials grant, cached
  until shortly before it expires) and creates keys through
  `POST /api/v2/tailnet/-/keys`. The secret, the tokens and the minted keys are
  never logged; errors show them as `tskey-…-[redacted]`.
- **Headscale**: see [deploy/headscale](../deploy/headscale/README.md) (keys over
  the REST API, embedded DERP + STUN behind Caddy, plus Tailscale's public DERP map by
  default so nodes behind networks that filter UDP to the hub still find a
  STUN server and go direct; `derp.urls: []` for a fully self-hosted setup).
- **Switching providers**: change `provider` and restart the hub. Workers
  follow on their next hub session; clients follow on their next hub contact
  (`longarm ls` and the like). The node records the new control server in hub.toml
  `[tailnet]`, gets a key for it and restarts its tsnet node. Every control
  server has its own state dir (`tsnet-<host>` in the data/config dir), so
  switching back needs no new key while the old state is still valid. The
  advertised endpoints update through the hub. A node whose state no longer
  logs in (an ephemeral client the control server removed, a deleted device)
  wipes that state and gets a fresh key by itself.
- **Status and re-key**: `longarm admin tailnet status [--json]` shows the
  provider, the control URL and every node's tailnet state (running /
  starting / needs-key / error), its actual tailnet name and IPs (Tailscale
  appends `-1`, `-2`… when a name is taken; that name is what longarm shows and the
  IP is what the worker advertises), and the last key minted for it. `longarm
  admin tailnet rekey <node>` makes a node log out, wipe its state and enrol
  again: at once for an online worker, otherwise on its next hub contact.
- **Leaving / stale devices**: `longarmd leave` and `longarm leave` log the tsnet node
  out (its node key is invalidated; ephemeral client nodes also vanish by
  themselves). The hub's OAuth client has no `devices` scope, so it cannot
  delete devices: remove stale worker devices (left, re-keyed, reinstalled) in
  the Tailscale admin console (Machines), or with `headscale nodes delete` on
  Headscale.
- A tailscale listener in `longarmd.toml` (`listen = ["tailscale://…"]` with
  `$TS_AUTHKEY`) replaces the hub-managed one; it keeps the plain `tsnet`
  state dir.

### Self-update

Releases are signed with minisign; longarmd only installs binaries whose
signature verifies against the public key built into it
(`-X .../internal/update.PublicKey=RW...`) and whose trusted comment names the
exact file and version.

**The private key stays on the admin's machine** (by default
`~/.config/longarm/release/longarm-release.key`, next to longarm's config); the hub
only gets signed files and checks them against `[releases] public_key`.

```sh
# on the admin machine
longarm-hub release keygen                              # once: ~/.config/longarm/release/longarm-release.{key,pub}
                                                   # (LONGARM_RELEASE_KEY_PASSWORD encrypts the key)
longarm admin release publish dist/hub-release --version 1.4.0 [--channel stable]
                                                   # signs locally, uploads to the hub (admin certificate)
longarm admin release sign dist/hub-release --version 1.4.0   # only sign (build.ps1 -Key does this too)
# or on the hub, with files already signed elsewhere:
longarm-hub release add dist/hub-release --version 1.4.0 [--channel stable]
longarm admin release push 1.4.0 --canary 1 --wait 30m [--batch 3 --batch-wait 5m] [-f]
longarm admin release ls | status [-f] | abort
longarmd update status                                  # last update on this worker
```

For automation there is a narrower credential than admin:
`longarm admin issue --role release --name ci-2026h2 --out ./ci-tls` (or
`longarm-hub issue --role release …` on the hub) may only run
`longarm admin release publish | ls | push | status | abort` - no other hub call,
nothing on workers. `longarm admin release publish --skip-identical` makes a
re-run a no-op.

The hub keeps the files in `<data_dir>/releases/<version>/`, and with
`[releases] blob_url` (env `LONGARM_HUB_RELEASES_BLOB_URL`; an Azure Blob
Storage container written with the hub's managed identity, or `file:///<dir>`)
in that container as `<version>/<file>` too; the data dir is then a cache that
the hub fills from the container (checked against the sha256 it recorded) when
it lost a file - deploy/azure with `--storage-auth identity`, whose data dir
dies with the container. Such downloads are bounded: two at a time, 10 minutes
each (one that the requester abandons goes on for others waiting for the same
file), a blob whose size isn't the recorded one is refused before it is read,
and after a failed download (missing, corrupt, storage error) requests for
that file fail fast (404 / 503) for a minute, doubling up to 30 minutes,
until it is published again. A file that is in neither place is logged at every
start; install scripts then hand out the newest *complete* stable version (or
none), `release push` refuses that version and `release ls` marks the file
`(missing)`. Publishing the same files again (same names and sha256; no
`--replace` needed, and `--skip-identical` doesn't skip an incomplete version)
stores them again and leaves the version as it was.

The worker downloads through the hub's gRPC port, verifies sha256 and
signature, renames its binaries to `*.old` (running shims keep using them)
and puts the new ones in place, then exits; the service manager (or, in the
foreground, longarmd itself) starts the new version. A watchdog started from the
old binary waits: the new version must keep its hub session for 60 s
(`[update] health_window`); if it fails, or does not start within
`start_grace` (2m), the watchdog restores the old files and restarts it, the
version goes on the worker's skip list and the rollout halts. Jobs keep
running throughout. longarmd.toml: `[update] disabled, channel, interval (1h),
health_window, start_grace`. Package-managed installs (deb/rpm/brew) update
through their package manager.

### Firewalls

Windows Defender Firewall drops unsolicited inbound UDP for programs without
an allow rule, which defeats hole punching (longnet) and Tailscale's direct
paths. The MSI adds program rules for longarm/longarmd/longarm-tray; `longarmd service install`
(elevated) adds one for longarmd; for longarm without the MSI, `longarm firewall allow`
(elevated; `longarm firewall` shows the state, `longarm firewall remove`), which the
hub's install.ps1 runs when elevated. `longarm` and `longarmd` print a hint when
punching gets no answer and no rule exists.

Dismissing the Windows Firewall prompt (e.g. for a new longarm.exe path) adds an
inbound **Block** rule for that program, which wins over any allow rule:
connections then silently stay on relays. longarm detects this (and a missing
allow rule) for the executable that punches - longarm.exe for `longarm daemon`,
longarmd.exe for longarmd - and shows it with the fix command: `longarm daemon status`,
`longarmd status`, a line under `longarm pool` and on stderr of `longarm ls` when legs are
relayed, and a warning in the tray popup. The fix, in an elevated terminal:
`longarm firewall allow` (removes the program's Block rules, adds the allow
rules; `--program <path>` for longarmd.exe). `longarm firewall status` exits 1 when
blocked or missing. macOS: the pkg adds
longarm/longarmd to the application firewall when it is on (`socketfilterfw --add
/usr/local/bin/longarmd --unblockapp ...`). Linux: only if ufw/firewalld is active,
allow the longarmd/longarm UDP ports or the interface (e.g. `ufw allow in on tailscale0`;
for longnet punching, outbound-initiated UDP is allowed by default by both).

## CI/CD

GitHub Actions on GitHub-hosted runners; setup, release and rollback in
detail: [ci-cd.md](ci-cd.md).

- `ci.yml` (pull requests, `main`): privacy check, gitleaks, gofmt, vet,
  cross-compile 6 targets, shellcheck, actionlint; `go test ./...` on Linux,
  Windows and macOS.
- `release.yml` (tag `v*`): build on Windows (GoReleaser + MSIs) →
  **publish** (sign with the release key, upload with the `release`
  certificate, GitHub Release) → **hub** (SSH forced command
  [`deploy/hub/longarm-hub-deploy`](../deploy/hub/longarm-hub-deploy): verify, install,
  health check, automatic rollback) → **rollout** (`--canary 1 --wait 15m`,
  fails when it halts). Without a configured hub only the GitHub Release is
  made. `v1.2.3-rc1` → channel beta, no hub/fleet.
- `deploy.yml` (manual, from `main`): hub deploy/rollback, fleet push (downgrades are admin-only).

```sh
git tag -a v1.4.0 -m "Longarm 1.4.0" && git push origin v1.4.0   # build, publish, hub, canary rollout
ssh deploy@hub status | check 1.4.0 | 1.4.0 | rollback             # what the CI's key can do on the hub
longarm-hub release verify longarm-hub-linux-amd64 --version 1.4.0           # signature check vs [releases] public_key
```

## longnet: the self-made tunnel (relay + hole punching)

`longnet` is a second network backend next to tcp/tailscale (DESIGN §4.3):
end-to-end QUIC with the Longarm certificates, carried either directly over
UDP (after hole punching) or through `longarm-relay`, which only forwards opaque
packets. Connections start on the relay immediately and move to a direct path
in the background; if the direct path dies they fall back to the relay without
breaking streams (`longarm logs -f` keeps going). When UDP to the relay is blocked,
nodes use TLS over TCP to the relay instead.

**longarm-hub contains the relay**: with a hub, workers listen on
`longnet://hub.example.com:7443?name=w1` (UDP 7443; the TLS fallback shares the
hub's TCP 7443) and clients learn that endpoint from the hub, no relay
certificate or workers.toml needed. The standalone `longarm-relay` below remains
for setups without a hub. The longnet listener follows certificate renewals.

```sh
# relay host (public): certificate with the relay role from the same CA
longarm admin issue --role relay --name relay1 --host relay.example.com --out ./relay-tls
longarm-relay --listen :7800 --tls-dir ./relay-tls --metrics 127.0.0.1:7801   # UDP+TCP :7800, STUN on UDP :7801
#   --tcp :443 to put the TLS fallback on another port; --rate 32M per-node limit (0 = off)
#   --stun2 ADDR: second STUN port for NAT type detection (default: UDP port + 1; open it in
#   the firewall too); --stun-advertise "self,stun.example.net:3478" overrides the list
#   ("self" = this relay's STUN ports, ":port" = this relay's IP, host:port = an external
#   server, resolved by the nodes; "off" = none)
#   GET /metrics (Prometheus) and /sessions (JSON: per node transport, reflexive address,
#   NAT type, STUN-mapped addresses, candidates, bytes) on --metrics

# worker: listen on longnet and tcp at the same time
longarmd run --listen tcp://127.0.0.1:7701 --listen "longnet://relay.example.com:7800?name=w1"
#   URL options: tcp=HOST:PORT (TLS fallback address), bind=:7702 (local UDP port),
#   mode=auto|udp|tcp, direct=0 (relay only), relay_name=DNS (check the relay cert name),
#   stun=HOST:PORT,... (extra STUN servers besides the relay's)
```

Client `workers.toml`:

```toml
[longnet]
relay = "relay.example.com:7800"   # default relay for longnet workers
# relay_tcp = "relay.example.com:443"
# mode = "auto"                    # auto | udp | tcp
# disable_direct = false

[[worker]]
name = "w1"
transport = "longnet"                # address defaults to [longnet] relay
```

`longarm ls` shows the live path, e.g. `longnet/direct 80ms (203.0.113.78:27225, nat easy/hard)` or
`longnet/relay 31ms (udp 203.0.113.5:7800, nat hard/hard)`; "nat ours/peer's" is the NAT type
each side detected by STUN (open, easy = same public port for every destination,
hard = symmetric, ? = unknown). Every NAT pair goes direct except hard × hard,
and an easy side whose filter only admits the exact ip:port it sent to (port-restricted)
facing a hard side; those stay on the relay. Debug knobs (environment, for
longarm and longarmd): `LONGARM_NET_DISABLE_DIRECT=1` (always relay), `LONGARM_NET_RELAY_MODE=tcp|udp|auto`
(`LONGARM_NET_FORCE_TCP=1` = tcp), `LONGARM_NET_STUN=host:port,...` (extra STUN servers),
`LONGARM_NET_LOG=1|debug` (longarm prints relay/NAT/punch events on stderr, e.g. why a
punch attempt failed).

**STUN behind source NAT.** Some platforms (Azure Container Instances) rewrite the source
address of every inbound UDP packet: a relay there sees every node at an internal private
address, and so do its STUN answers. Nodes ignore a private STUN answer from a server on a
public address (it is neither a candidate nor counted for the NAT type; logged once per
server); with fewer than two usable answers the NAT type is unknown and nodes punch
normally. A private answer from a server on a private address (a hub on the LAN) is used as
before. The relay notices the situation itself — nodes report public STUN-mapped addresses
(from other servers) but connect from private ones, and no node connects from a public
address — logs it, shows `"stun_snat": true` in `/sessions`, stops advertising its own STUN
ports and stops passing its observed addresses to peers as candidates. Direct connections
then need external STUN servers: advertise them from the relay (`--stun-advertise`, hub
`relay_stun_advertise` / `LONGARM_HUB_RELAY_STUN_ADVERTISE`) or set them on the nodes
(`LONGARM_NET_STUN`, URL option `stun=`).
