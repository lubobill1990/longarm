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
  for devices of a user, `?user=<user>` (`longarm://client/alice-laptop?user=alice`),
  plus the node's immutable ID (`urn:longarm:node:<uuid>`, see "Node
  identity").
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

## Node identity

Every node - worker, admin worker, client device, admin device, the holder
of a release certificate - has a unique, **immutable ID**: a random UUID
the hub assigns at enrollment (`internal/nodeid`). Its **name** is a
display name: it can change (`longarm worker rename`, `longarm admin
rename`, the panel) and, once the node is revoked, be used by a new node.
Authorization, revocation and the hub's records go by the ID.

**Certificates.** The identity URI SAN `longarm://<role>/<name>[?user=]`
stays (older binaries read it) and a second URI SAN carries the ID:
`urn:longarm:node:<uuid>`. CN and OU mirror name and role for humans;
workers keep the DNS SAN `<name>`. The name in a certificate is the
node's name when it was issued; the hub records it (`NodeInfo.cert_name`)
and clients dial a worker by it (TLS server name, longnet relay address)
while checking that the certificate carries the ID the hub reported - a
renamed node's old certificate keeps working under its old name, and no
other node can answer for it. Certificates issued before node IDs carry no
ID; the hub maps them to their node by **serial** (every certificate it
issued or saw is recorded with its node's ID), else - a hand-issued one it
never saw - by name among non-revoked nodes. They get the ID at their next
renewal, including workers' automatic renewal.

**Names never collide with IDs.** Two rules make every target string mean
one thing:

1. Names are **unique among non-revoked nodes** - enforced by the hub's
   database (a unique index over the names of non-revoked nodes) and
   checked at enrollment, sign-in, pairing and rename. A renamed node's old
   name stays reserved while its certificate still has it. Revoked nodes
   keep their names as history; they don't block reuse.
2. A name never **looks like an ID**: names made only of hex digits and
   dashes, 4 characters or more (`^[0-9a-fA-F-]{4,}$`), anything that parses
   as a UUID, and the reserved `id:` / `name:` forms are refused. A name
   derived from a host name that would break the rule gets a prefix: `w-`
   for workers (`1234` -> `w-1234`), `d-` for devices (a user `cafe`'s
   device: `d-cafe`).

**Targets.** Wherever a node is named - `longarm run|exec|session start|
attach|get|put|tail|forward|...`, `longarm worker share|label|remove|
rename`, `longarm admin approve|revoke|drain|label|rename|profile assign`,
port links, the panel's actions, MCP tools - the target is resolved to an ID:

| Target | Means |
|---|---|
| `id:<prefix>` | the node whose ID starts with the prefix (dashes and case ignored) |
| `name:<name>` | the node with that exact name |
| a full UUID | that node |
| hex digits and dashes, 4+ characters | an ID prefix (git-style; never a name, rule 2) |
| anything else | a name |

Among the nodes visible to the caller (others' own workers don't exist for
a user). A name means the non-revoked node of that name; with none, the one
revoked node of that name (dispatching to it fails: revoked). A prefix
needs at least 4 hex digits and must be unique: a prefix (or a name of
several revoked nodes) matching more than one node is an **error that
lists the candidates** - short ID, name, owner, state - and nothing is
ever guessed. With rules 1 and 2 a bare target is a name match or an
ID/prefix match, never both. On the client, `workers.toml` entries win for
names (as before), then the hub's list (or its cache).

**Output.** IDs appear next to names in their short form - the shortest
prefix that is unambiguous among the listed nodes, at least 8 characters
(`longarm ls`, `longarm worker ls`, `longarm admin nodes`, `longarm jobs`,
`longarm sessions`, the panel, the tray). JSON gains ID fields, additively:
`NodeInfo.id` / `cert_name` / `companion_id`, `FleetJob.worker_id`
(`jobs --json`), `workerId` (`sessions --json`), `PlaceUpdate.worker_id`,
`FleetEvent.node_id`, `PortLink.from_id` / `to_id`, `Whoami.id`,
`JoinResponse.id`, MCP `id` / `worker_id`.

**Revocation is by ID.** The revocation list workers get holds the revoked
certificates' serials (which covers certificates without an ID until they
expire) and the revoked nodes' IDs (any certificate carrying one is
refused, until the node's last certificate would have expired). Names are
never revoked: right after `longarm admin revoke box`, a new node may join
as `box` - with a new ID, which the old revocation doesn't touch.

