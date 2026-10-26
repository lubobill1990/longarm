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
longarm wait w1 <id>                               # exit code = job's (killed 137, lost 254, longarm error 255)
longarm kill w1 <id>                               # kills the whole process tree
longarm jobs w1 [--state running]
longarm admin shutdown w1                          # admin role only; jobs keep running
```

Secrets (`--secret-env NAME` or `NAME=LOCALVAR`) are read from longarm's own
environment, sent over mTLS, kept only in the job's memory/env, and redacted
from the job log.

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
input is sent with newlines as Enter (CR).

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
longarm run w1 --repo https://github.com/o/r --ref main --secret-env GH_TOKEN -- pnpm test
```

The worker keeps one bare mirror per repository (`<data>/mirrors/<repo>.git`)
and a worktree per sync target or job (`<data>/worktrees/<job-id>`); a job
with `--repo` stays `preparing` until its worktree is ready. Syncing into an
existing worktree resets it: tracked changes and untracked files there are
discarded, ignored files (node_modules, build output) are kept.

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
and `-L`/`-D` forwarding (VS Code Remote-SSH). Optionally longarmd also listens
directly, accepting the public keys in `<data>/authorized_keys`:

```toml
# <data>/longarmd.toml
listen = ["tcp://100.64.0.11:7701"]
max_log_bytes = 1073741824      # out.log cap per job/session; rotates to out.log.1 (0 = off)

[ssh]
# disabled = true
listen = ["tcp://100.64.0.11:2222"]   # optional direct sshd (authorized_keys)
# shell = ["pwsh", "-NoProfile", "-Command"]   # exec shell (default cmd.exe /c | $SHELL -c)

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
longarm login hub.example.com:7443 --ca-fingerprint <fp>      # or: sign in with Microsoft Entra ID (see below)
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
longarm jobs [w1] [--state running]      # fleet-wide job index
longarm run -- pnpm test                 # placed by the scheduler (= --on auto)
longarm run --type build --mem-gb 8 --require os=linux --repo . -- make   # reservation, labels, repo cache affinity
longarm run --no-queue -- ...            # fail instead of queueing when no worker passes the gate
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

### Your own workers

Any user can register their **own** machines as workers and use them from
their client devices - by name (`longarm run --on`/`longarm exec <worker>`) or by
automatic placement, which picks the least loaded worker the user may use.
A worker a user registers is usable **by that user only**: other users don't
see it (`longarm ls`, the panel, events, metrics history), can't call it, and
the scheduler never places their jobs on it. Workers registered by admins
(join codes without owner) stay the shared fleet, as before.

```sh
longarm worker add [<name>] [--ttl 24h] [--label k=v]...   # prints a join code (stdout) and the command:
#   longarmd join <hub> <code> --ca-fingerprint <fp> --name <name>   # on the new worker, then
#   longarmd service install ...                                     # (see "Install a worker")
#   the worker joins approved (no admin needed) and owned by you; at most 10 unused codes per user
longarm worker ls [--json]                # your workers, workers shared with you, the fleet (owner, shares, load)
longarm worker share <worker> [--user bob]... [--team web]... [--everyone]
longarm worker unshare <worker> [--user bob]... [--team web]... [--everyone]
longarm worker remove <worker>            # revoke it (owner or admin)
longarm ls                                # an OWNER column appears once a worker has an owner
                                          # ("-" = fleet, "+shared" = shared by its owner)
```

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

### Sign in with Microsoft Entra ID

Instead of join codes and panel passwords, people can sign in with their
Microsoft Entra ID (Azure AD) account: `longarm login` uses the token of
their Azure CLI sign-in (`az login`), the web panel has a "Sign in with
Microsoft" link. The hub verifies Microsoft's token and maps the Entra user
(its object ID) to a Longarm user, which it **creates on first sign-in**
(named after the UPN's local part: `alice@example.com` -> `alice`; taken
names get a digit, `alice2`; the UPN is kept, see `longarm admin user ls`).
Like `longarm admin user add`, the first user turns multi-tenancy on.

```sh
longarm login hub.example.com:7443 --ca-fingerprint <fp>   # client cert of your user (admin cert for
                                                           # the hub's Entra admins), hub.toml, like join
#   device name <user>-<hostname> (--name to choose); signing in again on the same
#   machine (expired certificate) replaces the device's certificate, no --force needed
#   if az has no token for the hub: it prints the exact command, e.g.
#     az login --tenant <tenant> --scope api://<client id>/longarm.connect
longarm login hub.example.com:7443 --scope api://<client id>/.default   # another scope (rarely needed)
```

`longarm login` finds `az` on PATH (Windows: also under `Program Files\Microsoft
SDKs\Azure\CLI2\wbin`), or at `$LONGARM_AZ`. The token goes only to the hub,
over the TLS connection that `--ca-fingerprint` pins (without it: trust on
first use, with a warning, as `longarm join`); it is never printed or stored.

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
`fleet_forward` (via longarm daemon, returns the local URL). Secrets are passed by
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

ROLES = `client` (longarm), `worker` (+ the longarmd service, auto-start, restart on
failure; data dir `C:\ProgramData\Longarm\longarmd`, readable by
SYSTEM/Administrators/the service account only) or `both` (JOIN_CODE = the worker's, CLIENT_JOIN_CODE = the
client's). The MSI adds inbound UDP+TCP firewall rules for longarm, longarmd and
longarm-tray, puts them on PATH, starts the tray at logon, and on uninstall stops
the service, makes the worker leave its hub (`longarmd leave`) and removes
everything (`KEEP_DATA=1` keeps jobs and mirrors). The hub's
`install.ps1` / `install.sh` use the MSI / deb / rpm when the release has them
(elevated / root), else they install the bare binaries as before
(`-NoMsi`, `--no-package`).

**The worker service runs as a user account** (DESIGN §3.1: jobs then find
that user's git / npm / claude settings), never silently as SYSTEM. The
interactive MSI asks for the account (default: you) and its password in the
worker step and grants it "Log on as a service". A silent worker install
needs `SERVICE_USER` + `SERVICE_PASSWORD`; `SERVICE_ACCOUNT=LocalSystem`
instead runs it as SYSTEM (warned about in the install log and in longarmd.log).
A wrong password fails the install (the service can't start) and rolls it
back. Upgrading with a newer MSI keeps the account but needs
`SERVICE_PASSWORD` again. `install.ps1` does the same: `-ServiceUser`
(default you) with `$env:LONGARM_SERVICE_PASSWORD` or a prompt, or
`-ServiceAccount LocalSystem` / `$env:LONGARM_SERVICE_ACCOUNT`.

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
