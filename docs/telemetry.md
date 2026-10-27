# Telemetry

When something goes wrong in the field, the maintainers of a fleet need to
find out what happened and why - which command failed, on which kind of
machine, after what, with which error - without anybody's personal data
leaving their machines. Longarm's telemetry is built for that:

- it is **off unless the hub collects it**: the hub operator sets an Azure
  Application Insights connection string ([below](#turning-it-on-for-a-hub));
  without one, nodes collect nothing;
- every **machine's owner can turn it off**, and the hub cannot turn it
  back on ([opting out](#opting-out));
- it carries **no personal data in clear**: names become keyed hashes,
  secrets, addresses and command lines are removed - on the node before
  anything is queued, and again on the hub ([scrubbing](#how-scrubbing-works));
- what a machine would send can be **inspected** before it is sent
  (`longarm telemetry show`).

## Where it goes

```
longarm (CLI runs, daemon, mcp)  ┐ queue: <config>/telemetry/queue.jsonl
longarm-tray                     ┘           │
longarmd                           queue: <data>/telemetry/queue.jsonl
                                             │  Telemetry.Push (gRPC, mTLS, every ~30 s)
                                             ▼
longarm-hub ── scrubs again, stamps fleet / node / user pseudonyms ──► Application Insights
            └─ its own telemetry                                       (<IngestionEndpoint>/v2.1/track)
```

- **Nodes push to their hub** over the authenticated gRPC connection they
  already use (`Telemetry.Push`, `proto/longarm/v1/telemetry.proto`); worker,
  client and admin certificates may call it. The hub answers whether it
  collects; if not, the node drops its queue, stops collecting and asks
  again after six hours.
- **longarmd** pushes every 30 s and once more when it stops.
- **longarm** CLI runs never wait for telemetry: a run queues its items on
  disk and sends the queue at exit only if it had a hub connection open
  anyway (at most one second); otherwise the next run, `longarm daemon` or
  `longarm mcp` (both push every 30 s) send it. `longarm telemetry flush`
  sends it now. **longarm-tray** queues into longarm's queue.
- **The hub** forwards in batches of up to 500 items, retries with backoff
  (429, 5xx, network errors; partial success resends the refused items that
  can be retried), keeps at most 10 000 items in memory and never blocks an
  RPC. It authenticates with a Microsoft Entra ID token of its managed
  identity when it has one (`AZURE_CLIENT_ID`, as deploy/azure sets), so the
  Application Insights resource can have local authentication disabled;
  otherwise the instrumentation key of the connection string is enough
  (development).
- **Retention** is the Application Insights resource's: 90 days by default.
  Who can read it is decided by the resource's access control in Azure.

## What is collected

Every item carries this context (Application Insights tags and custom
dimensions):

| Field | Example | Notes |
|---|---|---|
| component (`ai.cloud.role`) | `longarmd` | `longarm`, `longarmd`, `longarm-hub`, `longarm-tray` |
| version (`ai.application.ver`) | `1.8.2` | |
| os / arch | `windows/amd64` | |
| session (`ai.session.id`) | `3f9c...` | random per process |
| fleet | `a41c09e2b7d3` | short hash of the hub CA's fingerprint |
| node (`ai.cloud.roleInstance`) | `<node:3b6810>` | keyed hash of the certificate name; stamped by the hub from the caller's certificate |
| user (`ai.user.id`) | `<user:99de73>` | keyed hash of the Longarm user (clients and admins) |
| role | `worker` | `worker`, `client`, `admin`, `hub` |
| operation (`ai.operation.id`, `ai.operation.parentId`) | `4bf92f35...` | W3C trace id of the CLI command / request it belongs to |

### longarm (CLI, daemon, MCP) and longarm-tray

| Item | Name | Content |
|---|---|---|
| request | the command, e.g. `run`, `exec`, `admin user add` | exit code, duration, success (exit code ≠ 255) - one per CLI run |
| exception | `grpc.Unavailable`, `trust_refused`, ... | error of a failed run (scrubbed message, no stack) |
| request | `mcp fleet_exec`, ... | each MCP tool call: duration, outcome |
| dependency | `dial` (target `worker`) | transport race: winning leg (`tcp`, `longnet`, `tailscale`) and path kind, each leg's outcome by error class (`refused`, `timeout`, `tls`, ...), duration |
| dependency | `Jobs/Submit`, `Fleet/Place`, ... (targets `worker`, `hub`) | gRPC method, status code, duration (CLI: all; daemon/MCP: failed or slow ones plus per-minute aggregates) |
| event | `hub.unreachable` | error class, status code |
| metric | `dial.leg` | per-minute counts by transport and outcome |
| event | `process.start` / `process.stop` | daemon, MCP and tray: command, uptime |
| exception | `panic`, `fatal error` | crash of the daemon, MCP server or tray (next start; see below) |

### longarmd (worker)

| Item | Name | Content |
|---|---|---|
| event | `process.start` / `process.stop` | isolation mode, update mode, whether trust/access/sshd are on, number of listeners; uptime at stop |
| request | gRPC method, e.g. `Jobs/Submit` | sent for failures (server faults) and calls slower than 2 s: status code, duration, the caller's **role** (never its name); long-lived streams only when they fail |
| metric | `request.duration_ms` | per method and minute: count, sum, min, max, p50/p95/p99, failures |
| metric | `rpc.denied` | refused calls per method, status code and caller role |
| event | `job.submitted` | workspace / sharing / secrets used (yes/no), caller role, isolation, memory and CPU reservation - **no command line, environment, paths** |
| event | `job.ended`, `session.ended` | final state, exit code class (`ok`, `error`, `not_found`, `signal`, ...), duration, start delay, reservation; linked to the CLI command that submitted it |
| metric | `job.state` | state changes per minute |
| event | `cert.renewal` | success, status code on failure |
| event | `update.status` | self-update phases: applying, restarting, ok, rolled_back, failed, error (versions, scrubbed error) |
| event | `toolchain.reconcile` | profile state and scrubbed error |
| metric | `trust.denied`, `audit.sink_dropped` | refused TLS peers (end-to-end trust), audit records a sink dropped |
| metric | `link.dial`, `link.conn`, `link.bytes` | port links: streams the target accepted / refused / couldn't connect (with the gRPC code); local connections on the source by outcome (`ok`, `refused`, `dial_failed`), transport and path kind; bytes each way - **no link id, owner, worker names, ports or addresses** (the names a link brings are added to the scrubber) |
| trace | WARN / ERROR log records | the message template, attributes as properties (scrubbed), at most 10 per message per 10 minutes |
| exception | `panic`, `fatal error` | crash (next start) |

### longarm-hub

| Item | Name | Content |
|---|---|---|
| request / metric | gRPC method | like longarmd |
| event | `node.joined`, `node.approved`, `node.revoked` | role, state, the node's OS / arch / version |
| metric | `enroll.bad_code`, `node.connected` | join attempts with a bad code; worker sessions by version, OS and isolation |
| event | `cert.renewed`, `hub.cert_renewed`, `acme.certificate` | node and hub certificate renewals, a new Let's Encrypt certificate |
| metric | `acme.get_certificate_failed` | failed certificate fetches for the panel's host |
| event | `scheduler.placement` | outcome (`placed`, `queued`, `placed_from_queue`, `no_fit`, `over_quota`), job type, memory, queue position, wait |
| event | `node.update` | self-update reports of workers |
| metric | `relay.sessions`, `relay.sessions_new`, `relay.bytes`, `relay.drops`, `relay.handshake_fails` | per minute |
| metric | `nodes.online`, `scheduler.queue`, `links.active`, `telemetry.forward_failed`, `telemetry.hub_dropped` | per minute |
| event | `link.created`, `link.removed` | port links: the creator's role, bound to a job (yes/no), TTL; the removal reason class (`removed`, `expired`, `job_ended`, `user_removed`, `node_revoked`, `not_allowed`) and age - no ids, names or ports |
| trace | WARN / ERROR log records | as longarmd |
| event | `process.start` / `process.stop` | relay / https / tailnet on; uptime |

Every queue also reports `telemetry.dropped` when it was full.

## What is never collected

- command lines, arguments, environment variables (names or values), working
  directories, job output, prompts, file contents;
- host names, OS user names, Longarm node and user names, e-mail addresses
  (they become `<host:…>`, `<user:…>`, `<node:…>`, `<email>`);
- IP addresses (`<ip>`; only loopback stays), URLs' query strings and user
  info, join codes;
- secrets: tokens, passwords, keys, `Authorization` values, JWTs, private
  keys;
- paths under the user's home directory (`~/…`, other users' homes become
  `/home/<user:…>`); stack traces keep only module-relative files and
  function names.

## How scrubbing works

1. **Instrumentation sends little free text.** Items are mostly names,
   codes and numbers chosen in the code. Property keys that name sensitive
   data (`argv`, `cmd`, `env`, `cwd`, `path`, `host`, `addr`, `token`, ...)
   are replaced by `<redacted>` whatever their value.
2. **The node scrubs every string before it is queued** with
   `internal/pii`: its own names (node, user, the account names of
   per-user isolation, the users of `[access]`, every client that called a
   worker), the local host name, OS user and home directory become keyed
   hashes; secrets, e-mail and IP addresses are replaced; double-quoted text
   in messages (what Go errors quote: a program, a path, user input) becomes
   `"<q>"`. Sizes are capped (8 KiB per message, 1 KiB per property, 64
   properties, 64 stack frames).
3. **The hub scrubs again** with every node, host and user name in its
   registry, and replaces the node, user, role and fleet of each item with
   what the caller's certificate says.

The hashes are keyed with a salt derived from the fleet's CA certificate:
the same name is the same pseudonym on every node of a fleet (a problem can
be followed across machines), but whoever reads Application Insights without
the CA can't reverse them. Scrubbing free text is best effort, which is why
the instrumentation avoids free text in the first place.

## Errors, trace ids and crashes

- A CLI run is one **operation**: its W3C trace context goes with every
  call to workers and the hub (gRPC metadata `traceparent`), so a worker's
  request and job events link to the command that caused them. A failing
  command prints the short trace id - `longarm: Unavailable: ... (trace
  4bf92f35)` - to quote in a bug report. Log lines written while handling an
  operation carry `trace=<id>`, so `longarm diag` bundles can be matched to
  it.
- **Crashes** of longarmd, longarm-hub, longarm daemon / mcp and longarm-tray
  are written by the Go runtime to `<dir>/telemetry/crash-<component>-<pid>.txt`
  (a clean exit removes the file). The next start of a process using that
  dir sends it as an exception - only the panic message (scrubbed) and the
  stack's function names and file:line - and removes it.
- **Sampling**: workers and the hub send a request only when it failed or
  took longer than 2 s; everything else becomes per-minute aggregates. Log
  traces are limited per message; each node may push 1200 items per minute
  (burst 5000) - the hub drops the rest and counts them.

## Opting out

| Machine | How | Effect |
|---|---|---|
| worker | `[telemetry] enabled = false` in `<data>/longarmd.toml` | longarmd collects and sends nothing; the hub can't change it |
| client | `longarm telemetry off` (persistent; drops the queue) or `LONGARM_TELEMETRY=0` | longarm, its daemon, MCP server and longarm-tray collect and send nothing |
| fleet | no `[telemetry]` connection string on the hub | every node stops collecting when the hub tells it |

`longarm telemetry status` shows whether telemetry is on (and why not), the
fleet and node pseudonyms, what the hub last said and the queue size;
`longarm telemetry show` prints the queued items - exactly what would be
sent, as JSON lines. A worker's queue is `<data>/telemetry/queue.jsonl`.

## Turning it on for a hub

Create an Application Insights resource and give the hub its connection
string:

```toml
# hub.toml
[telemetry]
connection_string = "InstrumentationKey=...;IngestionEndpoint=https://<region>.in.applicationinsights.azure.com/"
# enabled = false   # keeps it off with a connection string set
```

or `LONGARM_HUB_APPINSIGHTS_CONNECTION_STRING` (wins over hub.toml; for
containers). With a managed identity (deploy/azure: `AZURE_CLIENT_ID`) grant
it the **Monitoring Metrics Publisher** role on the resource and disable
local authentication there; the hub then sends Entra ID tokens
(`https://monitor.azure.com/`). The hub logs `telemetry: forwarding to
Application Insights` at start.

Useful queries (Logs, KQL):

```kusto
// everything one CLI command caused, from a "(trace 4bf92f35)" in a bug report
union requests, dependencies, customEvents, exceptions, traces
| where operation_Id startswith "4bf92f35"
| order by timestamp asc

// workers whose jobs end with "lost"
customEvents | where name == "job.ended" and customDimensions.state == "lost"
| summarize count() by cloud_RoleInstance, application_Version
```