**Renaming** (`longarm worker rename <worker> <new>` for owners, `longarm
admin rename <node> <new>` for admins, the panel's Rename) keeps the ID, so
jobs, shares, labels, port links and history stay with the node. It is
audited (`nodes/rename`). A worker that follows renames (longarmd with the
`node-id` capability) is asked at once to renew its certificate with the
new name, and restarts itself when a listener depends on the name (longnet);
older workers keep the old certificate name (clients reach them by it)
until they are updated. A device whose certificate named no user keeps
acting as the user of its old name.

**Admin worker pairing.** A worker and the admin worker enrolled with it by
one pairing are linked by ID (`NodeInfo.companion_id`), not by the
`-admin` name suffix; the migration links existing pairs (an admin worker
and the one worker of the same owner on the same host).

**End-to-end trust.** Endorsements bind the ID (see "End-to-end trust"):
an endorsement made for a node's request that carries its ID signs the ID
too (`sig2`, next to the old signature that peers before node IDs check),
and peers that know IDs hold it to the certificate's ID - it stays valid
through renames and can't be moved to another node. An endorsement made
before node IDs still binds the **name**: it stays valid for its node while
the node's certificate has that name, i.e. until a rename. Either way the
endorsed thing is the node key, and the certificate's TLS key is bound to
it: a new node that gets an old node's name (after a revocation) has
another node key, so the old node's endorsement doesn't verify for it -
there is no window where it inherits trust. With `require_workers`, a
worker the user targets **by name** must have been endorsed under that
name (the hub maps names to nodes, and end-to-end trust doesn't take its
word for it: a renamed worker needs a new endorsement, or a target by ID);
one targeted by ID or prefix must have an endorsement carrying a matching
ID. A device holding the user key that endorsed it re-endorses itself
under its ID after its first renewal with an ID.

**Migration** (hub database schema 4 -> 5, at the first start of a hub
with this version; one transaction):

- every node gets an ID; names stay as display names; the nodes table is
  keyed by ID with the unique index over non-revoked names;
- a name that breaks rule 2 is changed (`w-`/`d-` prefix, then `-2`,
  `-3`... if taken), with an audit record (`hub/migration`, `nodes/rename`)
  and a warning in the hub's log; duplicate non-revoked names (impossible
  before: the name was the key) would get a suffix the same way. The live
  hub's names (bolu-pair, bolu-pc, e2e-worker, tmia-spike,
  cpc-bolu1-nyg7h-admin, bolu1-cpc-bolu1-nyg7h, ...) pass unchanged;
- certificates and audit records get their node's ID (by the old name, the
  old key); the job cache is re-keyed by worker ID; port links get
  `from_id` / `to_id`; admin worker pairs get their companion link;
