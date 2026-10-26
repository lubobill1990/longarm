# TENANCY report: multi-tenancy level 2

Semi-trusted users share one hub and its workers without data leaks.
Branch `tenancy`, PR #3 against `main`. Threat model, what is enforced,
platform details and known gaps: [security.md](security.md).

## Done

| Area | What | Where |
|---|---|---|
| Identity | user in the certificate (`longarm://client/<device>?user=<user>`); owner = user, else the certificate name (older certificates keep working); `longarm admin issue --user`, join codes bound to a user | `internal/pki`, `internal/hub/enroll.go` |
| Ownership (worker API) | every job and session carries an owner; Jobs Get/List/Logs/Wait/Kill, Sessions List/Attach/Kill/Logs, ids, Info, Metrics (no process lists for non-admins), disabled users refused; team-shared jobs read-only; admins act on all (`--owner` for sessions); enforced with per-user isolation or when the hub separates users, unchanged otherwise | `internal/server/tenancy.go`, `server.go`, `sessions.go`, `auth.go` |
| Ownership (hub) | job index, GetJob/WaitJob, event streams (per-subscriber filter), node list (others' devices, process lists and mirrors hidden), Place (owner, quota), Whoami (user, teams, quota, usage); audit, users, teams, links admin-only; panel: admin vs user logins ("My jobs", read-only nodes, "Me"), relayed kill/logs checked | `internal/hub/tenancy.go`, `fleet.go`, `events.go`, `schedq.go`, `http.go`, `panel.go`, `ui.go` |
| Teams | `longarm admin team add|rm|ls`, `longarm run --share team|team:<name>` (read-only for members) | `internal/tenancy`, `cmd/longarm/users.go` |
| Users | `longarm admin user add|code|set|ls|disable|enable|remove [--purge]`, quotas per user, panel logins (`--panel`), `longarm whoami`; disable/remove close the user's open connections; purge deletes the user's data and OS account on every worker (also workers that reconnect later) | `internal/hub/tenancy.go`, `cmd/longarm/users.go`, `internal/longarmd/hubagent.go` |
| Policy push | users, teams, quotas, disabled/purged users → workers (`tenancy.json`), versioned, re-sent on connect | `internal/hub/tenancy.go`, `internal/tenancy` |
| Per-user isolation (Windows) | longarmd as LocalSystem; local accounts `la-<user>` (random password, batch logon only, interactive/RDP/network/service logon denied); S4U logon + `CreateProcessAsUser` (handle list, own window station, suspended → Job Object); ConPTY sessions; profile held by longarmd; CPU-rate cap | `internal/platform/windows/users.go`, `startas.go`, `container.go`, `pty.go` |
| Per-user isolation (Linux) | longarmd as root; `useradd` accounts; setuid/setgid without supplementary groups; cgroup v2 per job (`CLONE_INTO_CGROUP`, memory.max, cpu.max, cgroup.kill); pty handed to the account; umask 077 | `internal/platform/linux/*.go`, `internal/platform/unix/*.go` |
| macOS | `mode = "per-user"` refused with a clear error | `internal/platform/isolation_darwin.go` |
| Data layout | `<data>/users/<account>/{jobs,sessions}` privileged and private, `{mirrors,worktrees,staging,tmp}` owned by the account and private; data dir locked down (traverse only for users); legacy shared data admin-only | `internal/userspace`, `internal/platform/*/securedir*` |
| Files / Workspace / git / sftp | done by a per-request helper (`longarmd user-helper`, gRPC over stdio) or `longarmd sftp-server` running as the user; job preparation and worktree pruning too | `internal/server/userhelper.go`, `internal/userspace/helper.go`, `internal/longarmd/peruser.go` |
| Exec, sshd | as the user's account (environment, home, cgroup); direct ssh listeners ignored in per-user mode | `internal/server/exec.go`, `internal/sshd` |
| Forwards | per-user: loopback ports listened on by the caller's own processes only (Windows owning PID → token user; Linux `/proc/net/tcp{,6}` uid); also ssh direct-tcpip | `internal/server/tenancy.go`, `internal/platform/*/ports*` |
| Loopback (Linux) | nftables table `inet longarm` (`[isolation] loopback = "isolate"` default, `require`, `allow`): the sender account is recorded in the conntrack mark, the listener's SYN-ACK / UDP reply from another account is dropped, the retried SYN reset; updated on account create/purge, removed by `leave --purge`/uninstall | `internal/loopguard`, `internal/userspace` |
| Quotas | fleet-wide (hub scheduler: queue or fail), per worker (jobs, memory, sessions), per-job memory/CPU defaults and caps (Job Object / cgroup) | `internal/hub/tenancy.go`, `internal/server/tenancy.go`, `internal/tenancy` |
| Toolchain | machine-wide in per-user mode (winget `--scope machine` incl. LocalSystem's winget path, npm globals next to node on Windows, uv in /usr/local/bin) | `internal/toolchain/backends.go` |
| Installers | MSI `ISOLATION=per-user`, deb `LONGARM_ISOLATION=per-user` + `longarmd-peruser.service`, `longarmd service install --isolation per-user`; `longarmd leave --purge`/uninstall delete the accounts | `packaging/`, `cmd/longarmd`, `internal/longarmd/leave.go` |
| Panel UI | People (users, teams, quotas, devices, join codes + install lines, panel logins, disable, remove/purge), Me, My jobs, owner columns, read-only nodes for users | `web/src/panel/*`, `internal/webui/dist` |
| Docs | `docs/security.md`, README "Sharing a hub with your team" (en + zh), usage reference, design notes | `docs/`, `README*.md` |
| Fixes on the way | Windows service mode logged nothing to `longarmd.log` (stderr write error stopped `io.MultiWriter`); startup errors of the service are now logged | `internal/longarmd/longarmd.go` |

## Build, run, test

```sh
go build ./...                                   # Go >= 1.27.3 (GOTOOLCHAIN=auto)
go vet ./...                                     # + GOOS/GOARCH for windows|linux|darwin x amd64|arm64
go test ./...                                    # unit + e2e (per-user OS tests skip)
LONGARM_PERUSER_TEST=1 go test -run PerUser -v ./e2e      # Windows, elevated: temporary LocalSystem service + accounts
# Linux, as root (what CI does): prebuilt binaries, test binary as root
for c in longarmd longarm longarm-hub; do go build -o bin/$c ./cmd/$c; done; go build -o bin/helper ./e2e/helper
go test -c -o bin/e2e.test ./e2e && (cd e2e && sudo env LONGARM_PERUSER_TEST=1 LONGARM_E2E_BIN_DIR=$PWD/../bin ../bin/e2e.test -test.run PerUser -test.v)
node web/build.mjs                               # panel (pnpm); cd web && pnpm test
pwsh packaging/windows/build-msi.ps1 -BinDir <dir> -Version 0.0.1 -Arch x64 -Out longarm.msi
```

Try it: `longarm admin user add alice --team web --panel`, `longarm join
<hub> <code>` on alice's machine, a worker with `[isolation] mode =
"per-user"` (as LocalSystem / root), then `longarm run -- whoami` as alice
prints `la-alice`.

## Tests

New tests:

- `internal/server/authz_test.go` - **every worker RPC × {owner, other user, team member, admin}** (the table must cover every method in the role table), disabled users, the one-person fleet unchanged (`TestNoTenancyUnchanged`).
- `internal/hub/tenancy_test.go` - client role table over every hub RPC; filtered job index, GetJob/WaitJob, node list, events; admin-only user/team/audit RPCs; Whoami; fleet-wide quota in placement; disabling, removing with purge, policy; user-bound join codes; panel roles over HTTP.
- `internal/tenancy/tenancy_test.go`, `internal/pki` (user identities).
- `e2e/peruser_test.go` - **two users (alice, bob) + a team member (carol) + an admin against one hub and one per-user worker** (Windows: temporary LocalSystem service; Linux: root): accounts per user; bob can't list/get/read/wait/kill alice's jobs or list/attach/kill/read her sessions; bob's Files API can't read, list or write in alice's home; from inside bob's own processes: alice's home, space, worktrees, mirrors, job log, the worker's TLS key, the users dir listing and alice's job's process environment are all refused (positive control: alice reads her own process's environment incl. the secret); forwards: alice reaches her dev server, bob is refused; carol reads the team-shared job but can't kill it and can't see the private one; bob's per-worker quota and job-id squatting; hub views of bob and carol; bob can't read the audit; per-user mirrors (bob's mirror doesn't have alice's repository); Linux cgroup of the job; admin kill; `user remove bob --purge` deletes bob's account and data on the worker and his certificate stops working. `TestPerUserRefusedOnMacOS` (macOS runner).
- The whole existing suite (shared mode) passes unchanged.

Results (commit `9de7153` and before):

| Where | Result |
|---|---|
| CI ubuntu-latest | `go test ./...` ok; **per-user e2e as root: `--- PASS: TestPerUserIsolation (30.76s)`** |
| CI windows-latest | `go test ./...` ok; **per-user e2e as administrator: `--- PASS: TestPerUserIsolation (92.00s)`** |
| CI macos-latest | `go test ./...` ok (incl. `TestPerUserRefusedOnMacOS`) |
| CI lint | privacy, gitleaks, gofmt, vet + build for 6 targets, shellcheck, actionlint ok |
| this machine (Windows 11, elevated) | `go test ./...` ok; per-user e2e `--- PASS: TestPerUserIsolation (38.75s)`; MSI builds with the new property |

The full suite (`go test -count=1 ./...`, all 27 packages ok) takes 3m45s
on this machine, dominated by the e2e package (206 s, as before this
change); the new unit tests add about 15 s, the opt-in per-user e2e
30-90 s.

## Design decisions and deviations

- **Users vs devices.** A user may own several certificates; the user is
  carried in the identity URI as `?user=`. Certificates without it act as
  the user named like the certificate, so existing fleets work unchanged.
- **When separation applies.** Hub: as soon as a user exists, a per-user
  worker connected, or `[tenancy] enabled`. Workers: per-user isolation, or
  the hub's policy says users are separated. A one-person fleet with plain
  client certificates behaves exactly as before (a laptop and a desktop
  still see each other's jobs).
- **Job and session directories stay privileged** (SYSTEM / root, private)
  instead of being owned by the user's account: the shim runs privileged
  and trusts `job.json`; letting the user write there would let a user steer
  a privileged process. Users reach their logs through the API; their code,
  mirrors, staging and tmp are theirs.
- **The shim stays privileged and starts the workload as the user** (as
  specified), but never imports the user's environment into itself.
- **File operations via a helper process as the user** (not thread
  impersonation): simplest correct choice on both OSes; costs one process
  start per Files/Workspace call.
- **Windows profiles** are held once per user by longarmd (not per job), so
  a killed shim can't leak a profile reference (an earlier version did:
  Windows then keeps the hive loaded until reboot).
- **Session names** are per user in per-user mode; in shared mode with
  separation they stay one namespace (another user's name - even of an
  ended session - is refused, so its transcript can't be wiped).
- **Fleet-wide quotas** are the scheduler's (queue/fail at placement);
  workers enforce per-worker quotas and per-job limits. A direct submit to a
  worker is bound only by the per-worker quotas (documented).
- **Panel logins of users** are generated by the hub (`--panel`: password
  and TOTP secret shown once) instead of transmitting an admin-chosen
  password.
- **macOS** per-user isolation is refused (allowed by the brief).

## Known gaps

See [security.md "What is not isolated"](security.md#what-is-not-isolated-known-gaps):
loopback TCP between users' processes on one worker on Windows (only
Longarm's own forwards check port owners; on Linux the nftables table
`inet longarm`, `[isolation] loopback = "isolate"` by default, refuses it
between accounts - not verified on this Windows-only development machine:
the root e2e on CI asserts it - one-way UDP and unix sockets stay open); `/proc` command lines on Linux unless mounted
with `hidepid=invisible` (longarmd warns); world-accessible OS objects that
workloads create themselves (`/tmp` mitigated by umask 077); shared
isolation mode gives API separation only (and unrestricted forwards);
fleet-wide quota bypass by direct submit; no per-user disk quota; admins and
the hub are trusted (hub-submitted jobs run as the requesting user on
workers with `remote_jobs = "all"`); Windows profiles of a purged user stay
until reboot if longarmd itself was killed.

Not done / not verified:

- The MSI property and the deb postinst path were built (MSI) / linted
  (shellcheck in CI) but not installed end to end (installing a service on
  this machine is not allowed; the opt-in MSI test was not extended).
- The panel's Playwright tests were not run (they need a live hub); unit
  tests cover the new pages.
- `longarm mcp` has no `share` parameter for `fleet_job_submit` (the CLI
  has `--share`); MCP results are filtered by the hub and workers like
  everything else.

## Needs from a human

1. Review and merge PR #3 (not merged, not tagged).
2. On the Windows machine the per-user tests were developed on: ten profile
   directories `C:\Users\lt<4 hex digits>-alice|bob` from early local runs
   (before the profile fix) are still loaded by Windows and can only be
   removed after a reboot (delete the directories and their `ProfileList`
   registry entries, or):
   `Get-CimInstance Win32_UserProfile | ? LocalPath -match '\\lt[0-9a-f]{4}-(alice|bob)$' | Remove-CimInstance`
   (their accounts are already deleted). Nothing else was left behind.
3. For a production per-user worker on Linux: mount `/proc` with
   `hidepid=invisible`; decide on `[isolation] cgroup_root` under systemd
   (`Delegate=yes`), see security.md "Operations".
