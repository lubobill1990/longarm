# Troubleshooting: from a bug report to a fix

How maintainers find out what went wrong on someone's machine, and how to
fix it. What is collected and how personal data is removed:
[telemetry.md](telemetry.md). Setting up the Azure side:
[deploy/azure](../deploy/azure/README.md#telemetry-and-diagnostic-bundles).

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
   workers and clients update from the hub's release channel.
6. Check the fix in telemetry: the `problemId` / failing request stops
   appearing for the new `application_Version`.

## Common problems

| Symptom | Look at | Usual cause |
|---|---|---|
| `worker w1 refused by end-to-end trust` | client error text, `longarm trust show` | worker not endorsed, or the hub dropped the extension (old hub) |
| `x509: certificate has expired` | `state/certificate.json` | node offline past renewal; `longarmd join --force` |
| exec hangs, then `relay` errors | `dependencies` of type `dial`, `dial.leg` metric | UDP blocked; TCP fallback on 7443 |
| job queued forever | hub `customEvents` `scheduler.placement` (outcome `no_fit`, `queued`) | no worker fits (memory gate), all drained |
| worker crashes in a loop | `exceptions` crash, bundle `telemetry/crash-*.txt` | panic - fix with a test |
| hub restarts | Azure: `az container logs`, `exceptions` from `longarm-hub` | liveness probe, Litestream restore error |