- existing certificates keep working: the hub maps them by serial, and they
  get the ID at their next renewal. Old clients and workers keep working:
  they send names, which the hub resolves; they ignore the new SAN.

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
| port forwards (per-user) | to loopback ports the user's own processes listen on | - | refused | any loopback port (`[forward] targets = "any"`: any address) |
| port links (`longarm link`, `fleet_link`): create, list, remove, use | own (as the user's account on both workers, per-user), between workers the user may use; both workers' own policies apply ([below](#port-links)) | - | not found; can't use the listener (per-user) | everybody's (own links: only between workers the admin's user may use) |
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
nowhere; logs are redacted. A job's **stdin payload** (`run --stdin-file`)
takes the same path - the submit's mTLS connection to the worker (the hub
never sees it; a longnet relay forwards only TLS records), longarmd's and
then the shim's memory, a pipe into the job - and is stored nowhere; job
records and the audit log carry its size only.
Unlike secrets it is not redacted from the job's output. A **forwarded git
credential** (`--git-auth forward`) takes that path too but stops at
longarmd: it never reaches the shim or the job
([forwarded git credentials](#forwarded-git-credentials)).

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
  keeps the full records. Port links: the link states the worker reports
  (`longarm link ls`) keep state, path kind and counters but no error
  texts (they quote addresses) and no endpoint; their audit lines keep the
  link id and owner - the hub created the link and knows its ends and
  port, so nothing about a link's target is new to it.
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
## Admin worker (Windows)

An **admin worker** is a second worker on the same machine that runs
commands with administrator rights: on Windows a `longarmd` running as
**LocalSystem** (the MSI's `ADMIN_WORKER=1`), so its owner can install
software and administer the machine remotely, also while nobody is signed
in. It is enrolled only through a pairing (`/link`) that carries its CSR
(`StartLinkRequest.admin_worker_csr_pem`, or kind `admin` alone) - with the
owner's Microsoft sign-in and approval; az sign-ins and join codes can't
make one. Its private key never leaves the LocalSystem longarmd: the
signed-in user's tray sends a CSR that longarmd made and hands the
certificate back. The `/link` page says, in a warning box, that the
pairing includes an **administrator** worker running commands as SYSTEM on
that machine, with its name (default `<worker>-admin`).

The hub marks it (`store.Node.Admin`, shown as the label
`longarm.io/admin=true`, `NodeInfo.admin_worker`) and enforces, whatever
the panel or the CLI does:

| Rule | Where |
|---|---|
| **Owner only, never shared**: `worker share` is refused for it (owner, `can_share` or admin alike); shares that reached its record anyway are ignored (`mayUse`), and the worker gets an access rule with the owner alone | hub (`ShareWorker`, `mayUseLocked`, `worker_access`) and the worker (owner/shares rule) |
| **Admin certificates get nothing**: a fleet admin who is not the owner is refused like any other user - hidden from users, `PermissionDenied` on the worker (owner-only means owner-only), and `--owner` (acting on another user's sessions) can't reach it either. Fleet admins still see it (with the `admin` marker), drain it and remove (revoke) it | worker (`ownerDeny`), hub (`relayCheck`) |
| **No hub-relayed commands**: the hub relays no submit, kill or logs to it, not even its owner's (panel, `longarm admin run|kill|logs`): commands go to it directly with the owner's certificate (`workers/denied` in the hub's audit) | hub (`relayCheck`) |
| **Never placed**: no `auto`, `--require` (even `--require longarm.io/admin=true`), scheduler queue, `session start auto`, MCP or client-side pick chooses it (`sched.Pick` rejects it as "admin worker (by name only)"): it is reachable by its name only | `sched.Pick` (hub and client) |
| **Reserved label**: `longarm.io/...` keys are the hub's - `worker label`, `admin label`, join codes and sign-in labels can't set or remove them (`PermissionDenied`), and a worker's own `[hub] labels` with such keys are dropped | hub (`SetLabels`, `CreateJoinCode`, `StartLink`, hello) |

End-to-end trust applies as for every owned worker (pin the owner on the
machine: `[access] users/owners`, `longarmd trust add-user`), and every
exec, run, session, file put/get and tail on it is in the worker's audit
log as on any worker. The session worker of the same machine is an
independent node: it has no way into the admin worker's local API (a
LocalSystem-only channel) and gets no rights over it.

**On the machine** (the MSI, `install-helper admin-setup`): the admin
worker is the service `longarmd-admin` (LocalSystem; no password, so it
works on Entra-only machines) with its own data dir
`C:\ProgramData\Longarm\longarmd-admin` (SYSTEM and Administrators only)
and its own listener (127.0.0.1:7702). While it waits for its sign-in it
makes its key and CSR itself and leaves only the CSR in the hand-off dir
`C:\ProgramData\Longarm\admin-signin`, whose ACL is SYSTEM, Administrators
and the installing user (who is an administrator anyway: no other local
user can pair it as theirs or hand it a certificate). The tray adds the CSR
to the user's pairing and puts the approved enrollment back there; the
service installs it only if the certificate is for its own key, chains to
the hub's CA (the pinned one, when the hub was pinned) and carries
`longarm.io/admin=true` (an older hub's plain worker certificate is
refused), and then writes `[access] users = ["<owner>"]`, `owners =
["<owner>"]`, `admins = "none"` into its `longarmd.toml` - owner only on
the worker too, whatever the hub says later. It can be removed without
reinstalling: the tray's "Disable admin worker" (or `longarmd admin-worker
disable`, elevated) makes it leave the hub (its certificate is revoked),
removes its service and data, and records `AdminWorker=0` so upgrades keep
it off; uninstalling the MSI or upgrading with `ADMIN_WORKER=0` does the
same.

**Corporate-managed devices**: a LocalSystem service that runs remote
commands is what endpoint protection watches for. Microsoft Defender for
Endpoint, Intune compliance or attack-surface-reduction rules may flag,
block or report it; check with whoever manages the device before turning
it on, and expect the commands it runs to be attributed to SYSTEM in their
logs.

## Installer (Windows MSI)

The MSI is per-machine: its custom actions run elevated, as SYSTEM, with
values from MSI properties - and Windows lets a non-administrator run a
**repair** of a per-machine product elevated, with public properties of
their choosing. So the installer treats every property as untrusted input:

- **Values are data, never options.** Windows Installer formats properties
  into a custom action's command line unquoted. Every `longarmd
  install-helper` action passes `install-helper msi <step>
  #LONGARM-MSI#key=[PROPERTY]|key=[PROPERTY]...`; the helper takes the text
  after the first marker verbatim from the raw command line and parses it
  strictly (`longarmd.MSIData`): exactly the step's keys, each once (the
  template has them all, so an injected `|key=...` is a duplicate), each
  value against its own pattern - HUB is a host name with an optional
  port, SESSION_SID a SID of an existing user account, LABELS / LISTEN /
  JOIN_CODE / CA_FINGERPRINT / NODE_NAME their formats, no quotes, spaces
  or separators. `HUB='x" --exe "C:\...\evil.exe" --hub "x'` is refused,
  not parsed. CI (`TestMSICustomActionData`, `msi-check.ps1`) checks that
  every action passes exactly the keys its step expects.
- **No property names a program.** The helper runs, and registers as
  services and scheduled tasks, only the binaries next to itself, and
  first checks that their directory (and the files) can be changed by
  SYSTEM, Administrators and TrustedInstaller only (Program Files); an
  install directory a user can write is refused. The admin worker's service
  name and directories are fixed; a service data dir (`DATADIR`) must be
  below `%ProgramData%\Longarm`. The helper's log is
  `%ProgramData%\Longarm\install-helper.log` (that dir secured first, an
  old log replaced, never followed), not a user's temp dir.
- **Who asked.** The actions get `UserSID` - the account that started the
  install, a private property the command line can't set - and check its
  membership in the local Administrators group (directly, as Entra
  accounts on a Dev Box, or through domain groups; LocalSystem for
  deployment tools): a **repair or change** of the installed product is
  refused for non-administrators before anything else runs (the
  `LongarmdCheck` action right after `InstallInitialize`, so nothing is
  written - conditioned on the Settings component's action state, not on
  the public REMOVE property, so `REMOVE=ALL REINSTALL=ALL` doesn't skip
  it; only a real uninstall does); the service worker and the admin worker need an administrator
  (and the admin worker's sign-in hand-off goes to that administrator
  only); the user-session task and the tray's start are for the requesting
  user - another user's only for an administrator. (`AdminUser` /
  `Privileged` aren't used: under UAC they don't tell an administrator from
  a user an elevated repair runs for.)
- **The sign-in defaults** (Hub, CaFingerprint, Login, Labels, Mode in
  `HKLM\Software\Longarm`: where sign-ins without a hub go and which CA
  they pin) are written by the helper, not the MSI's Registry table: values
  validated, and a change of recorded ones only for an administrator
  (`ApplyMSISettings`) - a second layer behind `LongarmdCheck`.
- **Role changes from the tray** (`longarmd install-helper roles
  #LONGARM-MSI#client=1|worker=1|admin=|sid=S-...`, started elevated by the
  tray's Account tab through UAC) follow the same rules: the data is parsed
  by `MSIData` (each key once, `1` or empty, a SID); who asks is the
  process's own token, which must be an elevated administrator; the
  user-session task is the given (existing) user's, binaries only from the
  helper's protected directory; the admin worker needs the worker role, an
  administrator to sign it in, and takes its hub and CA pin from what the
  installer recorded in HKLM (never from the command line). It changes the
  task / service and the installer's record (Roles, SessionUserSID,
  AdminWorker) together, so uninstall still removes all of it; its result
  goes to `HKLM\Software\Longarm\RolesResult` (readable by the tray, not
  writable by users).
- **Task definitions** are XML built with `encoding/xml` from a SID checked
  against its pattern and the account database; the program is the
  installed tray / longarmd.
- **Planted data dirs.** Any user may create directories in ProgramData. A
  machine-wide data dir that exists before the install is kept only when
  the installer recorded creating it (`HKLM\Software\Longarm\Provenance`,
  written by the helper as SYSTEM - not one of the MSI's own values, which
  a repair rewrites from properties) and nobody but SYSTEM /
  Administrators (and the service's own account) can write it; otherwise
  it is moved aside and removed (reparse points removed, never followed)
  and created fresh with a protected ACL before anything is written. Older
  installs' service data dirs pass when they are protected. The admin
  worker's dir needs the record; its sign-in hand-off dir starts empty at
  every setup.
- **The admin worker serves only what its sign-in made.** It pins the hub's
  CA itself (`https://<hub>/ca.crt`, checked with the system's roots, or
  the given fingerprint) before it hands out its CSR; it installs only a
  certificate for its own key, from that CA, marked `longarm.io/admin=true`;
  it records that CA in the provenance key; at every start it refuses to
  serve when `tls/ca.crt` isn't the recorded CA (planted credentials have no
  record).
- **The hub can't make it act.** The admin worker ignores what the hub
  starts by itself - desired toolchains (package operations), update pushes
  (it updates with the MSI only: its self-updater is off), port links,
  relayed job requests (submit, kill, logs, toolchain apply) and data
  purges - and the hub refuses toolchain profiles for it, skips it in
  profile and release rollouts and never offers it a version. Revocations,
  its access list and drains (which only narrow) and the tailnet transport
  still apply.
- **Local surface.** Its listener (127.0.0.1:7702) is TLS 1.3 with a
  client certificate required and verified against the recorded hub CA;
  owner-only `[access]` on top. Its local control API is a named pipe whose
  ACL is SYSTEM only (the service's account); its data dir is SYSTEM and
  Administrators only; the hand-off dir is SYSTEM, Administrators and the
  installing administrator. It has no other endpoint (no health or metrics
  port). A user who creates the pipe's name first can only keep the local
  API from starting (a denial of service, logged).

## Port links

Port links (`longarm link`, [usage](usage.md#port-links-worker-to-worker))
let a user's processes on worker A reach a loopback port of worker B. They
are the only way one worker talks to another; workers get **no control
rights** on each other.

**How a link is authorized.**

1. A user (or admin) creates it on the hub; the owner is the caller. The
   hub checks that both are approved workers, the `--job` (if any) is the
   caller's and running, the quota (`[links] max_per_user`) and that the
   listen port is free on A; it stores the link and audits it.
2. The hub signs a **grant** - link id, owner, A, B, target port, listen
   port, expiry - with the private key of its hub certificate and attaches
   that certificate. The hub certificate is issued by the fleet CA, which
   every worker already trusts, so workers verify grants without any new
   key distribution.
3. The hub pushes every worker the **full list** of links naming it on the
   worker's own hub session (no new RPC for workers on the hub). The list
   is authoritative: a link that is not in it is revoked.
4. For each accepted local connection A calls `LinkTunnel.Dial` on B with
   its worker certificate and the grant. This is the **only** RPC a worker
   certificate may call on a worker. B accepts it only if the grant's
   signature chains to the fleet CA with the `hub` role, the grant names
   the calling worker (mTLS identity) as A and B itself as the target, it
   has not expired, and the link is still in B's list with the same owner,
   source and port. B then applies `Forward.Open`'s checks for the owner -
   `[forward] targets`, and with per-user isolation: a process of the
   owner's account (and nobody else's) listens on the port - and connects
   to `127.0.0.1:<port>`. Every acceptance and refusal is in B's audit log
   (forwarded to the hub).
5. On A, with per-user isolation, longarmd accepts a connection to the
   listener only from a process of the owner's account (Linux: the uid of
   the client socket in `/proc/net/tcp{,6}`; Windows: the owning process of
   the client socket (`GetExtendedTcpTable`) and its token's user). macOS
   workers have no per-user isolation; in shared isolation any local
   process may use the listener. The listener binds both 127.0.0.1 and ::1, and
   jobs only get the link's variables while it is bound, so another
   account can't catch the owner's traffic on a free twin port. With
   users separated, only admins may link a shared-isolation source to a
   per-user target; the hub re-checks this whenever separation or a
   worker's isolation changes (and at start) and removes links no longer
   allowed.

**Revocation.** Removing a link (or its TTL, its job ending, its owner being
removed, either worker being revoked) pushes new lists to both workers;
both close the listener and cut every open connection through it, and the
hub stops relaying between the two workers when no link remains. Workers
also expire links themselves, so a link ends at its TTL even when the hub
is unreachable, and the source worker ends a link bound to a job when the
job ends there (the job always runs on the source). Workers keep the last
list across restarts (`<data>/port-links.json`) so links work while the
hub is down; a link removed during a hub outage ends when the hub is back
or at its TTL (default 8h, `[links] max_ttl`). Disabling the owner cuts
the open connections on both workers (the target refuses new ones). Each
worker caps a link's open connections (`[links] max_conns` in
longarmd.toml, default 64).

**What a compromised worker can do** (it holds its worker key and can run
anything as longarmd):

- As **A**: reach exactly the target ports of links that name it as their
  source, as their owners could anyway (B checks the owner's port there),
  until the links end. It cannot forge or alter grants (the hub's key), use
  another worker's grants (B checks the mTLS identity), reach other ports
  of B, or call anything else on B.
- As **B**: see the traffic of links that target it (it is the endpoint)
  and learn the ids and owners of those links.
- It **cannot**: create, list or remove links (client and admin
  certificates only), use links between other workers, or make the hub's
  relay carry its traffic to workers it has no link with.

**The workers' own policies win.** A grant is the hub's permission, and
like everything the hub sends it can only narrow what a worker allows,
never widen it (as for the [worker-side access
policy](#worker-side-access-policy), [user-owned
workers](#user-owned-workers) and [end-to-end
trust](#end-to-end-trust-the-hub-is-not-trusted-with-access)). Both
workers apply their local policy to the **link's owner**, on every
connection:

| Local policy (on A and on B) | A link of owner *u* works only if |
|---|---|
| tenancy policy | *u* is not disabled (disabling cuts open connections) |
| `[access] users` / `owners` | *u* is a user of the worker (`admins` does not apply: a link is a user's forward, like `Forward.Open` with a client certificate) |
| user-owned worker (owner, shares) | *u* is its owner or it is shared with *u* (by name, team, everyone); an unshare cuts open connections, and the hub removes the link |
| end-to-end trust (`trust.toml` with users) | *u*'s user key is pinned on the worker, **the link carries *u*'s signed consent** (below), and the other worker of the link is endorsed by *u*'s key |

- **The owner's consent (end-to-end trust).** A grant is the hub's word,
  and with end-to-end trust the hub must not be able to grant access. So
  `longarm link create` (and MCP `fleet_link`) on a device endorsed as a
  device of *u* signs the link's spec - link id, owner, A, B, port, listen
  port, expiry, job - with the device's node key and attaches the device's
  endorsement. The hub stores it with the link (it takes id and expiry
  from it, and checks it is well-formed and the caller's) and pushes it
  with the link to both workers. A worker that trusts users accepts the
  link only if the consent's endorsement is by the key it pins for *u*
  (not a denied node), the device's signature over the spec is valid, and
  the link matches the spec exactly (it may end earlier, never later) - B
  checks it on every `LinkTunnel.Dial`, A before it listens and on every
  local connection. A link without consent (created on a device that is
  not endorsed, or made up by the hub) is refused there: `link ls` shows
  "no owner's consent". Workers without trust keep accepting the hub's
  grant alone - there the hub is trusted by design.
- A worker certificate is not a user: on B it passes the role table only
  for `LinkTunnel.Dial`, and `[access]` and the owner/shares rule are then
  applied to the grant's owner instead of the calling worker. With
  end-to-end trust the TLS handshake already refuses a calling worker that
  no trusted user endorsed (`pki.TrustPolicy.Check` for a worker
  certificate: endorsed as a worker, under its name, by a pinned key).
  Links demand more - the endorsing user must be the link's owner - so a
  hub can't pair alice's port on B with a worker that alice didn't
  endorse (bob's machine, or a worker it minted), even if B trusts bob.
- A refuses the same way: no listener while its policy refuses the owner
  (`link ls` shows the link as `error`, "refused by this worker's policy"),
  local connections refused (and audited, `link/refused`) once it does,
  and, with end-to-end trust, no grant and no byte to a target that the
  owner's key did not endorse.
- The hub applies the owner/shares rule too: a link's owner must be
  allowed to use both workers when it is created (otherwise "no worker",
  for admins as well), and the hub removes links whose owner loses one of
  them (unshare, team change).
- With end-to-end trust, link both ends to workers the owner endorsed
  (`longarm trust endorse` of each worker's request). A worker endorsed by
  somebody else (a fleet worker endorsed by its machine's owner) can't be
  an end of the owner's links while trust is on there.

**What stays with the hub**: without end-to-end trust a compromised hub
or admin is trusted (as before) - a grant from a hub certificate of the
fleet CA is accepted by every worker of that CA, within the workers'
`[access]` and owner rules. With trust the hub can't create a link: it can
withhold or remove one, and replay a consent the owner gave - re-create
the same link (same id, ends, ports) until the consent's expiry, after the
owner removed it. Keep `--ttl` short for links you don't need long.

**Shared-isolation sources.** In shared isolation every local process
can connect to A's listener, so a link from a shared worker that other
users also run jobs on exposes the target port to them. A doesn't refuse
such links on its own: what it knows locally can't prove that nobody
else uses the machine (with `[access] users` unset everyone the hub
admits may; the users it has seen so far say nothing about tomorrow), and
under end-to-end trust the link exists only because its owner signed
it from her device, naming A. When users are separated, the hub refuses
non-admins a link from a shared-isolation worker to a per-user one; with
trust, that check is the hub's word and the owner's consent is what
counts. Link only from workers where you are the only user (per-user
isolation, or `[access] users` listing just you), or accept that the
others on A can reach the port.

**Unverified streams.** A worker certificate reaches `LinkTunnel.Dial`
before its grant is checked; B gives each such stream 10 s for its first
frame (the grant) and allows 16 of them per calling worker at a time
(`ResourceExhausted` beyond). The grant's hub certificate is checked
against B's certificate revocation list too.

**Network policy.** Tailnets need worker → worker on tcp:7701 for direct
paths ([deploy/hub/README.md](../deploy/hub/README.md)); without it links
use the hub's longnet relay. The relay forwards worker ↔ worker packets
only for pairs with an active link (`longarm-relay` run standalone never
does).

## Forwarded git credentials

`longarm run / sync --git-auth forward` lets a worker fetch a repository
from its host (Azure DevOps, GitHub, any https git server) with the user's
own credential, instead of a credential stored on the worker or shipping
the history from the client ([docs/usage.md](usage.md#fetching-with-your-credential---git-auth-forward)).

**What is forwarded, and for how long it is good.**

- Azure DevOps: an Entra access token whose audience is Azure DevOps only
  (`499b84ac-1321-427f-aa17-267ca6975798`), from `az account
  get-access-token` in the organization's tenant (discovered from the
  `X-VSS-ResourceTenant` header of an unauthenticated request to the
  organization URL - no credential is sent there), or what Git Credential
  Manager hands to `git credential fill`. It is valid for about an hour and
  carries the user's Azure DevOps rights (every organization of that
  tenant the user can reach), not only the one repository.
- GitHub: `gh auth token` - gh's OAuth token or a PAT, **long-lived** (valid
  until revoked) and with the scopes the user gave gh. Forwarding it trusts
  the worker with it beyond the job; prefer a fine-grained, read-only PAT
  for gh on machines whose workers you don't control, or use `--repo .`
  without `--git-auth`.
- Other hosts: whatever the user's credential helper returns (often a
  long-lived password or PAT).

**Who sees it.**

- The client: gets it from az / gh / git (through pipes; never in argv, a
  file or longarm's output) and keeps it in memory until it was sent. A
  queued `run` holds it while it waits for placement.
- The hub: **never**. It is only in the worker-direct requests
  (`SubmitRequest.git_auth`, `Workspace.Fetch`), inside the end-to-end mTLS
  connection to the worker (a longnet relay forwards only TLS records).
  The hub's index and its audit trail get the job spec, which says
  `git_auth: true` and nothing else. With [end-to-end trust](#end-to-end-trust-the-hub-is-not-trusted-with-access)
  the client also verifies that the worker is one the user endorsed before
  sending it.
- The worker: longarmd (and, with per-user isolation, the user's helper
  process, over its stdin pipe) holds it in memory during the fetch. git
  and git-lfs get it from a credential helper (`longarmd
  git-credential-helper`) that asks longarmd over a loopback connection
  with a random one-time key (in the helper's environment; worthless once
  the fetch is over: the listener closes), for the exact protocol, host
  and repository path only - it is never in git's or git-lfs's
  environment, command line or config, which git-lfs copies into
  `lfs/logs` on a fatal error. longarmd then zeroes its copy (best effort:
  the Go runtime, gRPC and the helper's output pipe may hold transient
  copies until the memory is reused). It is never written to disk: not job.json, the status, the
  mirror's or worktree's git config, `FETCH_HEAD`, remote URLs, the audit
  log (`git_auth: true` only), longarmd's log or the job's out.log
  (Authorization header values and the credential are masked in git's
  output and in failure reasons).
- The job: **never** - not in its environment, its stdin, its worktree's
  git config or remotes. A job that wants to fetch more needs its own
  credential (e.g. `--secret-env`).
- The worker's local administrators (and root / SYSTEM) can read
  longarmd's memory and its children's environment while the fetch runs:
  forward a credential only to workers whose administrators you trust with
  it - for an Azure DevOps token for up to an hour, for a GitHub token
  until it is revoked.

**Scope on the wire.** The credential is answered only for the fetched
URL: protocol (https), host with port and repository path (and its `.git`
twin, git-lfs's endpoint) must match exactly (`credential.useHttpPath`;
longarmd checks, not only git's config matching). So:

- **Redirects** are refused for the whole fetch (`http.followRedirects=false`
  for git; git-lfs asks the helper per URL, and another host or http gets
  nothing): the fetch fails with "the remote redirected ... use the final
  URL". (A URL-scoped `http.extraHeader`, the first design, was replayed by
  git to a redirect target - another host, or http - on the requests after
  the redirected discovery.)
- **The LFS endpoint** is pinned to the fetched URL's (`lfs.url` in the
  fetch's config, which wins over a repository's `.lfsconfig` `lfs.url` /
  `remote.*.lfsurl`); LFS downloads on a storage host get their own
  short-lived URLs from the LFS API, not the credential.
- **Submodules** are never fetched (`--no-recurse-submodules`; nothing
  recurses into them).
- **Traces** are off for every git and git-lfs run of the worker (fetch,
  ls-remote, worktree, checkout, LFS): `GIT_TRACE*`, `GIT_CURL_VERBOSE` and
  `GIT_TRANSFER_TRACE` from longarmd's environment are removed (by name,
  ignoring case), never set to `0` - git turns curl tracing on when
  `GIT_CURL_VERBOSE` is set at all, which put the remote's response
  headers into job logs in the first release of this feature; trace2 is
  forced off (`GIT_TRACE2*=0`, its targets can come from git config);
  `GIT_TRACE_REDACT=1`. Lines with `Authorization`, `Proxy-Authorization`,
  `Cookie` or `Set-Cookie` headers are dropped from everything forwarded
  (the job's log, errors, failure reasons), and bearer / basic tokens are
  masked anywhere. `[workspace] git_trace = true` in longarmd.toml keeps
  the inherited trace variables for debugging (the redaction and the line
  filter stay). `<mirror>/lfs/logs` is removed after an LFS fetch.
- Only https URLs get it (the client refuses http, the worker too; a job's
  `GH_TOKEN` keeps working over http as before, also scoped to the exact
  URL now).
- The worker's credential helpers are switched off for that fetch: a
  refused credential fails the fetch instead of falling back to the worker
  account's own credentials, and no helper stores the forwarded one.

**Shared mirrors.** In shared isolation the workspace mirror is shared by
the worker's users: a commit a job fetched with user A's forwarded
credential stays in the mirror. A later job that names such a commit
(`--repo <url> --ref <sha>`) does not fetch it again, but it is not checked
out on A's authority either: the job first proves its **own** access to the
remote - one `git ls-remote <url> HEAD` (protocol v2 lists that ref only; no
objects) with the job's forwarded credential, else its `GH_TOKEN` secret,
else the worker account's own credentials, exactly what a fetch would have
used. Refused: the job fails (exit code 126) like a refused fetch (`the
forwarded credential was refused` / `the worker could not authenticate`).
`Workspace.Fetch` (remote-first shipping) checks the same way when every
requested base commit is present already; refused, the client ships what
the worker lacks itself (it has those objects anyway). Jobs without
forwarding keep using the worker's own credentials, as before this
feature: a remote the worker account can read is readable for every user
of a shared worker. Per-user isolation keeps mirrors per user and skips the
check (a user's mirror only ever holds what that user fetched or shipped).

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
  account's files - including other users' worktrees and logs - and every
  local process can use every port link listening on that worker. (Port
  forwards reach only the worker's loopback ports unless `[forward] targets
  = "any"`; before that setting existed they reached any address the worker
  could reach - see "Upgrading: forward targets" under Operations.) Use it
  only for people who trust each other.
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

Port links are in the same chained log on both workers: `link/added`
(each link the hub's list brings, with owner, ends, port and expiry;
`"saved": true` when restored from `port-links.json` at start) and
`link/ended` (with the reason) on both; every `LinkTunnel/Dial` on the
target (accepted with the target, or denied with why); `link/refused` on
the source for local connections its policy or per-user check refused.

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
- **Both roles, one approval, said so**: a pairing of kind `both` (one
  machine as the approver's client device and own worker: `longarmd login
  --role both`, the install scripts' `--role both`, the tray) carries two
  CSRs, longarm's key and longarmd's, each with its own node key. The
  confirmation says in bold that approving does two things - a device
  that acts as you, and a worker your jobs may be placed on - and lists
  both names and both node key IDs; both warnings apply. The approval
  enrolls the worker first, then the device; if the device fails the
  worker is revoked again, so the machine never collects one half. The
  result (both certificates) is handed out once, and only while both
  nodes are still approved.
- Audited on the hub: `link/start`, `link/approve` (with the node or the
  error; kind both: the worker too), `link/deny`, `link/expire` - who,
  kind, name, host, address.

The poll token, the code and the CSR are not secrets an observer can turn
into a certificate: the certificate is useless without the machine's
private key, which never leaves it. The hub's CA is pinned (`--ca-fingerprint`,
built into the install scripts, or bootstrapped over HTTPS: "First contact"
below): pairing runs over the same TLS as join.
**End-to-end trust is still recommended**: a pairing approved by mistake
(or an Entra account takeover) yields at most a certificate the hub signed;
workers and clients that require endorsements still refuse the device until
your user key endorses it.

### First contact (domain-only sign-in)

The hub's gRPC port presents a certificate of the hub's own CA, which no
system trusts; a node must pin that CA before it sends a CSR or a token.
`--ca-fingerprint` (and the install scripts, which carry it) pins it
explicitly. `longarm login <domain>` / `longarmd login <domain>` without it
**bootstrap the pin over the web PKI**: they fetch
`https://<domain>/ca.crt` (and the gRPC address from
`https://<domain>/.well-known/longarm-hub`), verifying the panel's HTTPS
certificate (Let's Encrypt, `[https]` or the reverse proxy's) with the
system's normal roots, and pin the CA's SHA-256 fingerprint exactly as
`--ca-fingerprint` would; the gRPC connection then refuses any other CA.
The fingerprint is printed.

**Trust model**: for this first contact the machine trusts DNS and the web
PKI for the domain the user typed - the same trust as opening the panel in
a browser to approve the sign-in, which every Microsoft sign-in needs
anyway. Whoever can obtain a publicly trusted certificate for that domain
(or controls the TLS front of the panel) can hand out another CA, exactly as
they could serve a fake panel. Guarded against: plain HTTP (an `http://`
hub or a panel answering HTTP on the https URL is refused), redirects
leaving https or the host, answers that are not one PEM CA certificate,
and a well-known document naming another CA than `/ca.crt` serves. After
the first contact nothing depends on the web PKI: the pinned CA is stored
with the certificate (`tls/ca.crt`), renewals and later sign-ins to the
same hub use it. Use `--ca-fingerprint` (from `longarm-hub ca show`, over a
channel you trust) where DNS or the web PKI is not trusted, and for hubs
without a public HTTPS panel, which fail with a message asking for it.
`longarm join` / `longarmd join` with join codes keep their behavior
(`--ca-fingerprint`, else trust on first use with a warning).

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
  `device`), its name, the user and - for requests that carry it - the
  node's ID ("Node identity"). Nodes put it into their CSRs; the hub
  checks the binding and copies it unchanged into the certificate (join and
  every renewal).
- A worker with trusted users in `<data>/trust/trust.toml` checks every
  client certificate in the TLS handshake: endorsed by the pinned key of the
  certificate's user, as a device, under the certificate's node ID (an
  endorsement from before node IDs: under its name), not a denied node key. A client with `require_workers = true` checks workers the
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
- **Upgrading: forward targets (breaking).** Since `[forward] targets`
  exists (the release with port links), forwards - `longarm forward`, ssh
  `-L`/`-D` through the built-in sshd, port links - reach only the worker's
  own loopback ports by default, for every caller including admins. Shared
  workers used to forward to any host:port they could reach; a deployment
  relying on that (forwards to LAN hosts) needs `[forward] targets = "any"`
  in longarmd.toml after the upgrade. longarmd logs a warning naming that
  setting the first time it refuses a non-loopback target. See
  [CHANGELOG.md](../CHANGELOG.md#upgrading).

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
