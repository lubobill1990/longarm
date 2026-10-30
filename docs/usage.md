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

## Quick start (join a hub, sign in with Microsoft)

Install, sign in, run something. The hub is `hub.example.com` (or the one
built into the release: [Built-in default hub](#built-in-default-hub));
running one yourself: [Fleet with longarm-hub](#fleet-with-longarm-hub);
no hub at all: [Advanced: self-hosted without sign-in](#advanced-self-hosted-without-sign-in-ca-and-admin-issue).

```sh
# 1. install - Windows: longarm-windows-amd64.msi from the hub's "Get Longarm" page
#    (https://hub.example.com/get) or https://hub.example.com/releases/<version>/longarm-windows-amd64.msi,
#    Default (recommended), "This machine runs jobs"; the tray starts the sign-in and shows the code.
#    Or one line (worker + client; the scripts carry the hub's address and CA):
curl -fsSL https://hub.example.com/install.sh | sudo sh -s -- --login --role both           # Linux / macOS
$env:LONGARM_LOGIN=1; $env:LONGARM_ROLE='both'; irm https://hub.example.com/install.ps1 | iex   # Windows (elevated: the MSI)

# 2. sign in - the one-liners and the tray do it; already installed:
longarmd login hub.example.com --role both     # worker + client: one code, one approval
longarm login hub.example.com                  # a client only
#   Open https://hub.example.com/link and enter code BCDF-GHJK  - in any browser: sign in (Microsoft,
#   Google or Apple account, as the hub offers), type the code, approve

# 3. run something
longarm ls                                     # your workers (this machine too)
longarm run --repo . -f -- pnpm test           # placed by the scheduler, in a fresh worktree
```

On Windows the worker runs as you, started at your logon, no service and no
password ([the user-session worker](#the-user-session-worker-runsession-the-default));
on Linux / macOS `install.sh` installs the longarmd service as the `sudo`
user. Everything about the sign-in: [Sign in with Microsoft Entra ID](#sign-in-with-microsoft-entra-id) and [with a Google, Apple or personal Microsoft account](#sign-in-with-a-google-apple-or-personal-microsoft-account);
installers and their options: [Install](#install); removing it:
[Uninstall](#uninstall).

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

## Usage

```sh
longarm ls                                         # CPU / memory / jobs / labels of your workers, one row per machine
longarm ls -o wide                                 # + claude/copilot sessions, disk, uptime, top processes, path
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
longarm jobs [--mine | --all] [--limit N]          # the newest 20 (0 = all); admin certificates: only yours unless --all
longarm doctor [--json]                            # checklist: sign-in, hub, daemon, firewall, your workers
```

**Which workers you see.** On a hub, `ls`, `ls -o wide`, `worker ls`,
`jobs`, the tray's Workers tab and the MCP tools show your own workers and
devices, the workers shared with you and the fleet workers (no owner) -
admins included: an admin certificate gets exactly a user's view there, and
there is no flag that widens it. Other users' machines are counted, never
listed, by `longarm admin summary` (admins; see below and
[security.md](security.md#what-admins-see)).

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
| 79 | `admin release push --allow-none`: no worker of the channel can update itself (MSI installs, user-session workers), nothing was rolled out; the message lists them | nothing to do: they update with the MSI (the tray's banner) or the install script |
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
  "job-id-short", "jobs-mine", "doctor-json", "worker-update", "ls-version", "agent-auth",
  "agent-handoff", "agent-sessions"]}
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
| `worker-update` | `longarm worker update <worker> [--latest\|--version X] [--wait] [--json] [--auto on\|off]` (the worker must also support it: `machine-update` in its `Node.Info` features), see [Updating](#updating-longarm-worker-update-and-the-longarm-update-service) |
| `ls-version` | `longarm ls` has the VERSION column; `ls --json` carries `version` and `updateAvailable` (`{"version", "reason"}`) |
| `agent-auth` | `run` / `exec` / `session start` / `attach --agent-auth`, `--agent-auth-shared-ok`, `--copilot-account`, MCP `agent_auth` (the worker must also support it: `agent-auth` in its `Node.Info` features), see [Agent credentials](#agent-credentials---agent-auth) |
| `node-ids` | node IDs: a worker target may be a name, a node ID, a unique ID prefix, `id:<prefix>` or `name:<name>`; `longarm worker rename`, `longarm admin rename`; IDs in listings and JSON (`workerId`, `id`) - see [Node IDs](#node-ids-and-targets) |
| `agent-handoff` | `longarm handoff` (see [Hand off an agent session](#hand-off-an-agent-session)) |
| `agent-sessions` | `longarm agent-sessions` |

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

### Agent credentials (`--agent-auth`)

When longarm starts Claude Code or Copilot CLI on a worker, it forwards
that agent's credentials from this machine with the job. Workers then need
no agent logins of their own, and the credentials are not stored on them.
The credentials travel like `--secret-env`: they go worker-direct (never
through the hub), stay in memory on the worker, are put into the process
environment, and are redacted from the job's log and the session
transcript.

```sh
longarm run w1 --wait -- claude -p "fix the flaky test" --dangerously-skip-permissions
longarm run w1 --wait -- copilot -p "fix the flaky test" --allow-all-tools
longarm session start w1 -s fix --repo . -- claude
longarm run w1 --agent-auth claude,gh -- pwsh -c "claude -p 'open a PR'"
```

| flag | |
|---|---|
| `--agent-auth auto\|claude\|copilot\|gh\|none` | What to forward. Repeatable or comma-separated (`--agent-auth auto,gh`). Default: `[agent_auth] default` in workers.toml, else `auto`. |
| `--agent-auth-shared-ok` | Forward to somebody else's worker with shared isolation as well (see [Which workers](#which-workers-get-them)). |
| `--copilot-account NAME` | Forward this account of the Copilot token pool. |

The flags apply to `run`, `exec`, `session start` and `attach`. `attach`
forwards only when it may create the session: attaching to a running
session sends nothing. The MCP tools `fleet_exec` and `fleet_job_submit`
take `agent_auth` (a list) and forward only the agents in
`LONGARM_MCP_AGENT_AUTH` (default `claude,copilot`; `gh` only when listed;
`none` turns it off). They never forward to a worker that would need
`--agent-auth-shared-ok`.

**`auto`** forwards an agent's credentials when the command's program is
that agent. It looks at the base name of `argv[0]` only, case-insensitive
and without `.exe` / `.cmd` / `.bat` / `.ps1` / `.js`: `claude` or
`copilot`. It does not look into a command wrapped in a shell
(`pwsh -c "claude ..."`, `sh -c`, `cmd /c`, `npx`), nor into MCP's
`command` (it runs in a shell). For those, name the agent:
`--agent-auth claude`. `gh` is never forwarded automatically. `none`
forwards nothing.

To turn forwarding off globally (then only an explicit `--agent-auth`
forwards), or to choose accounts, use workers.toml:

```toml
[agent_auth]
default = "none"              # "auto" (default), "none", or a list such as "claude,copilot"
copilot_gh_user = "octocat"   # Copilot gets `gh auth token --user octocat`
gh_user = "octocat"           # --agent-auth gh: `gh auth token --user octocat`
```

#### Where the credentials come from

- **Claude Code**:
  - The `env` block of `settings.json` in `$CLAUDE_CONFIG_DIR`, else in
    `~/.claude`. This is where a proxy setup keeps `ANTHROPIC_BASE_URL`,
    `ANTHROPIC_AUTH_TOKEN` and the model ids.
    - Names with `TOKEN`, `KEY`, `SECRET` or `PASSWORD` in them, and
      `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY` and
      `CLAUDE_CODE_OAUTH_TOKEN`, go as secrets.
    - The rest go as plain job environment. Plain values are stored in
      `job.json` and shown by `jobs --json`, like `-e`.
  - `ANTHROPIC_API_KEY` and `CLAUDE_CODE_OAUTH_TOKEN` from longarm's own
    environment. These win over the file.
  - Signed in with a Claude subscription only (`~/.claude/.credentials.json`,
    or the macOS keychain), with none of the above: nothing is forwarded.
    longarm says: run `claude setup-token` and set `CLAUDE_CODE_OAUTH_TOKEN`,
    or use an API key or proxy token. The subscription's OAuth refresh
    token is never copied: it is a long-lived account credential
    ([why](security.md#forwarded-agent-credentials)).
  - With `CLAUDE_CODE_USE_BEDROCK` / `_VERTEX` / `_FOUNDRY` in the env block,
    the env block is forwarded. The cloud credentials are not: they are the
    worker's own.
- **Copilot CLI**: the first of these is forwarded as `COPILOT_GITHUB_TOKEN`:
  1. `--copilot-account NAME`: that account of the token pool.
  2. `COPILOT_GITHUB_TOKEN`, `GH_TOKEN` or `GITHUB_TOKEN` from longarm's
     environment.
  3. `[agent_auth] copilot_gh_user`: `gh auth token --user <login>`.
  4. `gh auth token`: gh's active account.
  5. The token pool `~/.copilot/accounts/tokens.json`, if there is one:
     `{"<account>": "<token>", ...}` (or `{"<account>": {"token": "..."}}`).
     Accounts are used in turn, by name. Only the position is kept, in the
     longarm config dir; tokens are not.

  Copilot CLI takes gh's OAuth tokens (`gho_...`) and fine-grained PATs
  with the Copilot permission, not classic PATs (`ghp_...`).
- **gh** (`--agent-auth gh`, opt-in only, never with `auto`): `GH_TOKEN`
  for agents that use git or gh themselves. It comes from `gh auth token
  --user <gh_user>` if configured, else from `GH_TOKEN` / `GITHUB_TOKEN`,
  else from `gh auth token`. This gives the job the token's full scope. To
  fetch the job's repository, `--git-auth forward` is enough; it never
  reaches the job.

What you give yourself wins: an `-e NAME=...` or `--secret-env NAME` of
the same name replaces the forwarded value.

With `auto`, an agent without credentials on this machine is a note on
stderr, and the command runs without them. With an agent named explicitly,
it is an error (exit 2). longarm names on stderr what it forwards and from
where (`forwarding agent credentials to w1: claude: settings.json env`),
never a value.

#### On the worker

- Credentials are handled like `--secret-env`. They are in memory only,
  then in the process environment. They are redacted from `out.log`
  (`logs`) and the session transcript (`logs --session`), and are never in
  `job.json`, the audit log or longarmd's log.
  - `jobs --json` (`spec.agentAuth`), `sessions --json` (`agentAuth`),
    `job.json` (`agent_auth`) and the audit log (`agent_auth`) name the
    forwarded agents, never values.
  - `secretEnvNames` lists the secret variables' names.
  - The live stream of an attached terminal is not redacted: it is your
    own terminal.
- With Copilot credentials, the job, session or exec gets a private
  **`COPILOT_HOME`** of its own:
  - `<data>/agent-home/<j|s|x>-<id>-<random>`, inside the user's space with
    per-user isolation, accessible to that account only. Parallel jobs
    don't share Copilot state, and nothing leaks between jobs or users.
  - Copilot keeps its config, session state and logs there. It may also
    cache the token there. That is the only place a forwarded credential
    can reach the worker's disk, and longarm does not write it.
  - The directory goes away with the job or session (GC, `[gc] keep`;
    `longarm admin gc`, or a session replaced by a new one of its name) and
    right after an `exec`. Leftovers are collected after an hour.
  - `-e COPILOT_HOME=<dir>` keeps the account's own `~/.copilot` (or
    another directory) instead. `handoff` does this to resume a Copilot
    session it copied there.
  - Note that the state of a session run with a private home is not in
    the account's `~/.copilot`, so the worker-side session list doesn't
    show it.
- The worker must announce the `agent-auth` feature (`Node.Info`).
  - Explicit `--agent-auth` on an older longarmd: exit 2.
  - `auto` on an older longarmd: the command runs without forwarded
    credentials, with a note.

#### Which workers get them

- **Your own workers**, and workers without a hub record (workers.toml):
  yes.
- **Somebody else's worker** (shared with you, or a fleet worker) **with
  per-user isolation**: yes. The job runs as your own OS account there.
- **Somebody else's worker with shared isolation**: refused (exit 2).
  Other users' jobs run there as the same OS account and could read the
  job's environment. `--agent-auth-shared-ok` forwards anyway, with a
  warning.
- **Admin workers** (they run as SYSTEM): never with `auto`, which only
  prints a note. Only when the agent is named (`--agent-auth claude`), and
  only to your own admin worker.

"Your own" is the hub's word, as the owner it records. With end-to-end
trust on the client (`longarm trust require on`), it is only a worker
whose node key you endorsed with your own user key. A worker endorsed by
another user you trust, or one the hub merely says is yours, then counts
as somebody else's. Isolation is always the worker's own report over the
verified connection. Under end-to-end trust, so is the admin flag
([security](security.md#forwarded-agent-credentials)).

#### Recommended invocations for unattended agent jobs

Agents ask before using tools. A job has nobody to answer, so it needs
permission flags. longarm doesn't add them: choose them yourself.
Bypass/yolo modes let the agent run anything the job's account can,
including reading the forwarded credentials. Use them on workers and
repositories you trust.

```sh
# Claude Code: prompt from a file (memory-only on the worker), streamed JSON
# output, all tools allowed. Narrower: --permission-mode acceptEdits, or
# --allowedTools "Bash(go test:*)" Edit.
longarm run w1 --repo . --stdin-file prompt.md --wait --name fix-x -- \
  claude -p --dangerously-skip-permissions --output-format stream-json --verbose --session-id <uuid>
# retry with the same conversation:  ... -- claude -p --resume <uuid> ...

# Copilot CLI: all tools, no questions, no colors, logs in the job's dir.
longarm run w1 --repo . --wait -e COPILOT_AUTO_UPDATE=false -- \
  copilot -p "$(cat prompt.md)" --yolo --no-ask-user --no-color
# narrower: --allow-all-tools without --yolo, or --allow-tool / --deny-tool
```

- `--wait` / `--follow` return the agent's exit code.
- `logs <id>` shows Claude's stream-json lines or Copilot's transcript.
- `--model` picks a model for either agent.
- Give parallel agents separate worktrees (`--repo`) or directories.

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
`file-tail`, `session-env` (sessions get `LONGARM_WORKER` and `LONGARM_SESSION`);
for `longarm handoff`: `file-lstat`, `handoff-direct`, `handoff-push`.
Against a longarmd without them `session start|send|stop` and
`tail` exit 2 ("does not support it; update longarmd") before doing
anything; `sessions` lists such a worker's sessions without the new fields.

### Agent sessions in the tray: watch, attach, steer

Click **Claude Code** or **Copilot CLI** on a worker's card (the tray's
Pool tab) to open the **Sessions window**: every Claude Code / Copilot CLI
session of your workers, grouped by worker, with its state - ● working,
◐ waiting for input, ○ ended, ◇ running outside Longarm (a terminal on the
worker's desktop). A session opens in a tab:

- **Terminal** - the session's live terminal in the window (xterm.js), as
  `longarm attach` shows it: replay, then live; type into it, resize the
  window. Closing the tab detaches; the session keeps running.
- **Conversation** - the agent's transcript as a conversation: your
  prompts, its answers, tool calls with their results, live.
- **Steer** - a message box under the session: Enter sends the text and
  Enter, plus Esc (interrupt), "y" and Ctrl-C buttons. It types into the
  session's terminal like `longarm session send ... --if-idle-ms 1500`:
  when somebody else is typing in it right then, nothing is sent and the
  box says so.
- **New session** starts `claude` or `copilot` (optionally resuming a
  transcript) in a Longarm session on a worker, with your agent
  credentials forwarded as `session start` does.

Agents Longarm started (`longarm attach -- claude`, `longarm session
start`, the tray, `longarm handoff`) have all three views. An agent started
in a terminal on the worker's desktop shows its conversation only - Longarm
has no access to that terminal; **Continue here** resumes it in a Longarm
session (a fork while the original still runs). The window works against
workers of any version (it uses the existing `Sessions` and `Files` calls);
transcripts are read with your rights and shown, never stored.
`docs/agent-sessions-ui.md` has the design and the daemon API
(`/v1/agent-sessions`, the WebSocket attach bridge).

### Hand off an agent session

Move an in-progress Claude Code or Copilot CLI conversation - and the code
it works on, uncommitted and untracked changes included - between this
machine ("local") and workers, and resume it there: local -> worker,
worker -> worker, worker -> local. The client orchestrates everything over
the worker-direct channels (`Files`, `Exec`, `Workspace`, `Sessions`); the
hub never sees a transcript.

```sh
longarm agent-sessions                        # resumable sessions here: AGENT ID MODIFIED SIZE PROJECT TITLE
longarm agent-sessions w1 --repo-path ~/src/app --json
longarm handoff --to w1 --latest              # the newest session of this checkout -> w1, then attach
longarm handoff --from w1 --to w2 --session <id> --no-attach
longarm handoff --from w2 --to local --session <id>   # a new worktree next to your checkout; the agent runs here
```

**`longarm handoff --to <worker|local> [--from <worker|local>] [--agent claude|copilot] (--session ID | --latest) [--name NAME] [--via relay|remote|auto] [--via-threshold 20M] [--git-auth forward] [--allow-secrets] [--keep-branch] [--no-attach] [--json]`**, `longarm handoff --cleanup`

1. **Locate.** `--session ID` (an id `agent-sessions` lists), or `--latest`:
   the most recently modified session whose project directory is in the
   current checkout (locally), in `--repo-path`, or anywhere on the machine
   (a worker). `--agent` limits the search to one agent. `--from` defaults
   to where longarm runs: the worker of the longarm session it runs in
   (`LONGARM_WORKER`, which longarmd sets in every session together with
   `LONGARM_SESSION`; worker feature `session-env`), else local. A copy
   that was handed off already is refused (exit 2): hand it off from where
   it went.
2. **Quiesce the source.** On a worker, the longarm session running the
   agent (its command names the session id, or it runs the agent in the
   session's directory; `--source-session NAME` says which) is stopped
   once it had no input or output for `--idle` (default 5s, observed from
   the client, no clock comparison; at most `--idle-timeout`, default 10m,
   else exit 75), with `session stop --grace`. Running handoff from inside
   that very session is refused (exit 2). Locally, and on a worker outside
   longarm sessions, a process that still has the session open is refused
   (exit 75, with how to exit it): Claude Code's
   `~/.claude/sessions/<pid>.json` naming the session with that pid alive;
   Copilot CLI's `open-sessions-state.json` entry refreshed within 3
   minutes (best effort).
3. **Move the code.** A snapshot commit of the source worktree - HEAD plus
   uncommitted and untracked files, `.gitignore` respected - is made with a
   temporary index (`read-tree HEAD`, `add -A`, `write-tree`,
   `commit-tree`) and kept on `refs/longarm/handoff/<id8>-<time>` in the
   source checkout: the user's branch, index and files are not touched.
   The target gets a new worktree of it on branch `handoff/<id8>` (`-2`,
   ... when taken) with HEAD at the original commit and the changes
   uncommitted again (untracked files untracked; the staged / unstaged
   split is not kept):
   - a worker: `<home>/longarm-handoff/<repo>-<id8>`, made like `sync`
     (only commits the worker's mirror lacks travel; `--git-auth forward`
     has the worker fetch the base commits from the remote first, see
     [Fetching with your credential](#fetching-with-your-credential---git-auth-forward),
     so only the snapshot's delta is shipped). It is not garbage-collected:
     remove it with `longarm exec <w> -- git -C <dir> worktree remove --force .`
     when done;
   - worker -> worker: the source makes a bundle of the snapshot minus what
     the target has (after the target's remote-first fetch with
     `--git-auth forward`) and the target fetches it from the source
     directly (below), else longarm relays it;
   - local: `git worktree add` of your checkout (`--local-repo`, default
     the current directory's; it must be a checkout of the same repository)
     at `<checkout>-handoff-<id8>` next to it, or under `--worktree-dir` /
     `LONGARM_HANDOFF_DIR`. Your current checkout is never modified.

   Git LFS: a local source ships the snapshot's LFS objects (as `--lfs
   ship`); from a worker, LFS files arrive as pointer files.

**How the code travels (`--via relay|remote|auto`).** The default is
`auto` (`[handoff] via` / `via_threshold` in workers.toml change it): the
size of the objects the target lacks (git's on-disk size of what a bundle
would carry, after the remote-first fetch with `--git-auth forward`) decides
- below `--via-threshold` (default 20M) **relay**, from it on **remote**.

- **relay** keeps the commits on longarm's own end-to-end channels. Local
  -> worker: as `sync`. Worker -> local: a bundle via this machine. Worker
  -> worker: **directly** between the workers (worker feature
  `handoff-direct`): this machine registers a one-time offer on the source
  worker - that one bundle, for the target worker's certificate only, for
  whoever presents a random 32-byte token, for 10 minutes, once - and asks
  the target to pull it; the target dials the source with its worker
  certificate over the port-link legs (tcp, tailnet, longnet relay - the hub
  relay only forwards ciphertext), and both apply the port-link rules for
  the owner (a user of the worker, owner / shares, disabled users; with
  end-to-end trust the pinned key, and the other worker must be endorsed by
  the owner). The offer is given by the owner's own device over its mTLS
  connections to the workers, never through the hub, so the hub can't forge
  or widen it. Relays carry worker <-> worker traffic only for the worker
  pairs the hub lists (those of port links), so with a hub this machine
  first asks it to list the pair for the transfer (`Fleet.HandoffPair`, at
  most 10 minutes, released right after; the caller's own workers or fleet
  workers only, never another user's - admins included); the hub's relays,
  embedded and external (`longarm-relay --hub`), then carry it. The source copies the bundle into its staging area and
  deletes it after the one fetch or at expiry; the target checks the size
  and sha256 against what the source reported before importing it (its
  prerequisites must be in the mirror). When that fails - a worker without
  the feature, no leg connects, a policy refuses - longarm relays the
  bundle through this machine instead, with a note (keep it online until
  the handoff is done). `--json` reports `"transfer"` (`upload`, `direct`,
  `relay`, `bundle`, `remote`) and `"relayedBytes"` (what this machine
  relayed between workers).
- **remote** pushes the snapshot commit to a temporary branch
  `longarm/handoff/<user>/<id8>-<yyyymmddhhmm>-<random>` of the repository's https
  remote (Azure DevOps, GitHub, ...) - `git push <remote>
  <snapshot>:refs/heads/<branch>`: your branch, index and upstream are not
  touched - and the target fetches it from there (a worker with the
  forwarded credential, as `--repo <url> --git-auth forward`; locally with
  your own git). A local source pushes with your own git credentials; a
  worker source pushes with the forwarded credential (worker feature
  `handoff-push`): **with `--git-auth forward` the credential then also
  grants write access, for that one push of that exact URL** - handed out
  by the worker's loopback credential helper as for a fetch, pushing exactly
  that one ref (`--no-follow-tags`, `push.followTags`, push options and
  submodule recursion off, to the URL rather than a remote name, no forced
  update; the same for a local push and every delete), with the
  repository's hooks off, only to `longarm/handoff/` branches, never stored
  in a config file. Workers need `--git-auth forward` for this path
  (without it, relay). The branch is deleted once the target has the code
  (by this machine with your git when it has a checkout of the repository,
  else by the source worker with the forwarded credential); if that fails
  longarm prints the `git push <url> --delete <branch>` that does it.
  `--keep-branch` keeps it; `longarm handoff --cleanup [--json]` (in your
  checkout) lists your `longarm/handoff/<user>/*` branches and deletes
  those older than an hour.

  Before pushing, a **secret check** scans the changed and untracked files
  (built-in rules: private keys, AWS / GitHub / GitLab / Slack / Google /
  OpenAI / Anthropic tokens, Azure storage keys, credentials in URLs, JWTs,
  `.env`-style `SECRET=` / `TOKEN=` / `PASSWORD=` assignments, key and
  credential files by name; placeholders don't count). A hit keeps the
  snapshot off the remote - a pushed branch can be read by everybody with
  access to the repository and lingers in caches after it is deleted - and
  longarm relays instead, naming file and rule (never the value);
  `--allow-secrets` pushes anyway. A changed file over 50 MB, no https
  remote, no credential, a worker too old, or a push the remote refuses
  (branch policies, permissions) also fall back to relay, with a note.
4. **Move the session.** The transcript and its state are copied to where
   the agent looks for them on the target (below), with absolute paths
   rewritten from the source worktree to the target's, per JSON string
   value: in known path fields (`cwd`, `file_path`, `filePath`, `gitRoot`,
   ...) a leading source path is replaced and the separators converted
   (Windows <-> Unix); in other strings only exact occurrences of the source
   path at a path boundary are replaced. Everything else stays byte for
   byte, and the session id is kept. What the target already has under
   that id (the session was there before) is moved to
   `<agent dir>/longarm-handoff-backup/<id>-<time>/` first, never
   overwritten. The source copy stays as it is, as a backup, with a marker
   (`<id>.handoff.json` next to a Claude transcript, `longarm-handoff.json`
   in a Copilot session directory: target, time, worktree, snapshot);
   `agent-sessions` lists such copies only with `--all`.
5. **Resume.** A worker: a longarm session (`--name`, default
   `handoff-<id8>`) in the target worktree running `claude --resume <id>` /
   `copilot --resume=<id>`, then `longarm attach` (`--no-attach` / `--json`:
   printed instead). Local: the agent runs in this terminal (`--no-attach`:
   the command is printed).

Not carried over: running processes - tool calls in flight, background
shells, MCP server processes, dev servers. The agent starts what it needs
anew on the target. Claude Code may ask to trust the new directory.

**Preflight** (before anything changes): the agent is installed on the
target (else exit 2) and not older than the version that last wrote the
session (else exit 2; `--force` makes it a warning); warnings when it does
not look logged in there (`claude auth status`; Copilot's `config.json`)
and for MCP servers configured on the source (user-level and
project-level; Claude's `.mcp.json` travels with the code) that the target
lacks - server names only, read in memory. The session's files may be at
most `--max-size` (default 64M; else exit 2).

**Safety.** Only your own machines: a worker with an owner other than you
(as the worker says, or as the hub lists it) is refused as `--from` and as
`--to` (exit 77) - also when it is shared with you, and also for admin
certificates (they act as their user); the worker refuses the handoff calls
(`Handoff.Offer` / `Fetch` / `Pull`, `Workspace.PushHandoff`) of anybody but
its owner as well. Fleet workers (no owner) and workers without a hub
follow the rules below. Only your own sessions: on a worker, the longarm session
running the agent must be yours (else exit 77); without one, the worker's
agent state must be yours - per-user isolation, your own worker, or a
setup without a hub - else exit 77 (a fleet or shared worker in shared
isolation keeps every user's agent state in one account). A target in
shared isolation that is not yours gets a warning: its users' jobs can read
the transcript there. Admin workers are used only when named (`--from` /
`--to`), never as the implicit `--from`. The file names of a session come
from the source and are untrusted: each must be a plain relative path (no
absolute paths, drive letters, `..` or empty components, backslashes or
control characters; on Windows targets no `:`, device names such as `CON`,
or trailing dots and spaces) inside the session's own files (its
transcript, session directory, `file-history/<id>/` ...), at most 20000 of
them; one bad name refuses the whole handoff (exit 2) before anything is
written. No existing directory on the way may be a symlink or junction (the
worker needs the `file-lstat` feature), files are created exclusively, and
each file may be no larger than the source listed it (within
`--max-size`). This machine names its temporary copies by number, never by
a source's name. Transcripts travel only inside the
mTLS connections to the workers; this machine keeps its copy in a private
temporary directory that is removed afterwards. longarm never prints or
logs transcript contents; the workers' audit logs record file paths and
sizes (`Files`), the git commands (`Exec`) and the session's command, not
contents. `agent-sessions` prints titles (the agent's title or the first
prompt) only.

**Exit codes:** 0 done; 2 usage, a handed-off copy, no agent or an older
one on the target, too large, not a git checkout; 3 no such session; 75
the session is open in a process, or the source session did not become
idle in time; 77 not your session; 255 transport and worker failures (the
source session may have been stopped by then: resume it there with the
agent's resume command).

**Storage** (what longarm reads and writes):

| | Claude Code (verified with 2.1) | Copilot CLI (layout verified with 1.0; resuming a copied session is experimental) |
|---|---|---|
| state dir | `~/.claude` (`$CLAUDE_CONFIG_DIR`; on a worker as `claude auth status --json` reports it) | `~/.copilot` (`$COPILOT_HOME` locally) |
| transcript | `projects/<dir>/<id>.jsonl`; `<dir>` = the project path with every character but `A-Za-z0-9` as `-` (`C:\src\app` -> `C--src-app`; over 200 characters: refused) | `session-state/<id>/events.jsonl` |
| also moved | `projects/<dir>/<id>/` (subagents, tool results), `file-history/<id>/`, `tasks/<id>/`, `todos/<id>-*.json` | the rest of `session-state/<id>/` (`workspace.yaml` with `cwd` / `git_root`, checkpoints, files) |
| open in a process | `sessions/<pid>.json` (`sessionId`, `pid`) | `open-sessions-state.json` (`refreshedAt`) |
| resume | `claude --resume <id>` in the project directory | `copilot --resume=<id>` |

**`longarm agent-sessions [<worker>|local] [--repo-path DIR] [--agent claude|copilot] [--all] [--json]`**

Lists a machine's resumable sessions, newest first; `--json`:
`{"machine": "w1", "sessions": [{"agent", "id", "projectDir", "modified",
"title", "size", "version", "handedOff": {"handedOffTo", "at", ...}}]}`.

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

### Remote desktop: `longarm desktop`

```
longarm desktop cpc-alice-x7k2q                 # opens it (the tray: "desktop" on its card)
longarm desktop cpc-alice-x7k2q --pick          # choose its Dev Box again
longarm desktop cpc-alice-x7k2q --devbox arm1   # say which Dev Box it is ([project/]name)
longarm desktop cpc-alice-x7k2q --web           # the browser client instead of Windows App
longarm desktop cpc-alice-x7k2q --print         # print the link (or the forward's address)
longarm desktop cpc-alice-x7k2q --url 'ms-avd:connect?...'   # remember this link
longarm desktop cpc-alice-x7k2q --forget
```

What the worker is decides how its desktop opens (longarmd reports it in
`Node.Info`; workers before this release don't, update them first):

- **Microsoft Dev Box**: opens in **Windows App**, which connects through
  Azure Virtual Desktop itself - RDP Shortpath, Entra sign-in, the Dev
  Box's Conditional Access. Longarm carries none of it; it only finds the
  link. longarmd recognizes a Dev Box by its Azure instance metadata (the
  Dev Center it was made by); `longarm desktop` then asks that Dev Center
  for your Dev Boxes with your Azure CLI sign-in (`az login --tenant
  <tenant>`; Longarm stores no Azure tokens) and for the chosen one's
  `ms-avd:` link. Which Dev Box a worker is can't be told from either side,
  so the first time it takes the only Dev Box no other worker has, or asks
  (`--devbox` without a terminal). The choice, the link and the Dev Center's
  endpoint are kept in `<config>/desktop.json`; after that the link opens
  without reaching the worker, so a stopped or hibernated Dev Box starts as
  Windows App connects. Without the Azure CLI it opens
  https://devbox.microsoft.com.
- **Windows 365 Cloud PC** (not a Dev Box): opens https://windows365.microsoft.com.
- **Any other Windows worker with Remote Desktop on**: a forward to its
  Remote Desktop port (like `longarm forward`, listed by `forward --list`)
  opened in mstsc on Windows, Windows App on macOS, FreeRDP on Linux; it
  ends when mstsc closes (elsewhere: Ctrl+C). The Windows firewall rule for
  Remote Desktop can stay off - the forward connects to 127.0.0.1 on the
  worker. With per-user isolation forwards only reach your own processes'
  ports, so this needs an admin certificate there.

### SSH / scp / VS Code Remote-SSH

```sh
longarm ssh w1                                     # ssh without any ssh config (what the tray's ssh buttons run)
longarm ssh w1 -- git status                       # a command
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
longarm ls                               # from the hub (STATE online/offline 3d/draining/pending); --direct probes workers
longarm jobs [w1] [--state running]      # fleet-wide job index (--json: "source": "hub"; --hub-only: no fallback)
longarm run -- pnpm test                 # placed by the scheduler (= --on auto)
longarm run --type build --mem-gb 8 --require os=linux --repo . -- make   # reservation, labels, repo cache affinity
longarm run --no-queue -- ...            # fail (exit 75) instead of queueing when no worker passes the gate
longarm exec -- hostname                 # short command on the least loaded worker
longarm wait <id>; longarm logs <id> -f; longarm kill <id>   # worker optional: found through the hub
longarm admin nodes | approve | revoke | drain | undrain | label <node> k=v | join-codes | audit
longarm admin summary [--rows]           # the whole fleet as counts per user (admin nodes lists what your user sees)
longarm admin audit [--node N]           # the records about your user and your own hub actions; others' only counted
longarm-hub audit [--node N] [--json]   # on the hub host (operators): the complete audit trail
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

**While the hub is down** (a redeploy, an outage), everything that talks to
a worker keeps working over cached endpoints and pinned identities (the
worker's certificate from the CA, end-to-end trust as configured): `exec`,
`run <worker>`, `attach`, `logs`, `get` / `put`, sessions, port forwards,
`longarm mcp`, the tray. Longarm says so once ("hub ... unavailable; direct
connections to workers still work"); the tray's header shows "hub
unavailable · direct connections still work". Only what needs the hub
waits: placement without a worker (`--on auto`), workers not in the cache,
sign-in, new port links, the panel. Relayed paths survive a hub restart
through an external relay ([below](#external-relays-relayed-connections-that-survive-hub-restarts));
through the hub's own relay they reconnect by themselves once it is back
(`attach`, `logs -f` and `wait` reconnect with backoff; a forwarded
connection opened meanwhile waits up to 30 s for the worker,
`LONGARM_FORWARD_WAIT`).

**Metrics direct-first.** The client daemon (the tray's) keeps a connection
per leg to the workers this user may use (the hub's list for this user, and
its cache) and streams each worker's metrics directly from it; the hub's
metrics only fill in for workers it doesn't reach or stream from. At most
32 workers get a direct stream (`LONGARM_POOL_MAX_DIRECT`; those with this
client's jobs first, then the fastest); without a viewer (tray closed, no
`longarm ls`) for 5 minutes the daemon idles: no direct streams, legs probed
every 30 s. The tray shows per worker where its numbers come from and how
old they are ("metrics direct · updated 2s ago"); `longarm ls -o wide` has a
METRICS column (`direct 2s`, `hub 1s`, `hub 3m (stale)`), and `longarm ls`
uses the running daemon's direct metrics over the hub's.

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
longarm admin user code alice [--device alice-desktop]     # a join code for the first device (user added with --no-code)
longarm admin user set alice --team web --team infra       # replace teams; quota flags change the quota
longarm admin user set alice --clear-quota                 # back to the default quota
longarm admin user ls [--json]                             # number of devices and workers, teams, usage, quotas
longarm admin user disable|enable alice                    # refused on the hub and every worker
longarm admin user remove alice [--purge]                  # revoke all of alice's devices and workers; --purge:
                                                           # workers delete her jobs, data and OS account
longarm admin summary [--rows] [--json]                    # the fleet per user: workers / admin workers / devices,
                                                           # online, worker versions; --rows: each user's
                                                           # machines by version (aggregates only).
                                                           # No names; audited
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
longarm ls                                # an OWNER column appears once a worker has an owner; ADMIN: the machine's admin worker
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

### Sign in with a Google, Apple or personal Microsoft account

Besides Entra (work and school accounts of one tenant), a hub can let
people sign in with **personal accounts**: Google, Apple and Microsoft
accounts (outlook.com, live.com, hotmail.com ...). Each provider is a
`[auth.<provider>]` block in hub.toml; any combination works, also next to
`[auth.entra]`. The panel's login page then has one button per provider
(`/auth/login` without a provider shows a page to choose), and `/link` -
the page `longarm login`, `longarmd login`, the tray and the MSI with
`LOGIN=1` send you to - signs you in the same way.

The account maps to a Longarm user exactly like an Entra user: **created
on first sign-in** (named after the verified e-mail address's local part,
`alice@example.com` -> `alice`; without an address, after the provider:
`google`, `apple2`), bound to the provider's stable subject (`sub`), not to
the address. So **every machine one account links is that one user's**:
`longarmd login` on a box at home and `longarm login` on a laptop, both
approved with the same Google account, give the laptop a client
certificate of that user and the box a worker owned by it - its jobs are
placed there, nobody else's. Another account is another user (accounts of
different providers too; there is no merging by e-mail address).

Only the browser path exists for these providers: no `--az` (that is
Entra's token), and no app-only sign-in. As with Entra, once any provider
is configured only sessions opened by a provider's sign-in may approve at
`/link` (a password login could be a shared admin account).

```toml
[auth.google]
client_id = "<id>.apps.googleusercontent.com"
client_secret_file = "/etc/longarm-hub/google-client-secret"   # root:longarm-hub 0640
admins = ["you@example.com"]       # admin certificates and the panel's admin role
allowed = []                       # who may sign in (empty: every account of the provider);
                                   # entries: verified e-mail "user@example.com", "*@example.com",
                                   # or the provider's subject "sub:<id>" (exact case)

[auth.microsoft]                   # personal Microsoft accounts (work/school: [auth.entra])
client_id = "<application id>"     # a GUID
client_secret_file = "/etc/longarm-hub/microsoft-client-secret"
admins = ["you@example.com"]

[auth.apple]
client_id = "com.example.longarm.signin"   # the Services ID
team_id = "ABCDE12345"
key_id = "KEY1234567"              # the "Sign in with Apple" key
private_key_file = "/etc/longarm-hub/apple-AuthKey.p8"   # checked when the hub starts
```

Registration, with `<public_url>` the hub's `public_url` (the redirect
URIs must match it exactly):

- **Google** (Google Cloud console -> Google Auth Platform): an External
  app with the scopes `openid`, `email`, `profile` (non-sensitive: publish
  it "In production" without a review, else only test users get in); a
  client of type *Web application* with the redirect URI
  `<public_url>/auth/callback/google`. Only addresses Google marks
  verified count (name, `admins`, `allowed`).
- **Microsoft account**: an app registration with *Supported account
  types* = personal Microsoft accounts only (`az ad app create
  --sign-in-audience PersonalMicrosoftAccount --web-redirect-uris
  <public_url>/auth/callback/microsoft`), and a client secret (`az ad app
  credential reset`). The hub accepts ID tokens of the consumer tenant
  only; a work account that reaches this button is refused (it signs in
  with `[auth.entra]`). The address is the account's sign-in name.
- **Apple** (Apple Developer -> Certificates, Identifiers & Profiles): an
  App ID with *Sign in with Apple*; a Services ID (the `client_id`) with
  *Sign in with Apple* configured for the hub's domain and the return URL
  `<public_url>/auth/callback/apple`; a key with *Sign in with Apple* (its
  `.p8` downloads once: `private_key_file`, its ID: `key_id`). Apple posts
  the result back (`response_mode=form_post`); the hub signs a short-lived
  ES256 client secret per sign-in. A "Hide my e-mail" relay address is the
  user's address as far as the hub is concerned.

Each sign-in carries a state (single use, in a MAC'd cookie), a nonce and,
for Google and Microsoft, PKCE; the hub redeems the code with its client
credential and checks the ID token itself (signature against the
provider's published keys, issuer, audience = `client_id`, expiry, nonce).
A change to `admins` / `allowed` applies at the next sign-in and at every
`/link` approval. Removing a provider's block locks its users out of the
panel and `/link` (their devices keep working until their certificates
expire; `longarm admin user disable` ends them at once).

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
longarm daemon start | stop | status | events     # client session daemon (also: tailscale/longnet legs of one-shot longarm); start replaces an older daemon
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
# relay = "auto"                   # auto (default): when every listen URL is loopback (sign-in
#                                  # installs: longarmd login, the tray, the MSI's session and admin
#                                  # workers), also listen on the hub's longnet relay - the address
#                                  # comes with each hub session, so nothing to configure, also after
#                                  # an upgrade - with hole punching for a direct path; "off": never
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
"People" page, see only "My jobs", the nodes read-only and "Me"). With
multi-tenancy an administrator's "Nodes" and "Jobs" pages show what its
user sees (its own and shared machines, fleet workers; its own jobs) and
the "Fleet" page the whole fleet as counts per user, worker versions and,
optionally, each user's versions (`GET /api/admin/summary`; aggregates
only, no names, audited).
Pages: nodes (online state, CPU and
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
title bar). The popup shows the load of every worker you may see (your own,
shared with you, the fleet's - admins too, there is no "all workers" switch)
plus this client's link to each: active leg and path (tcp, tailscale direct/peer-relay/DERP, longnet
direct/relay-udp/relay-tcp), NAT types, RTT with a 5-minute sparkline. Data
comes from `longarm daemon` (hub events when the hub is reachable, otherwise
streamed from the workers; the popup shows which). Tabs: workers, my jobs
(the jobs started from this computer, like `longarm jobs --mine`: a click
opens a job's command, times and output right under it; kill; there is no
"everyone" switch - `longarm jobs --all` lists your jobs from other
computers and team-shared ones), forwards, this worker (longarmd: drain, limits, toolchain, jobs,
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

## Advanced: self-hosted without sign-in (CA and `admin issue`)

No hub and no Microsoft sign-in: one admin machine holds the fleet's CA and
issues certificates by hand; the client finds workers in `workers.toml`.

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

A hub can be added later: `longarm-hub --data-dir <dir> ca import ~/.config/longarm/ca` keeps
the certificates issued so far ([Fleet with longarm-hub](#fleet-with-longarm-hub)).

## Installers, transport racing, self-update

### Install

| Platform | Package | Unattended |
|---|---|---|
| Windows | `longarm-windows-<arch>.msi` (longarm, longarmd, longarm-tray if built; Default / Custom dialog) | `msiexec /i longarm-windows-amd64.msi /qn MODE=default ROLES=both LOGIN=1 [LABELS=pool=devbox] [ADMIN_WORKER=1]` (the built-in hub, user-session worker, no password; sign in with the tray or `longarmd login --role both`); another hub: `MODE=custom HUB=hub.example.com LOGIN=1`; join codes and services: the table below |
| Linux | `longarm_<v>_<arch>.deb/.rpm` (longarm, longarmd, `longarmd@.service`) | `sudo env LONGARM_ROLES=worker LONGARM_HUB=hub:7443 LONGARM_JOIN_CODE=K7P-Q2M LONGARM_USER=$USER apt install ./longarm_*.deb` (or `/etc/longarm/join.env`) |
| macOS | `longarm-darwin-<arch>.pkg` (longarm, longarmd in `/usr/local/bin`; Developer ID signed and notarized: [macOS installer and MDM](#macos-installer-pkg-and-mdm)); Homebrew cask | a configuration profile (domain `com.longarm`) or `defaults write /Library/Preferences/com.longarm ...`, then `installer -pkg` |
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

#### Upgrading with Longarm running

Install the newer MSI over the old one (double-click, `msiexec /i`, or the
tray's update banner) while the tray, the user-session worker and the admin
worker run: there is no "files in use" / "unable to automatically close
all requested applications" dialog and no reboot, and running jobs and
sessions keep running. What happens (`internal/msiupgrade`;
`packaging/windows/longarm.wxs`):

1. **Before the in-use check** (as you, before `InstallValidate`): your
   tray is asked to exit - its stop event, a named kernel event
   `Global\LongarmStop-<pid>` that only its user, SYSTEM and Administrators
   may set - and ended by its process id after 10 s, or at once when it
   has none (an older version). The tray is Longarm's only process with a
   window, the only one Windows Installer's in-use check would list.
   Restart Manager is off (`MSIRESTARTMANAGERCONTROL=Disable`): it can't
   close windowless processes, which is what showed the dialog.
2. **Before the files are replaced** (elevated, before `StopServices` and
   `InstallFiles`): the services that run (`longarmd-admin`, `LongarmUpdate`,
   a `RUN=service` `longarmd`) are stopped and waited for, then every user's
   tray, worker host (`longarm-tray --worker-host`, `longarmd session-mode
   run`) and `longarmd run` whose image is in the install dir - asked
   through their stop events, ended by process id (checked: same start
   time, image still in the install dir) after 20 s or when they have none.
   No other program is touched. Job shims (`longarmd shim`, `longarmd
   conhost`), `longarm` clients in terminals and the client daemon keep
   running: the binaries they hold are moved to `<install dir>\old`
   (Windows lets a running program be renamed), and the new ones go in
   beside them. The restarted tray then finds the client daemon older than
   itself and runs `longarm daemon start`, which replaces a daemon older
   than its own `longarm` (its port forwards move to the new one) - never
   a newer one: an older `longarm` (an app's bundled copy, an old CLI)
   just uses the daemon that runs. What ran is recorded in `C:\ProgramData\Longarm\msi-upgrade.json`.
   The new version is installed over the old one, and the old product is
   uninstalled afterwards (`MajorUpgrade Schedule="afterInstallExecute"`):
   the files and components the two share stay (the new ones), and none of
   the old version's uninstall steps (leaving the hub, removing the worker
   task, services or data) runs during an upgrade. (0.1.8 to 0.1.13 removed
   the old product first, after this stop step, which Windows Installer
   refuses: error 2613, "RemoveExistingProducts action sequenced
   incorrectly". Upgrading from those versions works with 0.1.14 and later.)
   An upgrade can't switch the worker between a service and the user-session
   worker (`RUN`, or `ROLES=client` on a service worker): it is refused
   before anything stops - uninstall first (`KEEP_DATA=1` keeps the jobs).
3. **After the install** (`longarmd install-helper msi restart`, at the end,
   once the old product is gone):
   the recorded services start again (the admin worker's is also re-created
   by its setup step), each recorded user's worker task runs
   (`LongarmWorker-<SID>`; the installing user's is restarted anyway), and
   the trays start again in their users' sessions, not elevated (the
   on-demand task `LongarmTray-<SID>`; the installing user's by the usual
   start at the end of the install, unless `LAUNCH_TRAY=0`). Only what was
   running is started. Moved-aside binaries nothing holds any more are
   deleted (the others at the next upgrade).
4. **A failed upgrade** rolls back: the old binaries are moved back and what
   was stopped starts again.

An uninstall stops the trays, worker hosts and `longarmd run` the same way
before removing the files. The tray also answers Windows' end-of-session
messages and Restart Manager (another installer that needs a file it holds,
e.g. a WebView2 update): it quits at once and is registered to be restarted
afterwards (`RegisterApplicationRestart`). An update started from the tray
(its banner runs `msiexec`) needs nothing from the tray: the MSI asks it to
exit and starts it again. The helper's log is
`C:\ProgramData\Longarm\install-helper.log`.

#### Updating: `longarm worker update` and the Longarm Update service

Every MSI install (each role, client-only machines too) has a small
Windows service, **Longarm Update** (`LongarmUpdate`,
`longarm-update.exe`, LocalSystem, delayed auto start). It can do one
thing: install a newer Longarm MSI - downloaded from the hub this machine
recorded at install (`HKLM\Software\Longarm` `Hub`; never a hub named in a
request), signed with the release key built into `longarm-update.exe`
(the minisign trusted comment must name exactly that file and version),
matching the hub's release record (sha256, size), strictly newer than the
installed version, and for this machine's architecture
(`longarm-windows-<arch>.msi`). It runs `msiexec /i <msi> /qn /norestart`
as a one-shot SYSTEM scheduled task (`LongarmUpdate-Install`), so the
upgrade above - which stops and replaces LongarmUpdate too - completes;
the installed product's remembered properties (hub, roles, run mode, admin
worker) carry over. Running jobs and sessions keep running.

From your client, for a worker you own:

```sh
longarm worker update cpc-alice-1                    # the hub's latest stable release
longarm worker update cpc-alice-1 --version 0.1.9 --wait
# cpc-alice-1: checking: updating cpc-alice-1 to 0.1.9
# cpc-alice-1: downloading: downloading longarm-windows-amd64.msi (31 MB)
# cpc-alice-1: verifying: verifying longarm-windows-amd64.msi
# cpc-alice-1: installing: installing Longarm 0.1.9 (from 0.1.8); ...
# cpc-alice-1: 0.1.8 → 0.1.9; 2 jobs and 1 session kept running; its admin worker cpc-alice-1-admin restarted too
longarm worker update cpc-alice-1 --json             # {"worker", "from", "to", "phase", "summary", "kept_jobs", ...}
longarm worker update cpc-alice-1 --auto on [--hours 6]   # automatic updates (MSI installs)
```

- **Who:** the worker's owner only (a client or admin certificate of the
  owner; the admin worker's owner pinned in its `longarmd.toml`). The
  request goes end to end to the worker (`MachineUpdate.Start`), which
  hands it to the service over its local pipe; the hub has no command for
  it and can't relay one - fleet admins update fleet workers with
  rollouts. A worker without an owner (a fleet worker) refuses.
- **Which worker:** the user-session worker (or the admin worker when it
  is alone): the machine updates once and both workers restart. The
  user-session worker (your account) reaches the service like the tray:
  it asks the service manager only for `SERVICE_QUERY_STATUS`, which every
  interactive user has. (Before 0.1.19 it asked for full access, got
  "access denied", concluded there was no service and tried to update
  itself: "C:\Program Files\Longarm is not writable for this account
  (package-managed install?)". `longarm worker update` - the tray's
  button too - then asks the machine's admin worker, which runs as SYSTEM
  and hands the request to the service, so such a worker updates from
  your client once.) A Windows install that can't update itself and has
  no service says what to do (run the new MSI there once: it installs the
  service).
- **Other installs** (`install.ps1` per user, Linux, macOS) take the same
  command and output: longarmd fetches that version through the hub
  (`CheckUpdate` with `want`), verifies the signatures with the key built
  into it and updates itself (Self-update below), only to a newer version.
- **Too old:** a worker without the `machine-update` feature (its
  `Node.Info`) is refused with exit 2 and how to update it once by hand.
- **Exit codes:** 0 updated, up to date or requested (without `--wait`);
  2 refused (a downgrade, a package that isn't for this machine, a worker
  too old); 3 no such worker or version; 75 an update in progress or rate
  limited (retry); 77 not the owner; 255 the update failed.
- **Versions:** `longarm ls` has a VERSION column; `0.1.8↑` and the line
  `↑ 0.1.9 available for ...` mark workers that report a newer release
  they can't install themselves (`NodeInfo.update_available`: MSI
  installs). The tray's Workers tab shows each machine's versions (worker
  and admin worker) and, for the owner, an **Update to X** button with the
  progress and the result - only when the worker says `longarm worker
  update` can install it (`update_available.method`: `msi`; `none` shows
  the reason on the version instead); the panel's My machines page shows
  `longarm worker update <name>` next to an outdated version (the panel
  itself can't start an update: the hub has no way to).

**Automatic updates** are off by default. The machine's owner (the user
the MSI was installed for) or a local administrator turns them on: the
tray's Settings tab ("install new releases automatically"),
`longarm worker update <worker> --auto on` (relayed by your own worker),
or `longarm-update auto on` on the machine. The hub can't: the setting
lives in `HKLM\Software\Longarm\Update`, written by the service only for
those callers. When on, the service checks the hub's newest release of the
machine's **release channel** every 6 hours (`--hours`) and installs it
under the same rules - once no job has started for 5 minutes (the workers
tell it when jobs run), at most a day later.

**Release channel**: stable (the default: releases only), preview
(release candidates too) or dev (a build of every change merged to main).
The tray's Settings tab → Updates → Release channel, for the machine's
owner or a local administrator (stored next to the automatic-updates
setting); the tray's update banner, automatic updates and
`longarm worker update` then mean that channel's newest release, and the
machine's longarmd tells the hub (`longarm ls` marks newer releases of the
channel). Moving back to a more tested channel takes effect with its next
release. docs/ci-cd.md "Release channels".

**On the machine** (any local user may ask; only newer signed releases
install):

```powershell
longarm-update status                      # installed, available, in progress, last result, auto
longarm-update install --latest | --version 0.1.9
longarm-update install --version 0.1.9 --reinstall   # the same version again: an elevated administrator only
```

The tray's own update banner installs through the service when the machine
has it (no UAC prompt; the Settings tab shows the service's status) and
otherwise downloads and checks the MSI itself and runs `msiexec` with a UAC
prompt.
The service's downloads, state and log are in
`C:\ProgramData\Longarm\updates` (SYSTEM only; the installer's verbose log
is `install-<version>.log` there, the service's `longarm-update.log`); an
uninstall removes the service, the setting and that directory.

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

### macOS installer (.pkg) and MDM

Every release has two installers, `longarm-darwin-arm64.pkg` (Apple silicon)
and `longarm-darwin-amd64.pkg` (Intel), on the GitHub Release and on the hub
(`https://hub.example.com/releases/<version>/longarm-darwin-arm64.pkg`). Each
is a flat package that installs `longarm` and `longarmd` into `/usr/local/bin`
(macOS 12 or later). In a release built with the maintainer's Apple
credentials ([ci-cd.md 2.8](ci-cd.md#28-macos-signing-and-notarization-apple-developer-id)):

- the binaries are signed with a **Developer ID Application** certificate,
  with the hardened runtime and a secure timestamp, identifiers
  `com.longarm.longarm` and `com.longarm.longarmd` (also the plain binaries
  `longarm-darwin-<arch>`, `longarmd-darwin-<arch>`, `longarm-hub-…`,
  `longarm-relay-…` and the ones in `longarm-darwin-<arch>.tar.gz`);
- the package is signed with a **Developer ID Installer** certificate,
  **notarized** by Apple, and the ticket is **stapled** to it (Gatekeeper
  accepts it offline);
- `SHA256SUMS`, `checksums.txt` and the `.minisig` signatures are made from
  these final, signed files.

A release built without them ships the same files unsigned (its build log
says so); MDM-managed Macs refuse unsigned packages.

**Install by hand.** Double-click the pkg, or:

```sh
pkgutil --check-signature longarm-darwin-arm64.pkg   # "Developer ID Installer: <maintainer> (<TEAMID>)", "Notarized"
spctl --assess --type install -vv longarm-darwin-arm64.pkg   # accepted, source=Notarized Developer ID
sudo installer -pkg longarm-darwin-arm64.pkg -target /
longarmd login hub.example.com --role both            # then, for a worker:
sudo longarmd service install --user "$USER" && sudo longarmd service start
```

What the package's `postinstall` does (as root): adds `longarm` and
`longarmd` to the application firewall when it is on; on an upgrade,
restarts an installed `com.longarm.longarmd` LaunchDaemon so it runs the new
binary (jobs keep running); and, only if the preference domain `com.longarm`
holds `Roles` (`worker` / `both` / `client`), `Hub`, `JoinCode` and `User`
(optional `CAFingerprint`), joins the hub as that user and, for a worker,
installs and starts the LaunchDaemon with the data dir `~User/.longarm`.
The values are validated (a malformed or non-root-owned preferences file is
ignored); the join code is removed from `/Library/Preferences/com.longarm`
afterwards.

**For MDM administrators** (Jamf, Intune, Kandji, Mosyle, …):

- **Package**: upload the pkg for the Macs' architecture (both, scoped by
  architecture; the arm64 package refuses Intel Macs, the amd64 package needs
  Rosetta on Apple silicon). It is a signed distribution package, as Intune's
  "macOS app (PKG)" and Jamf policies require. Package ID `com.longarm.pkg`,
  version = the release version; installs `/usr/local/bin/longarm`,
  `/usr/local/bin/longarmd` and `/usr/local/share/longarm/`.
- **Team ID**: the 10-character Apple Team ID of the signing certificates
  (`pkgutil --check-signature` shows it in parentheses; `codesign -dv
  /usr/local/bin/longarmd` as `TeamIdentifier=`). The maintainer publishes it
  next to the release; it identifies Longarm in the profiles below.
- **Unattended join**: a custom settings / Application & Custom Settings
  payload for the domain `com.longarm` with `Roles`, `Hub`, `JoinCode`,
  `User`, `CAFingerprint` (lands in `/Library/Managed Preferences/com.longarm.plist`,
  which `postinstall` reads first). Use a join code that expires (`longarm
  worker add --ttl 24h`): the profile keeps it until it is removed. Or
  install without it and let users run `longarmd login`.
- **Background items** (macOS 13+): `longarmd service install` adds the
  LaunchDaemon `/Library/LaunchDaemons/com.longarm.longarmd.plist`
  (program `/usr/local/bin/longarmd`), which macOS reports as a background
  item. To keep users from turning it off, a **Service Management - Managed
  Login Items** payload (`com.apple.servicemanagement`) with the rule
  `RuleType` = `TeamIdentifier`, `RuleValue` = the Team ID (or `Label`
  `com.longarm.longarmd`).
- **Privacy (PPPC)**: Longarm needs no system extension, kernel extension
  or entitlement. Only if jobs must read TCC-protected places (Desktop,
  Documents, removable volumes, another app's data), give `longarmd` access
  with a **Privacy Preferences Policy Control** payload: `IdentifierType`
  `path`, `Identifier` `/usr/local/bin/longarmd`, `CodeRequirement` as
  `codesign -dr - /usr/local/bin/longarmd` prints it (`identifier
  "com.longarm.longarmd" and anchor apple generic and certificate
  1[field.1.2.840.113635.100.6.2.6] and certificate
  leaf[field.1.2.840.113635.100.6.1.13] and certificate leaf[subject.OU] =
  <TEAMID>`), and the service, e.g.
  `SystemPolicyAllFiles` (Full Disk Access). The code requirement pins the
  Team ID, so a re-signed or replaced binary doesn't inherit the access.
- **Firewall**: with a managed application firewall, allow
  `/usr/local/bin/longarmd` and `/usr/local/bin/longarm` (or the identifiers
  above) incoming connections; `postinstall` only adds them when the user's
  firewall is on.
- **Updates**: a worker whose LaunchDaemon runs as a user (`--user`, the
  pkg's unattended setup) can't write `/usr/local/bin` and doesn't update
  itself: deploy the next release's pkg (`longarmd status` and the hub
  panel show outdated workers). A root LaunchDaemon (no `--user`) updates
  itself from the hub; the files it installs are the same signed binaries,
  checked against the release key first ([Self-update](#self-update)), so the
  code signature stays valid.

### Uninstall

No uninstaller removes the **client** enrollment (`~/.config/longarm`,
`%USERPROFILE%\.config\longarm` on Windows): run `longarm leave` first (the
hub revokes the client's certificate, its credentials are removed), then
delete the directory if you want `workers.toml` and the caches gone too.
Workers are handled below. A worker that left while the hub was unreachable
(`--force`) is still listed there: `longarm worker remove <name>` (yours) or
`longarm admin revoke <name>`.

**MSI** (also what `install.ps1` installs when elevated). Settings > Apps >
Installed apps > **Longarm** > Uninstall, or:

```powershell
msiexec /x longarm-windows-amd64.msi /qn [KEEP_DATA=1]     # the installed version's MSI file
# without the file: by product code (the product is "Longarm", UpgradeCode {0d623137-abc8-4dc1-8e7d-32fc483c9e93})
$pc = (Get-ItemProperty HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\* | Where-Object DisplayName -eq 'Longarm').PSChildName
msiexec /x $pc /qn
```

No `longarmd leave` needed: the uninstall runs it (forced, `--kill-jobs`)
for every worker it installed - the user-session worker (the installing
user's `~\.longarm`), the service (`DATADIR`, default
`C:\ProgramData\Longarm\longarmd`) and the admin worker
(`C:\ProgramData\Longarm\longarmd-admin`) - so their certificates are
revoked, running jobs end and the data dirs go (`KEEP_DATA=1` keeps jobs and
mirrors; the enrollment goes either way). It also removes the scheduled task
`LongarmWorker-<SID>`, the services `longarmd` / `longarmd-admin`, the
firewall rules, the PATH entry, the tray's autostart, the settings in
`HKLM\Software\Longarm` and `C:\Program Files\Longarm`.

**`install.ps1` without the MSI** (not elevated, `-NoMsi`, `-Prefix`, or a
release without an MSI). What it installed: `longarm.exe`, `longarmd.exe`
(and `longarm-tray.exe`, the logon task's host) in `-Prefix` - default
`%LOCALAPPDATA%\Programs\Longarm`, elevated `C:\Program Files\Longarm`; the
prefix on the user's `Path` (machine `Path` when elevated; not with
`-NoPath`); elevated, a firewall rule for `longarm.exe` (`longarm firewall
allow`); for a worker the scheduled task `LongarmWorker-<SID>` (`-Run
session`, the default) or the service `longarmd` (`-Run service`); the data
dir `~\.longarm` (or `-DataDir`). Remove in this order:

```powershell
longarmd session-mode uninstall            # the logon task; stops the worker (else the host restarts it)
#   -Run service, elevated: longarmd service uninstall
longarmd leave --kill-jobs --purge         # hub revokes the worker; --purge deletes the data dir (-DataDir: --data-dir D)
longarm leave                              # the client
longarm firewall remove                    # elevated, if the rule was added
Remove-Item -Recurse "$env:LOCALAPPDATA\Programs\Longarm", "$HOME\.config\longarm"
# and the prefix from Path: System > About > Advanced system settings > Environment Variables (user or system)
```

**Linux / macOS.**

- **deb / rpm** (also `install.sh` as root on Linux when the release has a
  package): `sudo apt remove longarm` / `sudo dnf remove longarm`. Its prerm
  stops and disables every `longarmd@<user>` unit and runs `longarmd leave
  --kill-jobs` as that user; the data dirs (`~<user>/.longarm`) stay - delete
  them by hand. `longarmd-peruser` (per-user isolation) is not left by the
  prerm: `sudo systemctl disable --now longarmd-peruser; sudo longarmd leave
  --kill-jobs --purge` (data dir `/var/lib/longarmd`) first.
- **`install.sh` binaries** (`--no-package`, `--prefix`, not root, macOS):
  `longarm` and `longarmd` in `/usr/local/bin` (root) or `~/.local/bin`
  (`--prefix`); a worker gets the service `longarmd` (`longarmd service
  install --user <sudo user>`): systemd `/etc/systemd/system/longarmd.service`,
  launchd `/Library/LaunchDaemons/com.longarm.longarmd.plist`; data dir
  `~/.longarm` of that user (`--data-dir`). Remove:

  ```sh
  sudo longarmd service uninstall           # stop + remove the unit / LaunchDaemon
  longarmd leave --kill-jobs --purge        # as the service's user: revoke, delete ~/.longarm
  longarm leave && rm -rf ~/.config/longarm
  sudo rm /usr/local/bin/longarm /usr/local/bin/longarmd   # or ~/.local/bin/...
  ```
- **macOS pkg**: the same, then `sudo rm -rf /usr/local/share/longarm; sudo
  pkgutil --forget com.longarm.pkg` ([packaging/macos](../packaging/macos/README.md)).

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
longarm ls --direct -o wide  # PATH column: winning leg + the race, e.g. "longnet/relay 3ms [tcp: cancelled, longnet 41ms won]"
longarm ls -o wide           # via the hub: "hub; last longnet/relay ..." (the cached winner)
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
- **Headscale next to the hub** (Azure Container Instances: `deploy.sh
  --headscale`, [deploy/azure](../deploy/azure/README.md#headscale-a-self-hosted-tailnet)):
  `[tailnet.headscale] sidecar_dir` (env `LONGARM_HUB_HEADSCALE_DIR`) names a
  directory the hub shares with a Headscale container. The hub then runs it
  without anybody touching Headscale: it writes Headscale's config, ACL
  policy, DERP map and private keys there (the keys live in the hub's
  database), creates its own API key over Headscale's unix socket in that
  directory (renewed before it expires, replaced when Headscale no longer
  knows it), and serves Headscale's node endpoints on its own https listener
  (`proxy = true`): `/ts2021`, `/key`, `/derp` (+ `/derp/probe`,
  `/derp/latency-check`), `/bootstrap-dns` and `/machine/ping-response`, forwarded to
  `api_url` (default `http://127.0.0.1:8080`). Headscale's REST/gRPC API,
  metrics and interactive pages stay local. `url` defaults to `public_url`.
  The environment of a container: `LONGARM_HUB_TAILNET_PROVIDER`,
  `LONGARM_HUB_HEADSCALE_URL`, `LONGARM_HUB_HEADSCALE_API_URL`,
  `LONGARM_HUB_HEADSCALE_API_KEY_FILE`, `LONGARM_HUB_HEADSCALE_DIR`,
  `LONGARM_HUB_HEADSCALE_PROXY`, `LONGARM_HUB_HEADSCALE_PUBLIC_DERP` (Tailscale's
  public DERP map too; default off). `longarm admin tailnet status` shows
  `health ok | starting: ... | down: ...`, the hub's `/healthz` has
  `"headscale": "ok"`. While the sidecar starts, `TailnetKey` answers
  Unavailable without charging the node's key budget; nodes retry.
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
running throughout. longarmd.toml: `[update] disabled, channel (stable |
preview | dev; default: the machine's channel from the tray, else stable),
interval (1h), health_window, start_grace`. Package-managed installs (deb/rpm/brew) update
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
relayed, and a warning in the tray popup. The fix, in an elevated PowerShell:
`longarm firewall allow` (removes the program's Block rules, adds the allow
rules; `--program <path>` for longarmd.exe). `longarm firewall status` exits 1 when
blocked or missing. The commands longarm prints paste into PowerShell: a quoted
path gets the call operator (`& 'C:\Program Files\Longarm\longarm.exe' firewall allow`).
The tray's warning has an **Allow** button: one administrator prompt runs the
installed longarm.exe's `firewall allow --program <the program the daemon or
longarmd reports>` (only an existing longarm.exe / longarmd.exe), then checks
again - also for a copy another app ships, which the card points out
([troubleshooting](troubleshooting.md#windows-firewall--connections-stay-on-relays)). macOS: the pkg adds
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
- `release.yml` (tag `v*`): build on Linux (linux, macOS, deb/rpm) and on
  Windows (Windows, MSIs) in parallel, the macOS files signed, packaged
  (`.pkg`) and notarized on a Mac after the Linux build, the hub image next
  to them →
  **publish** (sign with the release key, upload with the `release`
  certificate, GitHub Release) → **hub** (SSH forced command
  [`deploy/hub/longarm-hub-deploy`](../deploy/hub/longarm-hub-deploy): verify, install,
  health check, automatic rollback) → **rollout** (`--canary 1 --wait 15m`,
  fails when it halts). Without a configured hub only the GitHub Release is
  made. `v1.2.3-rc.1` → channel preview, a push to `main` → a dev build
  (channel dev); neither deploys the hub or rolls out (docs/ci-cd.md
  "Release channels").
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
for setups without a hub, and as an **external relay** of a hub (next
section). The longnet listener follows certificate renewals.

### External relays: relayed connections that survive hub restarts

The relay inside longarm-hub restarts with the hub, so every relayed
connection (a client and a worker without a direct path) drops while the hub
redeploys - a minute or two on Azure Container Instances. An external relay
is a `longarm-relay` started with `--hub`, deployed apart from the hub (on
Azure: `deploy.sh --relay`, its own container group, see
[deploy/azure/README.md](../deploy/azure/README.md#external-relay)):

```sh
# a relay certificate from the hub's CA (on the hub: longarm-hub issue; or longarm admin issue with the CA)
longarm-hub issue --role relay --name relay1 --host relay.example.com --out ./relay-tls
longarm-relay --listen :7445 --tcp :7443 --tls-dir ./relay-tls --metrics 127.0.0.1:7801 \
  --hub hub.example.com:7443 --advertise relay.example.com [--state-dir DIR]
longarm admin relays          # the relay set, connected relays, workers on old versions
```

- **Relay set.** The relay registers its public endpoints with the hub
  (`--advertise HOST` with the bound ports, or `--advertise-udp` /
  `--advertise-tcp HOST:PORT`). The hub keeps them in its database and
  advertises the set - external relays first, its embedded relay last - in
  every worker session (and pushes changes to running ones). Loopback-only
  workers (every sign-in install) listen on every relay of the set and
  advertise one longnet endpoint per relay (an external relay's carries its
  TLS fallback: `longnet://relay:7445?name=w1&tcp=relay:7443`); clients race
  them like any other legs and keep them in `hub-cache.json`. Workers keep
  the last set in `relay-set.json` and listen on it when they start while the
  hub is down. A relay not seen for 7 days leaves the set;
  `longarm admin revoke <relay>` removes it at once.
- **Authorization without the hub.** Nodes authenticate to the relay with
  their CA certificates, as with the embedded relay (the relay's certificate
  is from the same CA: nothing new to trust). The hub sends the relay its
  revocation list and the worker pairs it may relay between (the active port
  links; client <-> worker needs none) over a subscription
  (`HubNode.RelaySync`) at once, on every change and every minute. The relay
  enforces them on its own and keeps them on disk (`--state-dir`): it never
  asks the hub per session, keeps relaying while the hub is down, and a
  relay that restarts during a hub outage starts with the cached list. A
  relay that has never had a list refuses sessions until the hub answers.
  Worker pairs end 24 hours after the relay last heard from the hub; the
  revocation list never expires (it only restricts). A revocation reaches
  the relay within a second and closes the node's relay sessions.
- **Certificate.** Valid 90 days (`worker_ttl`) when issued by the hub's
  renewal, 365 days from `deploy.sh --relay`; the relay renews it through the
  hub (`HubNode.Renew`, keeping its host names) when it expires within the
  hub's `renew_before` (30 days by default), and writes it to `--tls-dir`.
- **STUN** is answered on the relay's UDP port and `--stun2` as before;
  `--stun-advertise` (or `LONGARM_RELAY_STUN_ADVERTISE`) lists external
  servers for a relay behind source NAT (ACI).
- **Health:** `GET /healthz` on `--metrics` (503 until the relay has a
  policy); `longarm-relay healthcheck [--ready]`.

**Compatibility.** Nothing needs signing in again or reinstalling: workers
of this release pick up the relay set in their next hub session (connected
ones within seconds), older workers ignore the new fields and keep using the
embedded relay, which stays as the fallback. Surviving hub restarts needs the
new version on both ends: the worker to listen on the external relay, the
client to dial its endpoint with its TLS fallback (an older client still uses
the relay's UDP endpoint, and the embedded relay). MSI installs update
through the Longarm Update service (the tray's banner, `longarm worker
update`), script installs through self-update; `longarm admin relays` counts
the online workers that use the set and those still on older versions.

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

`longarm ls -o wide` shows the live path, e.g. `longnet/direct 80ms (203.0.113.78:27225, nat easy/hard)` or
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
