# Security model and multi-tenancy

Longarm runs commands on other machines by design. This document describes
who may do what, how several people can share one hub and its workers
without seeing each other's work ("multi-tenancy level 2"), what is
isolated, what is not, and where the limits are. For reporting
vulnerabilities see [SECURITY.md](../SECURITY.md).

## Levels

| Level | Who shares the fleet | Isolation between people | How |
|---|---|---|---|
| 1 | one person (or people who fully trust each other) | none | the default: `[isolation] mode = "shared"`, every workload runs as the worker's service account |
| **2** | **semi-trusted users** (colleagues, a team, students) | **API ownership + one OS account per user on every worker** | users on the hub, `[isolation] mode = "per-user"` on Windows and Linux workers |
| 3 | untrusted users | per-job VMs / containers, network namespaces | not implemented (see [outlook](#level-3-outlook)) |

## Threat model (level 2)

**Actors.** Several people ("users") share one `longarm-hub` and its
workers. Each user has one or more devices; every device has its own client
certificate from the hub's CA, which names the user. **Admins** (admin
certificates, panel administrators) are trusted: they manage users, workers
and releases and may act on everything - except where a worker's owner
limits them locally ([worker-side access policy](#worker-side-access-policy)).
The hub operator and the machines' local administrators are trusted. Workers are trusted to enforce the rules
for the users they serve.

**Assets of a user A.** A's jobs and sessions (existence, command lines,
environment, state, exit codes, output logs, terminals), A's files, code
(mirrors, worktrees, uploads), secrets passed to A's jobs, A's dev servers
(ports), A's hub data (job index entries, events, devices, quota usage).

**Adversary.** User B - curious, not necessarily malicious, but able to use
every Longarm API with B's own certificate and to run arbitrary code as B on
every worker B can reach (that is what Longarm is for). B must not be able
to **read, modify, observe or interfere with** A's assets:

- through the Longarm APIs (worker gRPC, hub gRPC, hub web panel and its
  JSON/SSE API, MCP tools, `longarm daemon`'s local API, the built-in sshd);
- from inside B's own jobs, sessions and shells on a worker A also uses.

**Out of scope:** kernel, hypervisor and driver escapes; hardware and
timing side channels; denial of service beyond what quotas cover (disk
filling, network saturation, fork bombs inside B's own cgroup / Job Object);
compromised admins, hubs or workers; level 3 (untrusted code).

## Identities

- Every certificate carries an identity URI `longarm://<role>/<name>` and,
  for devices of a user, `?user=<user>` (`longarm://client/alice-laptop?user=alice`).
  The **user** (the owner of everything a device creates) is the `user`
  parameter, or - certificates without one, e.g. from before
  multi-tenancy - the certificate's name.
- `longarm admin user add alice` creates the user on the hub and prints a
  join code **bound to the user**: the device that enrols with it gets a
  certificate of user `alice` (device name `alice`, or `alice-2`, ... or
  `--device`). More devices: `longarm admin user code alice`.
- With Microsoft Entra ID sign-in, `longarm login` and the panel create
  the user on first sign-in and enroll the device without a code (see
  "Sign-in with Microsoft Entra ID").
- Teams are hub-side groups (`longarm admin team add t1 alice bob`). A job
  submitted with `longarm run --share team` (or `--share team:t1`) may be
  **read** (state, logs) by the members of the owner's team(s); only the
  owner and admins can kill it. Sessions are never shared.
- `longarm admin user disable alice` refuses the user's calls on the hub
  and - with the next policy push, within seconds - on every worker.
  `longarm admin user remove alice` revokes the certificates of all of
  alice's devices; with `--purge` every worker also kills alice's jobs and
  sessions and deletes alice's data and OS account.

## When users are separated

The **hub** separates users as soon as one user exists (`longarm admin user
add`), a worker with per-user isolation has connected, a user-owned worker has joined, or `[tenancy]
enabled = true` is set in `hub.toml`. Until then everybody sees everything,
as in a one-person fleet (so a person with a laptop and a desktop as two
plain client certificates keeps seeing both machines' jobs).

The hub pushes a **tenancy policy** to every worker (users, teams, quotas,
disabled and purged users; persisted in `<data>/tenancy.json`). A worker
enforces ownership on its API when the policy says users are separated
**or** it runs with `[isolation] mode = "per-user"` **or** - for everybody
but the worker's owners - its `[access]` policy restricts anybody
([below](#worker-side-access-policy)).

## What is enforced

### Longarm APIs (hub and workers, default deny)

| Item | Owner | Team member (shared job) | Other user | Admin |
|---|---|---|---|---|
| jobs: get, list, logs, wait | yes | read-only | not found | yes |
| jobs: kill | yes | no | not found | yes |
| job ids | | | taken ids are refused (`AlreadyExists`, no details) | |
| sessions: list, attach, kill, logs | yes | no | not found / denied | yes (`--owner`) |
| files, workspace sync, exec, sshd/sftp | as the user's OS account (per-user) | - | - | as the admin's own account |
| port forwards (per-user) | to loopback ports the user's own processes listen on | - | refused | any |
| worker metrics | aggregate (no process lists) | - | - | with process lists |
| hub job index, events, `longarm jobs`, MCP tools | own + team-shared | | hidden | everything |
| hub node list | workers without process lists and others' mirrors; own devices | | others' devices hidden | everything |
| audit log, users, teams, join codes, links, releases, profiles | no | | no | yes |
| web panel | "My jobs", nodes (read-only), "Me" for user logins | | | everything ("People" page for users, teams, quotas) |
| a worker with `[access]` | users not in `users` get node info only | | | an admin certificate of a non-owner gets what `admins` allows (`own`: the "Owner" column; `observe`; `none`) |
| a user-owned worker (`longarm worker add`) | its owner; users it is shared with | | hidden on the hub, never placed, node info only on the worker | sees, shares, unshares and removes it on the hub; on the worker refused like anybody unless owner or shared with ([below](#user-owned-workers)) |

The "Admin" column is the default (`[access] admins = "full"`); a worker's
owner can narrow it on that worker, and the worker's owners always keep it.

Every worker audit record carries the acting certificate and, for devices
of a user, the user (`"user": "alice"`); the hub's records name the caller
(`web/<login>`, `admin/<name>`). A request the hub relays to a worker (panel
kill / logs / submit) is checked on the hub and audited on the worker as
`<who> via hub`; hub-submitted jobs run as the requesting user.

### Quotas

Per user (`longarm admin user add|set ... --max-jobs ...`) with a default in
`hub.toml` `[tenancy.default_quota]`:

| Quota | Enforced by |
|---|---|
| `max_jobs`, `max_mem_gb` (fleet-wide active jobs / reserved memory) | the hub's scheduler (`longarm run` placement, panel and `longarm admin run`): the request waits in the queue (or fails with `--no-queue`) |
| `max_jobs_per_worker`, `max_mem_gb_per_worker`, `max_sessions_per_worker` | every worker, on submit / session start |
| `job_mem_gb`, `max_job_mem_gb`, `job_cpus`, `max_job_cpus` (per-job defaults and caps) | every worker: Job Object memory / CPU-rate limits on Windows, cgroup `memory.max` / `cpu.max` on Linux (per-user isolation) |

### Per-user OS isolation on workers

`[isolation] mode = "per-user"` in `longarmd.toml` (MSI `ISOLATION=per-user`,
deb `LONGARM_ISOLATION=per-user` / `longarmd-peruser.service`, `longarmd
service install --isolation per-user`). longarmd runs as **LocalSystem**
(Windows) or **root** (Linux) and maps every Longarm user to a local account
`la-<user>` (prefix: `[isolation] account_prefix`; names that don't fit are
shortened with a hash), created on first use.

| | Windows | Linux |
|---|---|---|
| account | local user, random 40+ character password nobody knows; only "Log on as a batch job"; denied interactive, remote desktop, network and service logon | `useradd`: locked password, own group, private home (0700), no supplementary groups |
| how workloads start | S4U logon (`LsaLogonUser`, no password; needs SeTcbPrivilege) → primary token → `CreateProcessAsUser`, suspended until assigned to the job's Job Object; a window station of its own logon session; only its standard handles inherited | `setuid`/`setgid`, supplementary groups dropped, its own cgroup v2 (`/sys/fs/cgroup/longarm/<account>/<job>`, started inside it with `CLONE_INTO_CGROUP`), umask 077 |
| profile / home | `C:\Users\la-<user>`; longarmd keeps the profile loaded while it runs (HKCU for all the user's processes) | `/home/la-<user>` |
| who runs what as the user | jobs, sessions (ConPTY / pty), `exec`, the built-in sshd's shells, commands and **sftp**, and a short-lived **user helper** (`longarmd user-helper`) that does every Files and Workspace call, job preparation (git fetch, worktrees) and worktree pruning | same |
| what stays privileged | longarmd and the per-job **shims** (they start the workload as the user and write `status.json` / `out.log`; job directories are not writable by the user, so a user cannot make a privileged shim do anything else) | same |

**Data layout** (default `<data>/users`; `[isolation] users_dir`):

```
<data>/                     SYSTEM + Administrators / root only; users may only traverse it
  tls/ audit.log tenancy.json ...
  users/                    traverse only (no listing)
    la-alice/               SYSTEM + Administrators (+ traverse for la-alice) / root:la-alice 0710
      owner                 "alice"
      jobs/ sessions/       job and session dirs: privileged, private
      mirrors/ worktrees/   owned by la-alice, private (owner + SYSTEM + Administrators / 0700)
      staging/ tmp/
```

Mirrors are **per user**: alice's objects never land in a mirror bob's jobs
read. The scheduler's cache affinity is per user too (workers report
`<user>/<repo>`; other users don't see the repository names).

**Secrets** (`--secret-env`) are kept in the shim's memory (a privileged
process) and the job's environment (the user's processes). Other accounts
cannot read either (Windows: no `PROCESS_VM_READ` on other users' processes;
Linux: `/proc/<pid>/environ` needs ptrace access). On disk they exist
nowhere; logs are redacted.

**Port forwards** (`longarm forward`, `fleet_forward`, ssh `-L`): only to
loopback ports on which a process of the caller's own account listens
(Windows: `GetExtendedTcpTable` → owning process → its token's user;
Linux: `/proc/net/tcp{,6}` → socket uid). A port with a listener of another
account is refused, too.

**The built-in sshd** (`longarm ssh-config`, `longarm proxy`): shells, `exec`
and sftp run as the caller's account; direct listeners with
`authorized_keys` are ignored in per-user mode (they have no Longarm
identity).

**Toolchain** (hub profiles) is installed machine-wide in per-user mode
(winget `--scope machine`, npm global packages next to `node.exe` on
Windows, `uv` in `/usr/local/bin` on Linux) so every account has git, node,
pnpm, uv, pwsh, claude and copilot; per-user agent settings (`~/.claude`,
`~/.copilot`) stay in each account's profile.

**macOS** workers refuse `mode = "per-user"` with a clear error (launchd +
setuid per user is not implemented); they stay usable in shared mode, with
API ownership only.

### Loopback between users (Linux)

Without help from the firewall, every local process can connect to every
loopback listener: bob's job could talk to alice's dev server on
`127.0.0.1:3000`. With `[isolation] loopback = "isolate"` (the default)
longarmd installs and owns the nftables table `inet longarm` that refuses
TCP connections between **different isolation accounts** over `lo`
(`127.0.0.0/8`, `::1` and the machine's own addresses, which are routed
via `lo` too):

```
table inet longarm {
	chain output {
		type filter hook output priority 0; policy accept;
		oif != "lo" return
		ct direction original ct mark and 0xffff0000 == 0xffff0000 tcp flags & (syn | ack) == syn reject with tcp reset
		ct state invalid tcp flags & (syn | ack) == syn | ack meta skuid { 1001, 1002 } counter drop
		ct state new jump sender
		ct direction original tcp flags & (syn | ack) == syn jump sender
		ct direction reply ct mark and 0xffff0000 != 0x00000000 tcp flags & (syn | ack) == syn | ack jump answer
		ct direction reply ct mark and 0xffff0000 != 0x00000000 meta l4proto udp jump answer
	}
	chain sender {                          # who opened the connection
		ct mark set ct mark and 0x0000ffff
		meta skuid 1001 ct mark set ct mark or 0x03e90000 return   # la-alice
		meta skuid 1002 ct mark set ct mark or 0x03ea0000 return   # la-bob
	}
	chain answer {                          # who answers it
		ct mark and 0xffff0000 == 0x03e90000 meta skuid 1001 return
		ct mark and 0xffff0000 == 0x03ea0000 meta skuid 1002 return
		meta skuid != { 1001, 1002 } return
		ct mark set ct mark or 0xffff0000 counter drop
	}
}
```

How it works: the first packet of a connection on `lo` records the sending
account (`meta skuid` of the socket) in bits 16-31 of the conntrack mark:
its tag, the low 16 bits of its uid (0 for root and every non-account uid).
The listener's answer - the SYN-ACK, which the kernel attributes to the
listening socket, SYN-cookie and retransmitted SYN-ACKs included - is
checked in the output hook: an answer from another account's socket (or
one without a full socket the rule can attribute, which is rare) is
dropped and the connection marked blocked; the client's retransmitted SYN
then gets a TCP reset, so `connect()` fails with "connection refused"
after about a second. UDP replies between accounts are dropped the same
way.

The tags depend on the uid alone, so replacing the table when accounts are
added or purged never re-labels running connections (a position in the
account list would: long-lived connections would then count as another
account's). Accounts with uids below 65535 - useradd's usual range - are
their own tag; two accounts whose uids agree in the low 16 bits (e.g. 1001
and 66537), or a uid ending in 0x0000 / 0xffff, cannot be told apart: the
rules are then refused like an `nft` failure (warning, or no start / no new
account with `loopback = "require"`) - give the account another uid.

- **Kept working**: connections within one account (IPv4 and IPv6),
  root's connections to the accounts' ports (Longarm's port forwards, ssh
  `direct-tcpip`), the accounts' connections to root and system services
  (DNS on `127.0.0.53`, databases, longarmd's local API), everything not
  on `lo`, and other uids among themselves. It is uid based, so jobs,
  sessions, `exec`, sftp and the user helper are covered alike, with or
  without cgroups.
- **Lifecycle**: longarmd replaces the table atomically (`nft -f`) at start
  and whenever an account is created or purged; it stays while longarmd is
  stopped or restarting (the jobs outlive longarmd); `longarmd leave
  --purge`, uninstalling (deb prerm; `longarmd service uninstall` of a
  per-user service) and `loopback = "allow"` remove it. A longarmd switched
  to shared mode by editing `longarmd.toml` leaves it (it cannot tell its
  old table from another per-user longarmd's): remove it by hand with `nft
  delete table inet longarm`. Only one per-user longarmd per machine (the
  table name is fixed).
- **Ordering**: at start the table is in place before any space's
  supervisor runs a job; a new account (or a re-created one) is added to
  the table before anything runs as it - with `loopback = "require"` a
  failure leaves the user without a space (their next call retries).
- **Failure**: without `nft` (package `nftables`), without root or when the
  kernel refuses the rules, longarmd logs a warning and runs on unprotected;
  `loopback = "require"` makes it refuse to start (and to create accounts)
  instead. `loopback = "allow"` turns it off.
- **Requirements**: nftables with conntrack on `lo` (the default; a
  `notrack` rule for `lo` disables the protection), kernel and nft from
  the last years (`ct mark`, `meta skuid`, `reject` in output: Linux 4.x,
  nft 0.9). The upper 16 bits of the conntrack mark of loopback
  connections belong to Longarm. The table turns connection tracking on
  if nothing else (ufw, Docker, firewalld) did - all traffic is then
  tracked; mind `nf_conntrack_max` on very busy hosts. An account's
  SYN-ACK that conntrack considers invalid (sender unknown) is dropped too.
- **Limits**: UDP between accounts is one-way deliverable: a datagram from
  bob's socket to a port alice's process bound on `lo` **arrives** (it
  opens the conntrack flow; the output hook sees only the sender's socket,
  and in the input path the receiving socket is generally not looked up
  yet, so no rule can match its uid); only alice's replies are dropped. So bob can send requests
  to alice's UDP services (DNS-like or "fire and forget" protocols act on
  them) but gets no answers; protect UDP listeners by authentication or
  bind them elsewhere. Unix sockets (filesystem: protect them with file
  permissions; abstract ones are reachable by everyone) are not covered;
  neither are raw protocols other than TCP and UDP. SYN-cookie SYN-ACKs
  carry the listening socket and are judged like others; answers the
  kernel sends without a full socket (rare) fail closed. With
  `net.ipv4.tcp_fastopen` server mode enabled (not the default) data in a
  Fast Open SYN reaches the listener before the answer is dropped. A
  connection is attributed to the uid that **created** the socket.

Evaluated and not used: per-job network namespaces (outbound networking
would need veth/NAT or slirp per job), the `socket cgroupv2` match on the
receiving side (only listeners inside a job's cgroup would be covered -
not sessions or the user helper - cgroup ids are resolved when the rule is
loaded, kernel 5.13+), cgroup `connect4/6` BPF programs (BPF loading in
Go, a large dependency) and `iptables -m owner` (sender only).

**Windows** has no equivalent (Windows Firewall does not filter loopback
traffic of ordinary processes): the gap below stays there.

## Worker-side access policy

The owner of a worker machine limits, **on the worker**, what the hub's
admins and users may do there. The policy is `[access]` in
`<data>/longarmd.toml` - nothing the hub sends (tenancy policy, profiles,
commands) can change it:

```toml
[access]
users  = ["alice", "bob"]   # who may use this worker (default: everybody the hub admits)
owners = ["alice"]          # always full rights for their role; users implicitly
admins = "own"              # other admin certificates: full (default) | own | observe | none
hub_job_details = false     # default true
```

| Caller (its user = the certificate's `user`, else its name) | Gets |
|---|---|
| user not in `users` ∪ `owners` (when `users` is set) | `Node.Info` only; everything else `PermissionDenied`, audited with the reason |
| an owner | today's rights for its role: an owner's admin certificate is a full admin, an owner's client certificates act on everything when the hub does not separate users |
| admin certificate of a non-owner, `admins = "full"` | everything (as without `[access]`) |
| ... `admins = "own"` | what a client certificate of its user gets: its own jobs and sessions, no `Admin.Gc` / `Admin.Shutdown`, no process lists, no attaching to or killing others' sessions and jobs, port forwards as for any user |
| ... `admins = "observe"` | `Node.Info`, `Metrics.Snapshot` / `Metrics.Stream` without process lists |
| ... `admins = "none"` | `Node.Info` |
| client certificates | unchanged (subject to `users` and ownership) |

- A restricting policy (`users` or `owners` set, or `admins` other than
  `full`) turns on **ownership checks** on the worker even when the hub
  does not separate users - otherwise in a one-person fleet everybody
  acts as an admin. The owners are exempt: their devices keep seeing and
  controlling each other's jobs and sessions as before (with plain
  certificates without a user, list the certificate names as owners).
  Per-worker quotas still come only from the hub's tenancy policy.
- The **hub session**: with `users` set or `admins` other than `full`,
  `[hub] remote_jobs` defaults to `none` (the panel's and `longarm admin`'s
  kill / logs / run through the hub are refused); an explicit value in the
  file wins. Jobs the hub submits (`remote_jobs = "all"`) must still be for
  a user in `users`.
- `hub_job_details = false`: the job reports, the audit lines forwarded to
  the hub and the metrics the worker sends to the hub carry no command
  lines, environment (names), working directories, workspace repositories,
  file paths, ssh commands, forward targets, error texts or process lists.
  The hub keeps job id, owner, submitter, sharing, state, times, exit code
  and resources (a `type` label replaces the command for the scheduler's
  sizing of claude / copilot jobs), so scheduling, quotas and the panel's
  job list keep working. Repository names in the worker's mirror list (the
  scheduler's cache affinity) are still reported. The local `audit.log`
  keeps the full records.
- longarmd refuses to start with an unknown `admins` value or an invalid
  user name, and logs the effective policy once at start
  (`access policy policy="users=... owners=... admins=..." remote_jobs=... hub_job_details=...`).

**What it protects against**: hub admins acting with their admin
certificates (directly or through clients and MCP tools) and through the
hub session (web panel, `longarm admin kill|logs|run`), and hub-side
visibility of what the worker's users run.

**What it does not protect against**:

- **Names are the hub's word.** `owners` and `users` match the user a
  certificate names, and the hub decides that: any hub admin can enroll a
  device as an owner (`longarm admin join-code --role admin --user alice`
  - no CA key needed), and whoever holds the CA key can mint one. The worker
  can't tell such a certificate from alice's own. Against insiders the
  policy only holds together with [end-to-end
  trust](#end-to-end-trust-the-hub-is-not-trusted-with-access) (owners'
  certificates endorsed by keys pinned on the worker); without it, it stops admins who
  use their own certificates and the hub session, nothing more. longarmd
  warns at start when `owners` / `users` are set without it.
- **Local administrators** of the worker machine (they can edit
  `longarmd.toml` and read everything).
- **Shared isolation**: `admins = "own"` makes an admin an ordinary user,
  and in shared mode ordinary users' `exec`, files and sshd run as the
  service account, which can read other users' worktrees and logs. Use
  per-user isolation, or `observe` / `none`.
- Jobs and sessions started before a policy change keep running; the
  policy is read at start (change it, restart longarmd).

## User-owned workers

A user can register their own machine as a worker (`longarm worker add`,
[usage](usage.md#your-own-workers)). The worker code names the user as the
worker's **owner**; the worker joins approved and is usable by its owner
only, until shared - by user, team or everyone. Only users an admin
granted `can_share` (`longarm admin user set <u> --can-share`) may share
their workers; admins share and unshare any. Workers without owner (admins'
join codes) are the shared fleet as before. An owned worker turns user
separation on (like the first user).

| Caller (its user) | On a user-owned worker |
|---|---|
| the owner | everything its role allows, as on any worker |
| a user it is shared with (by name, a team of theirs, or everyone) | like any user of a multi-user worker: its own jobs and sessions only |
| anybody else, **admin certificates included** | hub: the worker is hidden (lists, events, history, panel) and never placed for them, a named submit is "not found"; worker: `Node.Info` only, everything else (`Admin.Shutdown` / `Gc` too) `PermissionDenied`, audited as `worker owner/shares` |
| hub admins (the hub's API, panel) | see it with owner and shares, share, unshare and remove it |

Admin certificates shared with get no admin rights over the owner's jobs
and sessions there. Every code, join, share, unshare and removal is audited
on the hub (`workers/code`, `workers/join`, `workers/share`, `workers/remove`);
the worker audits every change it receives (`worker/access`).

**Enforced twice, decided once.** The hub applies the rule when it lists
and places, and pushes owner and shares to the worker
(`HubCommand.worker_access` at every connect and change; kept in
`<data>/worker-access.json` across restarts and hub outages). The owner
holds from enrollment on: `longarmd join` writes it (and remembers the owned
code in the data dir's `hub.toml`), so a worker started before it ever
reached the hub - or without its file - serves its owner only; an
unreadable file refuses everybody until the hub sends the rule again. Pushes
carry a version: a late, older push never undoes a newer unshare. The
worker refuses everybody else on every call, and closes the connections -
with their sessions, forwards, ssh and log streams - of users who lose
access, whether by an unshare or by leaving a team the worker is shared
with. Followed log streams through the hub are re-checked on every poll.
Disabling or removing a user drops the user's unused worker codes; a code
whose owner is no longer a user does not join. But the worker's copy **comes from the hub**:
whoever controls the hub (its admins, its database, the CA key) can change
the owner or the shares, or enroll a device under the owner's name. The
pushed rule is applied strictly as an **extra deny after** the worker's own
`[access]` policy and end-to-end trust: it can never grant what those deny,
the hub has no way to write `longarmd.toml` or `trust/trust.toml`, and it
does not widen `[hub] remote_jobs` (jobs the hub submits must be for a user
the worker serves). **The hub session** is no way around it either: the hub
relays kill, logs and submit (panel, `longarm admin kill|logs|run`) to a
user-owned worker only for a caller whose user may use it - admins
included; others get `PermissionDenied` (audited as `workers/denied`). The
owner's own panel login acts as the owner and keeps working (with
`remote_jobs = "control"`, the default without `[access] users` or trust;
the worker's `remote_jobs` still applies on top).

**Against hub insiders** pin it on the machine, as `longarmd join` suggests:
`[access] users = ["alice"]` and `owners = ["alice"]` (plus whom alice shares
with), and [end-to-end trust](#end-to-end-trust-the-hub-is-not-trusted-with-access)
(`longarmd trust add-user <alice's key>`), so only devices alice endorsed get
in. With trust on, **sharing needs the worker's owner too**: a worker shared
with bob on the hub still refuses bob's devices until bob's user key is in
that worker's `trust.toml` (`longarmd trust add-user <bob's key>`), and bob
in `[access] users` if alice pinned it.

## What is not isolated (known gaps)

Level 2 keeps users apart through the OS's user separation, not through
containers. These remain:

- **Loopback networking between users on one worker** on Windows, and on
  Linux without nftables or with `[isolation] loopback = "allow"`: B's
  process can connect to A's dev server on `127.0.0.1:3000` directly (only
  Longarm's own forwards check the port's owner). On Linux with the default
  rules ([above](#loopback-between-users-linux)) TCP is refused; UDP
  datagrams still reach another account's socket (only its replies are
  dropped), and unix sockets are not covered. Bind dev servers with
  authentication, prefer unix sockets in the user's home, or use level 3
  (network namespaces).
- **Process lists.** On Linux every account sees every process and its
  command line in `/proc` unless `/proc` is mounted with
  `hidepid=invisible` - longarmd warns at start. Do it:
  `mount -o remount,hidepid=invisible /proc` and in `/etc/fstab`
  (`proc /proc proc defaults,hidepid=invisible 0 0`). On Windows other
  users' process **names** are visible (not their command lines or
  memory).
- **World-accessible OS objects** the workloads create themselves: files in
  shared places (`/tmp` - the umask of 077 makes new files private,
  `C:\Users\Public`, `C:\ProgramData`), named pipes and shared memory with
  permissive default ACLs, abstract unix sockets. Workloads should keep
  their data in their home, worktree or `TMPDIR`.
- **Shared isolation mode.** Without `mode = "per-user"` (and on macOS)
  every workload runs as the same service account: the API hides other
  users' jobs, sessions and process lists, but any job can read the service
  account's files - including other users' worktrees and logs - and port
  forwards reach any address the worker can reach. Use it only for people
  who trust each other.
- **Fleet-wide quotas** are enforced by the hub's scheduler. A client that
  submits to a worker directly (`longarm run --on w1`) is bound only by the
  per-worker quotas there.
- **Disk space** is not limited per user (GC still collects old jobs and
  mirrors); neither is network bandwidth.
- **Admins** see and do everything (including attaching to others'
  sessions) on workers whose owner did not limit them with `[access]`;
  worker administrators can read everything on their machine; the hub can
  submit jobs as any user to workers with `[hub] remote_jobs = "all"`.
- **Disabling a user** refuses new calls at once and closes the user's open
  connections on the hub and - when the policy arrives, within seconds - on
  every worker; jobs that are already running keep running (kill them, or
  remove the user with `--purge`).
- **Windows profiles** stay loaded until reboot if longarmd itself is killed
  (not stopped); a later purge then removes the account but leaves
  `C:\Users\la-<user>` until the next reboot.
- **Side channels** (CPU caches, timing, `/proc/meminfo`-style aggregates,
  the hub's node metrics) are out of scope.

## Audit trail

Every worker writes one JSON line per security-relevant call to
`<data>/audit.log` (who, what, where; denied calls too) and forwards the
lines to the hub (`longarm admin audit`, the panel). For the machine's
owner the hub's copy is not enough: the hub and its admins could change or
drop it. Two things add accountability that doesn't depend on them:

- **Hash chain.** Every record carries `"seq"` (1, 2, ...) and `"prev"`,
  the hex SHA-256 of the previous line (`""` for the first line of the
  file). `longarmd audit verify [--file PATH]` checks the chain and the
  seq numbers and prints `ok: N records` or the first broken record (line,
  seq, reason; exit code 1): an edited, deleted, inserted or reordered line
  is found. A chain restarts (`"method": "audit/chain-start"`, with the
  reason) when longarmd finds a log written before chaining, a line cut
  off by a crash, or a file that changed behind its back; `verify` lists
  those places.
- **Sinks** (`[audit]` in longarmd.toml, [usage](usage.md)): a copy of
  every record goes to places the owner controls - another file (a USB
  disk, a mount), a syslog server (UDP or TCP, RFC 5424, facility auth), a
  webhook (HTTPS POST of NDJSON batches, optional bearer token). Delivery
  runs in the background: a slow or dead sink never delays a call. Each
  sink queues up to 10 000 records (16 MiB); when that overflows the
  oldest are dropped and a chained `"audit/dropped"` record with the sink
  and count follows. Failing sinks are retried with backoff (up to 1 min)
  and logged at most once a minute; at stop longarmd flushes for up to
  2 s. Records may arrive twice after a retry (compare by `seq`/`prev`).
  An invalid `[audit]` section keeps longarmd from starting.

What this does **not** protect against: the chain is no signature.
Someone who can write the worker's disk (an administrator / root of the
machine) can rewrite audit.log with a recomputed chain, change its last
line unnoticed by `verify`, or stop longarmd. The protection against them
is the off-machine copy: compare audit.log with a sink's copy (the same
lines and hashes; `verify --file` checks a copy, which may start
mid-chain). Records written while longarmd was stopped don't exist, and
what a sink lost to an overflow is only counted.

## Sign-in with Microsoft Entra ID

With `[auth.entra]` (docs/usage.md "Sign in with Microsoft Entra ID") the
hub issues certificates and panel sessions to whoever Microsoft vouches for,
instead of to holders of a join code or a panel password: **the hub vouches
for the user after verifying the Microsoft token**. It checks every token
itself - RS256 signature against the tenant's published keys (fetched from
`<authority>/<tenant>/discovery/v2.0/keys`, cached, refetched for an unknown
key at most once a minute; `none` and HMAC algorithms are refused), issuer
`<authority>/<tenant>/v2.0`, tenant (`tid`), audience (the hub's app: access
tokens `<client id>` or `api://<client id>`, ID tokens `<client id>`),
expiry and not-before (2 minutes of skew), an object ID (`oid`), and:

- `longarm login` (Enroll.EntraJoin, no certificate needed, rate limited per
  IP like join): the delegated scope (`scp` contains `longarm.connect`), or
  an app-only token of an app listed in `apps` (CI, acting as its configured
  user, never as an admin);
- web sign-in: the nonce of this sign-in. State, nonce and PKCE verifier
  live in an HMAC-signed, HttpOnly, SameSite=Lax cookie for 10 minutes,
  each state is accepted once, the target after sign-in is a path on the
  hub, and only top-level navigations start a sign-in. In spa mode the
  browser redeems the code (no client credential exists) and posts the ID
  token, same-origin only; app-only tokens never open panel sessions.

What it changes:

- The Entra tenant becomes an authority over the hub: anyone it lets sign
  in (narrow with `allowed`) gets a client certificate of their own user,
  and whoever matches `admins` gets an **admin** certificate. An Entra
  account takeover, or a too broad `admins` entry like `*@domain`, is an
  admin compromise of the hub. Users are tied to the Entra object ID, not
  the UPN, so a renamed or reused UPN doesn't inherit another user.
- Tokens are never logged or stored (the hub logs the user, UPN, object ID
  and why a token was refused; sign-ins are audited as `entra/login` and
  `entra/panel-login`, users it creates as `users/add` by `entra/<upn>`).
- `longarm login` sends the token to the hub over TLS verified like `longarm
  join`: pin the CA with `--ca-fingerprint`. Without a pin (trust on first
  use) a man in the middle could relay the token - valid about an hour and
  only for this hub's app - to the real hub and enroll a device as the user.
- **End-to-end trust still applies on top**: a device enrolled by Entra
  sign-in binds its node key like any other, and workers with trusted users
  refuse it until the user's key endorsed it (`longarm trust ...`). Entra
  sign-in replaces the join code, not the user keys - the hub (and now the
  Entra tenant) still can't mint access to such workers.
- `longarmd login --az` (EntraJoin with kind `worker`) enrolls a worker
  owned by the token's user: approved at once, a worker certificate, never
  admin. App-only tokens (`apps`) may not: a CI app acting as a user must
  not be able to add machines that user's jobs get placed on.

### Pairing (`/link`)

`longarm login` without az and `longarmd login` enroll by pairing, a device
authorization flow run by the hub (Microsoft's device code flow is often
blocked by tenant policy): the machine sends its CSR (Enroll.StartLink,
no certificate needed) and shows the hub's `/link` address and a user code;
a person signed in to the panel types the code there and approves; the
machine collects its certificate (Enroll.PollLink with a poll token only it
has). **Approving is signing in: the approver becomes the owner** - a
client is the approver's device (admin certificate only for `admins`,
rules re-checked at approval: `allowed`, disabled users), a worker the
approver's own worker, never admin.

The danger is phishing: an attacker starts a pairing on *their* machine
and gets a victim to approve it - then the attacker's machine holds the
victim's client certificate, or (worse, and quieter) the victim's jobs get
placed on the attacker's "worker". Hence:

- **The code is typed, never in a link.** `/link` ignores codes in the URL
  and the device opens `/link` without one, so "click here to finish
  signing in" can't carry an attacker's code; the page says to enter only
  codes from a device you are setting up yourself.
- **The confirmation shows what asks**: worker or client, the requested
  name, host name, OS/arch, version, the address the hub saw the request
  from, the node key ID of its end-to-end trust binding (compare with
  `longarm trust show` / `longarmd trust status` on the device), and how long
  ago it started; for workers: "Only confirm if this is your machine: jobs
  you run may be placed on it."
- **Short-lived and single use**: codes are 8 characters of a 20-letter
  alphabet (consonants: no words, no look-alikes; 20^8 = 2.6e10), live
  10 minutes, are spent by the first approve or deny (a failed approval
  tells the device why), and pairings live in the hub's memory only (a
  restart forgets them), each a minute past its expiry at the latest. An
  approved certificate is handed out once, before that, and only while the
  node is still approved (revoked meanwhile: "denied", no tailnet key).
  What a pairing keeps is bounded: the CSR (at most 8 KiB, one PEM block -
  the limit of every enrollment - re-encoded; dropped once decided or
  expired) and short, printable device fields. StartLink is rate limited per address like joins;
  at most 5 pending pairings per address and 200 in all; polls faster than
  the 5 s interval are refused (the client slows down); code lookups on
  `/link` are rate limited per address and per user (10, then one per 10 s):
  guessing a code would enroll someone else's machine as one's own. The
  device prints whose it became ("owned by ...", "signed in as ...").
- **Only real sign-ins approve**: with `[auth.entra]`, a panel session that
  a Microsoft sign-in opened (marked by an HMAC cookie bound to that
  session); password logins - perhaps a shared admin account - can't. Only
  hubs without Entra accept any panel login of a Longarm user.
- **CSRF**: the forms are POST only, need an `Origin` of the hub and (when
  the browser sends it) `Sec-Fetch-Site: same-origin`, and carry a token
  HMAC-bound to the session, the step and the pairing (10 minutes); the
  page has a strict CSP and can't be framed.
- Audited on the hub: `link/start`, `link/approve` (with the node or the
  error), `link/deny`, `link/expire` - who, kind, name, host, address.

The poll token, the code and the CSR are not secrets an observer can turn
into a certificate: the certificate is useless without the machine's
private key, which never leaves it. Pin the hub's CA (`--ca-fingerprint`,
built into the install scripts): pairing runs over the same TLS as join.
**End-to-end trust is still recommended**: a pairing approved by mistake
(or an Entra account takeover) yields at most a certificate the hub signed;
workers and clients that require endorsements still refuse the device until
your user key endorses it.

## End-to-end trust (the hub is not trusted with access)

Everything above trusts the hub: its CA decides who a certificate belongs
to, so whoever holds the CA key - the hub's operators, an admin with access
to its host or storage, a thief of its database replica - can mint a
certificate for any name ("alice", an admin, a worker) and use it.
**End-to-end trust** removes that power: workers accept only devices that a
**user key** endorsed, and clients accept only workers a user key
endorsed. User keys never leave the users' devices; the hub only carries
the proof and can't produce one.

**How it works** (`internal/pki/trust.go`):

- Every node (worker, client device) has a long-lived Ed25519 **node key**
  (`<data>/trust/node.key`, `<config>/trust/node.key`), created at `join`.
- Its certificate carries an extension with the node key's signature over
  the certificate's TLS key (**binding** - the extension can't be moved to
  another certificate) and, once a user endorsed it, the **endorsement**:
  the user key's signature over the node key, the node's kind (`worker` or
  `device`), its name and the user. Nodes put it into their CSRs; the hub
  checks the binding and copies it unchanged into the certificate (join and
  every renewal).
- A worker with trusted users in `<data>/trust/trust.toml` checks every
  client certificate in the TLS handshake: endorsed by the pinned key of the
  certificate's user, as a device, under the certificate's name, not a
  denied node key. A client with `require_workers = true` checks workers the
  same way (its own user key and the users it added). Refusals are audited
  (`trust/denied`).
- With trusted users, the hub session's default `[hub] remote_jobs` is
  `"none"` (logs are users' data); set it explicitly to allow more.

**Setting it up**

```sh
# alice's laptop: a user key, this device endorsed (the certificate is renewed to carry it)
longarm trust init                        # prints alice's user key line
# every worker alice may use (its owner):
longarmd trust add-user <alice's user key line>
# alice's workers, so her clients can verify them:
longarmd trust request                    # on the worker
longarm trust endorse <request>           # on alice's laptop
longarmd trust accept <endorsement>       # on the worker (renewed within seconds)
longarm trust require on                  # on alice's devices, once her workers are endorsed
# alice's second device: longarm trust request there, longarm trust endorse <it> on the
# laptop, longarm trust accept <endorsement> there (or move the user key: export-key / import-key)
```

**A worker insiders should stay out of** combines it with the worker-side
access policy and an audit copy off the machine:

```toml
# <data>/longarmd.toml
[access]
owners = ["alice"]          # names alone are the hub's word; trust.toml makes them alice's key
admins = "none"
hub_job_details = false     # no command lines to the hub
[audit]
webhook = "https://audit.example.com/ingest"   # or syslog / file: a copy the hub can't touch
```

plus `longarmd trust add-user <alice's key>` (trust.toml) and per-user
isolation if other users share the machine.

**What it protects against**

| Who | Without | With end-to-end trust |
|---|---|---|
| hub admin with an admin certificate | exec, files, sessions, forwards on every worker | refused by workers that don't trust the admin's own user key |
| anyone holding the CA key (hub operator, stolen replica) | mint "alice" or a worker and use it | minted certificates carry no endorsement: refused; a minted worker is refused by clients that require trust |
| the hub's relay / a network position plus the CA key | impersonate a worker to clients (MITM) | the client refuses the unendorsed certificate |
| the hub session (panel, `longarm admin logs/kill/run`) | control by default | none by default |

**What it does not change**

- The hub can still **deny**: revoke certificates, disable users, refuse
  to renew. It can't grant.
- The hub still sees **metadata**: which nodes exist and are online, load,
  job ids and states, audit lines (combine with the worker's `[access]
  hub_job_details = false` to drop command lines) and the endpoints it
  relays.
- Teams come from the hub: a job shared with `--share team` can be read by
  whoever the hub puts into the owner's team - among the users the worker
  trusts. Don't share with a team when that matters.
- A worker's local administrators see everything on their machine; a
  user's devices hold that user's key.
- Toolchain profiles (a fixed set of tools) and signed self-updates (the
  release key is not the hub's) still come through the hub.
- A lost or leaked **user key**: create a new one (`longarm trust init` on a
  device without one), re-endorse your devices and workers, and have the
  owners replace it (`longarmd trust add-user` replaces a user's key). A
  lost device: `longarmd trust deny-node <its node key>` on the workers, and
  revoke it on the hub.
- A hub too old to copy the extension issues certificates without it: peers
  that require trust refuse them (fail closed).

## Diagnostic bundles

`longarmd diag` / `longarm diag` (docs/usage.md "Bug reports /
diagnostics") collect logs and state with names replaced by fleet-wide
pseudonyms and secrets removed; private keys, token files, the audit log
and job command lines / output are left out unless the user adds the last
two explicitly. The zip is written locally first; nothing leaves the
machine without `--upload` (and a confirmation, or `--yes`).

An uploaded bundle can be read by **the hub's admins** (`longarm admin diag
ls|get`; admin role only - clients and workers may upload but not list or
download, not even their own) and by **whoever has read access to the blob
container** (Storage Blob Data Reader/Contributor, the account's owners).
Like everything else on the hub, admins are insiders here: a worker's
`[access]` policy limits what they may *do* on the worker, not what a user
chooses to send them. Pseudonyms are keyed with the fleet's CA, so anyone
holding the CA certificate (every node) can test a guessed name against
them. Retention is the storage account's lifecycle policy: deploy/azure
deletes bundles 30 days after the upload; a `file://` store keeps them
until the operator deletes them. The hub logs uploads (report id, size,
uploader pseudonym) and downloads (report id, admin), never the content.

## Operations

- **Turning it on for an existing fleet**: add the users on the hub
  (`longarm admin user add ...`; existing client certificates keep working
  and act as the user of their name), then switch workers one by one to
  per-user isolation: stop longarmd, reinstall the service as LocalSystem /
  root with `--isolation per-user` (or set `[isolation] mode = "per-user"`
  and change the service account), start it. Jobs from shared mode stay
  visible to admins (legacy space) until GC removes them.
- **Data dir on Linux**: every parent of the users dir must be traversable
  by the accounts (`o+x`): `/var/lib/longarmd` (root's default data dir and
  the deb's per-user unit's) is; `~root/.longarm` is not - `longarmd service
  install --isolation per-user` refuses it (with `--data-dir`) and longarmd
  refuses to start on it, both naming the blocking directory. Without
  `--data-dir`, the install uses `/var/lib/longarmd` and copies an
  enrollment an older longarmd left in `~root/.longarm` (root's passwd home,
  not `$HOME`; copies 0600, dirs 0700; a source not owned by root or
  writable by group/others is refused; remove the old copy afterwards). On
  Windows the per-user default is `C:\ProgramData\Longarm\longarmd`, created
  with a protected DACL (SYSTEM and Administrators) - ProgramData's
  inherited ACL would let every local user read the node key. Any local
  user may pre-create it, so an existing one owned by another account is
  refused, and one with an inherited or open ACL is locked down again
  before anything is written to it. See docs/usage.md "Data directory".
- **cgroups** (Linux): longarmd creates `/sys/fs/cgroup/longarm`; with
  systemd you may prefer `Delegate=yes` and `[isolation] cgroup_root`
  inside the unit's cgroup. Without cgroup v2, jobs run in process groups
  (no memory/CPU limits, escaped processes survive a kill) and longarmd
  warns.
- **Removing a user**: `longarm admin user remove alice --purge`. Workers
  that are offline purge when they reconnect (the purge list is part of the
  policy). `longarmd leave --purge` and uninstalling a per-user worker
  delete all `la-*` accounts it created.

## Telemetry

A hub with an Application Insights connection string collects telemetry
from itself and its nodes ([docs/telemetry.md](docs/telemetry.md)). In the
terms of this threat model: the data leaves the fleet (to whoever can read
the Application Insights resource), so it is scrubbed on the node before it
is queued and again on the hub - no command lines, environment, job output,
addresses or names in clear (names become keyed hashes salted with the
fleet's CA). The hub is an insider: it can turn telemetry on for the fleet,
but not for a machine whose owner turned it off (`[telemetry] enabled =
false` in longarmd.toml, `longarm telemetry off`, `LONGARM_TELEMETRY=0`) -
the worker's local policy wins, like `[access]`. Telemetry items of a user
never reach other users: they go node → hub → Application Insights only.

## Level 3 outlook

Untrusted users need more than accounts: a job per container or micro-VM
(Windows: Hyper-V isolated containers / sandboxes; Linux: user + network +
mount + PID namespaces, gVisor or Firecracker), per-job network policy
(loopback and LAN isolation, egress rules), per-user disk quotas, and an
image-based toolchain. Longarm's job model (shim per job, user helper,
per-user data spaces, hub-side policy) is the place to plug that in: the
shim would start the workload in a sandbox instead of a Job Object /
cgroup, and the user helper would run inside the user's sandbox.
