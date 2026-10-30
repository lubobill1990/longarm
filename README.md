# Longarm

**Give your coding agents a long arm.** Longarm turns the spare machines
around you (a workstation under the desk, an old laptop, a cloud VM) into one
pool of workers that Claude Code, Copilot CLI or you can run commands, builds,
test suites and whole agent sessions on - from any laptop, through any NAT.

[中文说明](README.zh-CN.md) · [Usage reference](docs/usage.md) ·
[Design](docs/design.md) · [CI/CD](docs/ci-cd.md) · [Security](docs/security.md) ·
[Telemetry](docs/telemetry.md)

## Why

Coding agents are great at starting work and terrible at waiting for it. A
single `pnpm test` or a Rust build pins your laptop's CPU, a second agent
session doubles that, and three parallel sub-agents make the fans scream and
the battery melt. Meanwhile there is a 32-core box at home and a VM in the
cloud doing nothing.

Longarm lets the agent (or you) say *"run this over there"*:

- **Offload heavy jobs**: builds, test suites, `claude -p` / `copilot -p`
  sessions run on a worker; your laptop stays cool and quiet.
- **Fan out**: parallel sub-agents each get their own worker or worktree,
  placed by a scheduler that knows which machine has free memory.
- **Survive everything**: jobs run detached on the worker. Close the lid,
  change networks, restart the worker daemon - the job keeps running and you
  re-attach to its output.
- **Reach anything**: workers behind home routers and carrier-grade NAT are
  reached directly through hole punching, or through a relay when that fails.

## Features

