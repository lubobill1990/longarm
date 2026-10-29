# Troubleshooting: from a bug report to a fix

Users: [common problems](#for-users-common-problems) first. The rest is how
maintainers find out what went wrong on someone's machine, and how to fix
it. What is collected and how personal data is removed:
[telemetry.md](telemetry.md). Setting up the Azure side:
[deploy/azure](../deploy/azure/README.md#telemetry-and-diagnostic-bundles).

## For users: common problems

Start with `longarm doctor` if your version has it; otherwise the steps
below. Still stuck: file a bug report ([below](#what-a-good-bug-report-contains)).

### My worker is offline

`longarm ls` shows it `offline`. The Windows default (MSI Default,
`install.ps1`) is the **user-session worker**: longarmd runs as you, started
by a scheduled task at your logon, no service and no password. It keeps
running while a remote desktop is disconnected, but **stops at logoff or
reboot until you log on again** - log on (an RDP connection will do), or
start it now:

```powershell
longarmd session-mode status     # the task LongarmWorker-<SID>, the host, longarmd
longarmd session-mode start      # start it now ("not registered": longarmd session-mode install)
longarmd status                  # the running longarmd: hub link, jobs
```

Logs: `~\.longarm\longarmd-session.log` (the host: starts, exits,
restarts) and `~\.longarm\longarmd.log` (longarmd). A worker that must run
with nobody logged on: reinstall with `RUN=service` (Custom > Advanced; it
needs the account's password) - or use the [admin
worker](usage.md#admin-worker-windows), a LocalSystem service, for
administrator tasks.

Other installs: `longarmd service status` (elevated / `sudo`). Logs:
Windows service `<data>\longarmd.log` (MSI: `C:\ProgramData\Longarm\longarmd\longarmd.log`;
admin worker `C:\ProgramData\Longarm\longarmd-admin\longarmd.log`); Linux
`journalctl -u longarmd@<user>` (deb / rpm) or `journalctl -u longarmd`
(`install.sh`); macOS `sudo launchctl print system/com.longarm.longarmd`.
Data dirs: [usage.md](usage.md#data-directory).

### Sign-in expired

Certificates renew by themselves while the machine is online; one that was
offline past its certificate's expiry fails with `x509: certificate has
expired`. Sign in again - the same code and approval as the first time:

```sh
longarmd login --force --role both   # worker (+ this machine's longarm); --force: replace the old certificate
longarm login                        # a client only: replaces an expired certificate, no --force needed
```

Without a hub argument both use the hub they are enrolled with (or the one
the installer recorded). On Windows the tray's "Sign in with Microsoft" does
the same. Not `longarmd join --force` - that needs a new join code and is
only for fleets enrolled with join codes (self-hosted without Microsoft
sign-in).

### "Windows Firewall: ... connections stay on relays"

Windows Firewall drops unsolicited inbound UDP to a `longarm.exe` (or
`longarmd.exe`) without an inbound allow rule, and always to one with a
Block rule (dismissing the "allow access?" prompt adds one). Hole punching
fails and every connection goes through a relay: it works, but slower.
The tray, `longarm doctor`, `longarm pool` and `longarm ls` say so and name
the program.

- **The tray**: press **Allow** on the warning card and accept the
  administrator prompt. The tray runs its own installed `longarm.exe
  firewall allow --program <the program in the warning>` (removes that
  program's Block rules, adds the allow rules), checks again and clears the
  card.
- **By hand**, in an elevated PowerShell (Start > "Terminal (Admin)"), the
  command the warning shows, e.g.:

  ```powershell
  & "C:\Program Files\Longarm\longarm.exe" firewall allow
  & "C:\Program Files\Longarm\longarm.exe" firewall allow --program "C:\Program Files\Longarm\longarmd.exe"
  longarm firewall status    # ok, or what is still wrong (exit 1)
  ```

  A quoted path at the start of a line needs PowerShell's call operator
  `&`; without it PowerShell answers `ParserError: Unexpected token
  'firewall'`. Older longarm versions print the command without `&`: add it.
  In cmd.exe, leave the `&` out.

**A path under `%USERPROFILE%\tmia-desktop-live\longarm-x.y.z\`**: the
daemon was started by the copy of longarm that TMIA Desktop ships, not by
Longarm's installer (whose rules cover `C:\Program Files\Longarm`). The rule
has to be for the program that runs, so the tray offers to allow that path
and says it is a copy. The copy's folder changes with every TMIA Desktop
version, and the new path needs a rule again (or stop that daemon,
`longarm daemon stop`, and let the tray start the installed one).

### "A newer version is available"

The hub tells every machine about its newest release:

- **Workers that can update themselves** (a service or a binary install in
  a directory longarmd can write) follow the hub's rollout (`longarm admin
  release push`): canary first, health check, rollback.
- **Workers that can't** - typically MSI installs: the binaries are under
  `C:\Program Files\Longarm`, which the worker's account can't write
  (`longarmd update status` says whether this build can update) - report
  that a newer version exists but stay on theirs.
- **`longarm`** prints a notice on stderr, only on a terminal and never
  with `--json`, at most once a day (off: `LONGARM_NO_UPDATE_NOTICE=1`).
- **The tray** shows an "Update to X" banner: it downloads the MSI from the
  hub, verifies its sha256 (the hub's release record) and its minisign
  signature (the release key built into the tray) and runs
  `msiexec /i <file> /qb` - the remembered MSI properties carry over. A
  download that fails either check is reported and never runs.
- **The panel's** Nodes page marks outdated versions.

Update by installing the new MSI (the banner, the hub's "Get Longarm" page
`https://<hub>/get`, or `https://<hub>/releases/<version>/longarm-windows-amd64.msi`)
or by running the install script again (`install.ps1` / `install.sh`, as in
the [README](../README.md#join-a-hub)).

## What a good bug report contains

Ask reporters for three things (the issue template does):

1. **What they ran and what happened** - the command and the error text.
   Errors of `longarm` end with a trace id: `... (trace 4bf92f35)`.
2. **A diagnostic bundle id** - `longarm diag --upload` on the machine with
   the problem (on a worker: `longarmd diag --upload`, or remotely
   `longarm exec <worker> -- longarmd diag --upload --yes`). It prints a
   report id `LAR-20261027-7F3K9Q`.
3. **Versions** - `longarm version`, `longarmd --version` (the bundle has
   them too).

## The three sources

| Source | Where | Holds | Kept |
|---|---|---|---|
| Telemetry | Application Insights `<hub>-ai` | every component's requests, failures, crashes, warnings, key events - scrubbed, linked by trace id | 90 days |
| Diagnostic bundle | blob container `diagnostics` (`longarm admin diag get <id>`) | logs, configuration (no secrets), state, versions of one machine - scrubbed | 30 days |
| Local files | the machine | full logs (`<data>/longarmd.log`, `audit.log`, `longarm daemon` logs) | until rotated |

All three use the same pseudonyms: a node is `<node:3f2a9c>` everywhere
(keyed hash, fleet-specific), so a node in a bundle is the same node in
telemetry. Members of the fleet can compute the pseudonym of a name
(`longarm admin pseudonym <name>` - it needs the fleet's CA certificate,
which telemetry readers don't have) and so find a known node or user in
telemetry; the other way round there is no table to look up.

## Step 1: find the operation

In the Azure portal: Application Insights `<hub>-ai` -> Logs, or `az monitor
app-insights query --app <hub>-ai -g <rg> --analytics-query '...'`.

A trace id from an error message (the first 8 hex digits are enough):

```kusto
union requests, dependencies, exceptions, traces, customEvents
| where operation_Id startswith "4bf92f35"
| project timestamp, itemType, cloud_RoleName, name, resultCode, success, duration,
          message = coalesce(message, outerMessage), customDimensions
| order by timestamp asc
```

This shows the whole path: the CLI command (`longarm`), the hub calls it
made, the request on the worker (`longarmd`) and what failed there, in
order.

No trace id - search around the time and node of the report:

```kusto
union exceptions, traces
| where timestamp between (datetime(2026-10-27 09:00) .. 2h)
| where cloud_RoleInstance == "<node:3f2a9c>"
| where severityLevel >= 2 or itemType == "exception"
| project timestamp, cloud_RoleName, message = coalesce(outerMessage, message), problemId, customDimensions
```

## Step 2: is it one machine or many?

```kusto
exceptions
| where timestamp > ago(7d)
| summarize count(), nodes = dcount(cloud_RoleInstance), versions = make_set(application_Version)
    by problemId, cloud_RoleName
| order by nodes desc
```

```kusto
// failing requests by method and version
requests
| where timestamp > ago(1d) and success == false
| summarize failures = count(), nodes = dcount(cloud_RoleInstance) by name, resultCode, application_Version
| order by failures desc
```

```kusto
// connection problems: how dials to workers end (resultCode = error class),
// which leg wins, and the per-leg outcomes ("tcp:won,longnet:timeout")
dependencies
| where timestamp > ago(1d) and type == "dial"
| summarize total = count(), failed = countif(success == false), p95 = percentile(duration, 95)
    by resultCode, winner = tostring(customDimensions.winner), cloud_RoleInstance
```

```kusto
// per-leg outcomes per minute, fleet-wide
customMetrics
| where timestamp > ago(1d) and name == "dial.leg"
| summarize n = sum(valueCount) by transport = tostring(customDimensions.transport), outcome = tostring(customDimensions.outcome)
```

```kusto
// crashes after an update?
exceptions
| where timestamp > ago(3d) and customDimensions.crash == "true"
| summarize count() by application_Version, cloud_RoleName, problemId
```

Many nodes, one version: a regression - compare with the previous version
(`application_Version`). One node: its environment - go to the bundle.

## Step 3: the bundle

```sh
longarm admin diag ls --since 7d                 # report ids, node pseudonyms, versions
longarm admin diag get LAR-20261027-7F3K9Q       # downloads the zip
```

`manifest.json` says what is inside and why. Typical places to look:

- `logs/longarmd.log` (worker) / `logs/daemon.log` (client): grep for the
  trace id, then read backwards.
- `state/certificate.json`: expired or not yet renewed certificate, role,
  end-to-end trust extension.
- `state/connectivity.json` (client): which legs reached the hub and the
  workers.
- `config/*.toml`: unusual settings ([access], [isolation], listen URLs).
- `telemetry/crash-*.txt`: the stack of a crash.

## Step 4: reproduce and fix

1. Reproduce with the same version and configuration (the bundle's
   `config/` and `version.json`). Most problems reproduce with the e2e
   helpers: `e2e/hub_helpers_test.go` starts a hub, workers and clients on
   one machine.
2. Write the failing test first (unit test next to the code, or an e2e test
   for cross-process behavior), then fix.
3. If the error message didn't say what to do, improve it in the same
   change - the next user then doesn't need a bundle.
4. If telemetry didn't show the cause, add the missing event or property
   (docs/telemetry.md lists them; scrubbing applies automatically).
5. Ship: PR (squash), CI, `edge` image; the hub upgrade
   (`./deploy.sh --name <hub> --image ghcr.io/lubobill1990/longarm-hub:sha-<commit>`);
   once the release is in the hub's store, workers that can self-update
   follow the hub's rollout (`longarm admin release push`); MSI installs and clients
   get a notice (CLI, tray banner, the panel's Nodes page) and update by
   installing the new MSI or re-running the install script
   ([update notices](#a-newer-version-is-available)).
6. Check the fix in telemetry: the `problemId` / failing request stops
   appearing for the new `application_Version`.

## Common problems

| Symptom | Look at | Usual cause |
|---|---|---|
| `worker w1 refused by end-to-end trust` | client error text, `longarm trust show` | worker not endorsed, or the hub dropped the extension (old hub) |
| `x509: certificate has expired` | `state/certificate.json` | node offline past renewal; `longarmd login --force` / `longarm login` ([Sign-in expired](#sign-in-expired)); join-code fleets: `longarmd join --force` with a new code |
| exec hangs, then `relay` errors | `dependencies` of type `dial`, `dial.leg` metric | UDP blocked; TCP fallback on 7443 |
| job queued forever | hub `customEvents` `scheduler.placement` (outcome `no_fit`, `queued`) | no worker fits (memory gate), all drained |
| worker crashes in a loop | `exceptions` crash, bundle `telemetry/crash-*.txt` | panic - fix with a test |
| hub restarts | Azure: `az container logs`, `exceptions` from `longarm-hub` | liveness probe, Litestream restore error |