- **Workers as services** - `longarmd` runs as a Windows service, a systemd
  unit or a launchd daemon under a normal user account (so jobs find that
  user's git, node and agent settings).
- **Jobs that survive restarts** - every job and session lives in its own
  small shim process; the daemon can crash or update without killing them.
  Logs are streamed from any byte offset; exit codes are mirrored.
- **Interactive PTY sessions** - tmux-like `longarm attach`: named sessions,
  several clients at once, detach with `Ctrl-] d`, re-attach with replay.
  Programs (an IDE, a desktop app) drive the same sessions without a
  terminal: `longarm session start|send|stop`.
- **Port forwarding** - `longarm forward worker-1 3000` puts the worker's dev
  server on your `localhost`.
- **Port links between workers** - `longarm link create backend:8080 --on
  frontend` lets your jobs on one worker reach another worker's port as
  `127.0.0.1:8080`, worker to worker (frontend + tests here, backend there),
  authorized by grants the hub signs per link.
- **Code sync** - `longarm sync .` mirrors your checkout (commits, uncommitted
  changes, untracked files) into a git worktree on the worker; `longarm run
  --repo .` gives a job a fresh worktree (`--ref` a branch, tag or full
  commit id; Git LFS files as pointers, or their content with `--lfs ship`).
- **Built-in sshd** - `ssh`, `scp`, `sftp` and VS Code Remote-SSH work through
  the same mTLS channel, no SSH keys to manage.
- **MCP server** - `longarm mcp` gives Claude Code and Copilot CLI tools such as
  `fleet_status`, `fleet_exec`, `fleet_job_submit`, `fleet_job_wait`,
  `fleet_sync`, `fleet_forward` and `fleet_link`. Secrets are passed by name, never through
  the model.
- **Scheduler with a memory gate** - `longarm run -- make` picks the least loaded
  worker that has enough free memory and CPU headroom, reserves memory per job
  type, prefers workers that already have your repository, and queues when
  nobody fits.
- **Hub** - `longarm-hub` is the control plane: one-time join codes, a built-in
  CA for mTLS certificates (auto-renewed, revocable), node registry, fleet-wide
  job index, audit log, toolchain profiles, install scripts. It is never on the
  data plane: when it's down, workers keep working and clients talk to them
  directly.
- **Three transports, raced** - every connection dials all legs at once and the
  first mTLS handshake wins:
  - plain **TCP** (LAN, VPN, your own tailnet),
  - **Tailscale** via an embedded `tsnet` node (Tailscale SaaS or a self-hosted
    Headscale; the hub mints tagged one-time keys, nobody installs `tailscaled`),
  - **longnet**, Longarm's own QUIC transport with NAT type detection, hole
    punching and a relay fallback (UDP or TLS over TCP) built into the hub.
- **Tray app and web panel** - `longarm-tray` shows live CPU/memory per worker,
  your link to each (direct or relayed, RTT), your jobs and forwards; the hub's
  web panel (login with TOTP) manages nodes, jobs, join codes and users.
- **Signed self-updates** - releases are minisign-signed; workers update canary
  first, health-check the new version and roll back by themselves; MSI
  installs and clients get an update notice instead.
- **Packages** - MSI (Windows), deb/rpm (Linux), pkg and Homebrew cask (macOS),
  a Docker image and deb for the hub, one-line install scripts served by the hub.

| Hub web panel | Tray popup |
|---|---|
| ![Nodes page of the hub panel](docs/ui-screenshots/panel-nodes.png) | ![Workers tab of longarm-tray](docs/ui-screenshots/tray-workers.png) |

## Architecture

```
  laptop (client)                       hub (small cloud VM)
 ┌───────────────────────┐            ┌──────────────────────────────┐
 │ Claude Code / Copilot │            │ longarm-hub                  │
 │   └─ longarm mcp      │  gRPC/mTLS │  join codes · CA · registry  │
 │ longarm CLI           │◄──────────►│  scheduler · job index       │
 │ longarm daemon (pool, │  (control) │  web panel · release store   │
 │   forwards, latency)  │            │  longnet relay + STUN        │
 │ longarm-tray          │            └──────────────▲───────────────┘
 └──────────┬────────────┘                           │ heartbeats, metrics,
            │                                        │ job reports (control)
            │  data plane: exec, logs, PTY, files,   │
            │  forwards - raced over three legs:     │
            │    tcp · tailscale (tsnet) · longnet   │
            │    (direct after hole punching,        │
            │     else via the relay)                │
            ▼                                        │
 ┌─────────────────────────────────────────────────────────────────┐
 │ workers: longarmd (service)  worker-1 (Windows)  worker-2 (Linux)│
 │   shim per job/session · git mirrors + worktrees · built-in sshd │
 └─────────────────────────────────────────────────────────────────┘
```

The hub only coordinates. Commands, output, files and terminals flow directly
between client and worker (or through the relay, end-to-end encrypted with
the same mTLS certificates).

## Quick start

### Join a hub

Somebody already runs a hub (your team's, or the one built into the
release)? Install, sign in with Microsoft, done - no hub address, no join
code, no fingerprint, no password.

**1. Install** on each machine you want to use.

**Windows: download the MSI, click Next twice, approve in the browser.**
Get `longarm-windows-amd64.msi` from the hub's "Get Longarm" page
(`https://hub.example.com/get`), straight from its release store
(`https://hub.example.com/releases/<version>/longarm-windows-amd64.msi`) or
from the [releases page](https://github.com/lubobill1990/longarm/releases).
Run it, click Next, keep **Default (recommended)** - pick "This is my
everyday computer: send jobs to workers" or "This machine runs jobs (Dev
Box, build machine)" (preselected on a Dev Box / Cloud PC) - and click Next,
Install. The Longarm tray then starts the sign-in by itself and shows a
code: approve it in your browser (step 2). Roles can be changed later in the
tray's Account tab. The
worker runs as you while you are signed in (also with the remote desktop
disconnected - a Dev Box keeps the session), so it works on machines you
sign in to with Windows Hello or a security key only; "Allow remote
administrator commands" adds an [admin worker](docs/usage.md#admin-worker-windows)
for tasks that need SYSTEM. "Custom" takes another hub, a join code, or the
worker as a Windows service. Silently: `msiexec /i longarm-windows-amd64.msi
/qn ROLES=both` (properties: the comment at the top of
[packaging/windows/longarm.wxs](packaging/windows/longarm.wxs)). The
"Default" hub is built into the release (repository variable
`LONGARM_DEFAULT_HUB`, [docs/ci-cd.md](docs/ci-cd.md#24-secrets-and-variables)).

**SmartScreen.** A release whose MSI and exes are not Authenticode-signed
(releases are signed with Azure Trusted Signing only once a validated
certificate profile is set up, [docs/ci-cd.md](docs/ci-cd.md#27-authenticode-azure-trusted-signing-windows))
makes Windows show "Windows protected your PC" / "Unknown publisher": choose
**More info → Run anyway**, after checking the file against the release's
`SHA256SUMS` (`Get-FileHash longarm-windows-amd64.msi`). A signed release
shows the publisher; SmartScreen may still warn for a new release until it
has built up download reputation. Updates (tray, Longarm Update service)
always verify the release key's minisign signature, signed or not.

Or one line - the hub serves the scripts with its address and CA pinned:

```sh
curl -fsSL https://hub.example.com/install.sh | sudo sh -s -- --login --role both          # Linux / macOS
$env:LONGARM_LOGIN=1; $env:LONGARM_ROLE='both'; irm https://hub.example.com/install.ps1 | iex   # Windows (elevated: the MSI)
msiexec /i longarm-windows-amd64.msi /qn MODE=custom ROLES=both HUB=hub.example.com LOGIN=1   # MSI, another hub
```

Already installed? `longarmd login hub.example.com --role both` - the hub's
domain is enough: its CA is fetched from `https://hub.example.com/ca.crt`
over HTTPS and pinned ([docs/security.md](docs/security.md#first-contact-domain-only-sign-in)).

**2. Sign in with Microsoft - one code.** The install prints:

```
To link this machine as your device and your worker (one approval for both), sign in with your Microsoft account ...
  Open https://hub.example.com/link and enter code BCDF-GHJK
```

Open the page in any browser (your phone will do), sign in with Microsoft,
type the code, check the machine's name and approve - once, for both roles.
(The MSI runs no console login: the tray it opens - "Sign in with
Microsoft" - or `longarmd login --role both` shows the code afterwards and
needs no other input; after the approval the tray shows "Signed in as ...",
the worker's state, Sign out and Open panel.)

**3. The machine is your worker and your client.** It runs your jobs (only
yours until you share it: `longarm worker share`) and its `longarm` acts as
you; the hub's Entra admins get admin certificates. `longarm ls` lists it.
Label it for placement at sign-in (`--login --label pool=devbox`,
`longarmd login --label pool=devbox`, MSI `LABELS=pool=devbox`) or later -
`longarm worker label <worker> pool=devbox` - and `longarm run --require
pool=devbox` goes there.

longarmd's data dir (certificate, hub address, jobs) is `~/.longarm` of the
account it runs as - with the MSI's default user-session worker yours,
`~\.longarm`. A service has its account's (`sudo longarmd service install
--user alice`: alice's); as root on Linux the default is `/var/lib/longarmd`,
as LocalSystem on Windows `C:\ProgramData\Longarm\longarmd` (also the MSI's
service's); `--data-dir` / `LONGARM_DATA_DIR` override it
([docs/usage.md](docs/usage.md#data-directory)). Worker offline, sign-in
expired, update notices: [docs/troubleshooting.md](docs/troubleshooting.md#for-users-common-problems);
removing Longarm: [Uninstall](docs/usage.md#uninstall).

### Run your own hub

You need a hub reachable from everywhere (a small VM with a public IP: ports
7443/tcp+udp and 7444/udp). Binaries for all platforms are on the
[releases page](https://github.com/lubobill1990/longarm/releases); or build
from source with Go ≥ 1.27.3: `go build -o bin/ ./cmd/...`.

On Azure in one command (Container Instances, about 44 USD/month; details:
[deploy/azure](deploy/azure/README.md)), which prints the join command:

```sh
az login && cd deploy/azure && ./deploy.sh --name myhub --location eastasia   # or deploy.ps1
# --build-in-acr: the image built from your checkout in Azure (no docker); or the button:
```

[![Deploy to Azure](https://aka.ms/deploytoazurebutton)](https://portal.azure.com/#create/Microsoft.Template/uri/https%3A%2F%2Fraw.githubusercontent.com%2Flubobill1990%2Flongarm%2Fmain%2Fdeploy%2Fazure%2Fazuredeploy.json)

Or on any host with a public IP (details: [deploy/hub](deploy/hub/README.md)):

```sh
longarm-hub --data-dir /var/lib/longarm-hub ca init        # the fleet's CA
longarm-hub --data-dir /var/lib/longarm-hub serve &        # :7443 gRPC + relay, 127.0.0.1:7480 panel
longarm-hub --data-dir /var/lib/longarm-hub ca show        # CA fingerprint
longarm-hub --data-dir /var/lib/longarm-hub join-code --role admin
```

Then join it as above (`longarmd login hub.example.com --role both`, or the
one-liners). Sign-in with accounts needs them configured on the hub:
`[auth.entra]` for Microsoft work or school accounts
([docs/usage.md](docs/usage.md#sign-in-with-microsoft-entra-id)), `[auth.google]`,
`[auth.apple]`, `[auth.microsoft]` for personal accounts
([docs/usage.md](docs/usage.md#sign-in-with-a-google-apple-or-personal-microsoft-account));
without any, a panel login approves the code. Every machine one account
links is that account's: its devices and its workers. Or use join codes:
`longarm join hub.example.com:7443 <admin-code> --ca-fingerprint <fp>` on your
laptop, `longarm admin join-code --role worker` for each worker, there
`longarmd join hub.example.com:7443 <code> --ca-fingerprint <fp> --name worker-1`,
`longarm admin approve worker-1` and `sudo longarmd service install --user alice`
(Windows, elevated: `--user .\alice`) + `longarmd service start` - or the
one-liners with `--join <code>` instead of `--login`. Without any hub:
[a CA and `admin issue`](docs/usage.md#advanced-self-hosted-without-sign-in-ca-and-admin-issue).

### Use it

```sh
longarm ls                                     # workers, load, agent sessions
longarm exec worker-1 -- git --version         # short command, exit code mirrored
longarm run --repo . -f -- pnpm test           # scheduled job in a fresh worktree, follow its output
longarm attach worker-1 -s agent -- claude     # an interactive agent session you can detach from
longarm forward worker-1 3000                  # worker's dev server on localhost:3000
longarm link create worker-2:8080 --on worker-1   # worker-2's :8080 as 127.0.0.1:8080 for your jobs on worker-1
```

Hand it to your agent:

```sh
claude mcp add longarm -- longarm mcp
# Copilot CLI: ~/.copilot/mcp-config.json
#   {"mcpServers": {"longarm": {"type": "local", "command": "longarm", "args": ["mcp"], "tools": ["*"]}}}
```

Then ask: *"Run the full test suite on the fleet and tell me what fails"* or
*"Start three sub-agents on separate workers, one per failing package"*.

Everything else - workers without a hub, Tailscale/Headscale, the relay, the
tray app, self-update, packages, firewalls - is in the
[usage reference](docs/usage.md). A hub can collect scrubbed telemetry
(errors, crashes, durations; no command lines or names) into Azure
Application Insights; it is off unless the hub has a connection string, and
every machine can opt out (`longarm telemetry off`,
[docs/telemetry.md](docs/telemetry.md)).

## Sharing a hub with your team

Several people can share one hub and its workers without seeing each
other's work ([docs/security.md](docs/security.md) has the threat model and
the limits):

```sh
# on the admin's laptop: one user per person (optionally in teams, with quotas)
longarm admin user add alice --team web --max-jobs 8 --max-mem-gb-per-worker 64 --panel
longarm admin user add bob   --team web
#   -> a join code per user; on their laptops: longarm join hub.example.com:7443 <code> ...
longarm admin user code alice --device alice-desktop   # another device of alice
longarm admin team ls ; longarm admin user ls          # devices, usage, quotas

# workers that several people use: per-user OS isolation (LocalSystem / root)
sudo longarmd join hub.example.com:7443 <worker-code>   # data: /var/lib/longarmd (C:\ProgramData\Longarm\longarmd)
sudo longarmd service install --isolation per-user      # or MSI ISOLATION=per-user,
                                                        # deb LONGARM_ISOLATION=per-user
```

- Every job, session, file, port forward and mirror belongs to the user of
  the certificate that created it. Users see only their own jobs and
  sessions (`longarm jobs`, the panel's "My jobs", MCP tools), admins see
  everything; `longarm run --share team -- ...` lets team members read a
  job's output.
- With `[isolation] mode = "per-user"` a worker runs each user's jobs,
  sessions, shells, `exec`, file transfers and git as a local account of its
  own (`la-alice`): users can't read each other's files, code, logs,
  secrets or process memory, not even from inside their own jobs, and
  forwards only reach their own dev servers. Windows and Linux; macOS
  workers stay in shared mode.
- Quotas (active jobs, reserved memory - fleet-wide and per worker -
  sessions, per-job memory/CPU limits) are enforced by the scheduler and
  every worker. `longarm admin user disable|remove [--purge]` locks a user
  out (and deletes their data and accounts on the workers).

## Security model

- **mTLS everywhere.** Every node has a certificate from the fleet's own CA
  with an identity `longarm://<role>/<name>` (roles: admin, client, worker,
  hub, relay, release). Workers authorize every call by role; there is no
  password or shared secret on the data plane.
- **Enrollment** uses short-lived one-time join codes and pins the CA
  fingerprint; new workers wait for approval unless the hub auto-approves.
  Certificates renew automatically and can be revoked; the revocation list
  reaches every worker with its next heartbeat.
- **The hub never sees your data.** It coordinates but isn't on the data path;
  the relay forwards opaque, end-to-end encrypted packets.
- **Workers have no rights on each other.** A port link lets one worker open
  connections to one port of another, with a grant the hub signed for that
  link; nothing else ([security.md](docs/security.md#port-links)). Port
  forwards reach a worker's own loopback ports only, unless
  `[forward] targets = "any"`.
- **Jobs run as the worker's service account**, not as SYSTEM/root (unless you
  explicitly choose so) - or, on workers shared by several people, as a
  local account per user ([multi-tenancy](docs/security.md)). Treat every
  client with the `client` role as able to run arbitrary code on the workers
  it can reach - that is the point of the tool. Users who don't trust each
  other at all need separate fleets (CAs) or VMs.
- **Secrets** (`--secret-env`) travel over mTLS, live only in the job's
  memory and environment, and are redacted from logs. The MCP server only
  forwards secrets the operator allow-listed (`LONGARM_MCP_SECRETS`).
- **Local control APIs** are named pipes / unix sockets readable by the owner
  only.
- **Updates** are signed (minisign) and verified against a public key built
  into the binaries; the CI's credential can publish and roll out but not
  downgrade.

See [SECURITY.md](SECURITY.md) for reporting vulnerabilities.

## Status and limitations

Longarm is young. It has been used daily on a small mixed fleet (Windows,
Linux, macOS workers) for agent workloads, with an extensive end-to-end test
suite (real processes over real sockets) on all three platforms - but expect
rough edges, and expect the configuration format to change before 1.0.

- The fleet is designed for one team or one person: tens of workers, not
  thousands. One hub; no high availability (it isn't on the data path, so a
  hub outage only pauses scheduling and enrollment).
- Workers are trusted machines: there is no sandboxing beyond OS accounts
  (the service account, or one per user with per-user isolation; see
  [docs/security.md](docs/security.md) for what that does and doesn't
  separate). Run untrusted code in VMs or containers yourself.
- The tray app on Linux needs GTK 4 + WebKitGTK 6 (or GTK 3 with a build tag);
  macOS installers must be signed and notarized on a Mac.
- Hole punching can't beat every NAT; behind two symmetric NATs connections
  stay on the relay.

## Contributing

Issues and pull requests are welcome - see [CONTRIBUTING.md](CONTRIBUTING.md).
`go test ./...` runs the unit and end-to-end tests on Windows, Linux and macOS.

## License

[MIT](LICENSE). Claude Code is a product of Anthropic and Copilot CLI a
product of GitHub; Longarm is an independent project and not affiliated with
either.
