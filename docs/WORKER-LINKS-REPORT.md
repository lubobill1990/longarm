| CI on the PR (ubuntu, windows, macos, lint, azure template, image smoke) | all green; ubuntu needed reruns for unrelated flakes (`TestHubContainerStart` "text file busy", `TestAzureDeployScripts` missing `bootstrap.env`) and once for the intermittent `TestPerUserLinks` hang below; final run: `--- PASS: TestPerUserLinks (36.41s)` on Linux as root |
# WORKER-LINKS report: port links between workers (joint debugging)

A user's processes on worker A reach a loopback port of worker B as
`127.0.0.1:<port>` on A - frontend and tests on A, backend on B - with
traffic worker to worker (direct or through the hub's relay), never through
the user's machine, and without giving workers or jobs any control rights.
Branch `worker-links`, PR against `main`. Usage:
[usage.md "Port links"](usage.md#port-links-worker-to-worker); threat model:
[security.md "Port links"](security.md#port-links).

## Done

| Area | What | Where |
|---|---|---|
| Gap fix: forward targets | `[forward] targets = "loopback"` (default) \| `"any"` in longarmd.toml; applies to `Forward.Open`, the built-in sshd's direct-tcpip (proxied and direct listeners) and port links, for every caller including admins. Shared isolation used to forward to any host:port | `internal/server/tenancy.go`, `internal/sshd`, `internal/longarmd/longarmd.go` |
| Control | `longarm link create B:8080 --on A [--port N] [--ttl 8h] [--job ID]`, `longarm link ls [--all] [--json]`, `longarm link rm ID...`; MCP `fleet_link` (create / list / remove); hub gRPC `Fleet.Create/List/RemovePortLink`; panel page "Port links" (`GET/DELETE /api/port-links`) | `cmd/longarm/links.go`, `cmd/longarm/mcp.go`, `internal/hub/portlinks.go`, `web/src/panel/PortLinksPage.tsx` |
| Storage, lifetime | links in the hub db (migration 4), owner = caller, TTL (default 8h, max `[links] max_ttl`), optional binding to a job on A (removed when it ends or vanishes), removed with their owner (`user remove`) or a revoked worker; expired by the hub and by both workers | `internal/hub/portlinks.go`, `internal/hub/store` |
| Quotas | `[links] max_per_user` (20), listen ports 1024-65535 unique per worker, `[links] disabled` | `internal/hub/config.go` |
| Grants | the hub signs {id, owner, from, to, port, listen port, expiry} with its hub certificate's key (issued by the fleet CA, trusted by every worker); verification checks chain + `hub` role + signature + expiry | `internal/portlink` |
| Distribution, revocation | the hub pushes each worker the full, authoritative list of links naming it on the worker's existing hub session (`HubCommand.port_links`, at connect and on every change; target endpoints re-sent when B's change); workers keep it in `<data>/port-links.json`; a link missing from the list is revoked: both sides close the listener and cut open connections | `internal/hub/portlinks.go`, `internal/longarmd/portlinks.go`, `hubagent.go` |
| Target side (B) | `LinkTunnel.Dial` - the only RPC a worker certificate may call on a worker: grant valid, names the caller (mTLS) as A and this worker as B, link in B's list with the same owner/source/port, owner not disabled, then `Forward.Open`'s checks for the owner (`[forward] targets`; per-user: a process of the owner's account listens on the port); acknowledged with an empty frame; audited (accept and deny); removal cuts the stream | `internal/server/linktunnel.go`, `internal/server/auth.go` |
| Source side (A) | listener on 127.0.0.1:<listen port> per link (retried while the port is busy); per-user: only processes of the owner's account (Linux: socket uid in `/proc/net/tcp{,6}`; Windows: owning PID of the client socket → token user); each connection = a `LinkTunnel.Dial` stream over a gRPC connection to B made by the client transport race (tcp / tailscale / longnet) with A's own longnet and tailscale nodes; retries briefly when B hasn't got the push yet; status (listening/error, path, connections, active, refused, bytes) reported to the hub | `internal/longarmd/portlinks.go`, `internal/platform/*`, `internal/client` (`NewDialerFor`, `LegTransport`) |
| Relay | worker ↔ worker only for pairs with an active link (the hub sets them on its in-process relay; a standalone `longarm-relay` never relays between workers); removal drops the pair at once | `internal/relay/tls.go`, `server.go` |
| Discovery | the owner's jobs, sessions and exec on A get `LONGARM_LINKS=B:8080=127.0.0.1:18080,...` and `LONGARM_LINK_<WORKER>_<PORT>` (caller's variables win; a retried submit keeps the first submit's) | `internal/server/linktunnel.go`, `server.go`, `sessions.go`, `exec.go` |
| Audit | hub: `links/create`, `links/remove` (with reason: removed, expired, job ended, user removed, node revoked); B: `LinkTunnel/Dial` per connection (accepted / denied with why); A: `link/refused` for local connections refused in per-user mode | |
| Tailnet ACLs | worker → worker tcp:7701 added to the Headscale policy and the Tailscale grant docs (with the note that it grants nothing without a hub grant) | `deploy/headscale/config/policy.hujson`, `deploy/hub/README.md` |
| Docs | usage (section, hub.toml `[links]`, longarmd.toml `[forward]`, MCP), security (threat model, compromised worker, table, shared-mode text), design §3.2/§6, README (en + zh), Azure E2E step 4b | `docs/`, `README*.md`, `deploy/azure/E2E.md` |

## Build, run, test

```sh
go build ./...                                       # Go >= 1.27.3 (GOTOOLCHAIN=auto)
for os in windows linux darwin; do for a in amd64 arm64; do GOOS=$os GOARCH=$a go vet ./...; done; done
go test ./...                                        # unit + e2e (per-user tests skip)
go test -run 'PortLinks|LinkTunnel|LinkEnv|ForwardTargets|RelayWorkerPairs|PeerIsAccount|ProcNet' -v ./...   # the new tests
LONGARM_PERUSER_TEST=1 go test -run PerUser -v ./e2e # elevated (Windows) / root (Linux): includes TestPerUserLinks
buf generate                                         # proto (protoc-gen-go v1.36.12, -go-grpc 1.6.2)
node web/build.mjs; (cd web && pnpm typecheck && pnpm test)
```

Try it (a hub, two workers wa and wb):

```sh
longarm run wb -- python3 -m http.server 8080 --bind 127.0.0.1
longarm link create wb:8080 --on wa --port 18080
longarm exec wa -- curl -s http://127.0.0.1:18080/        # or $LONGARM_LINK_WB_8080 in your jobs on wa
longarm link ls ; longarm link rm <id>
```

## Tests

New:

- `internal/portlink`: signing/verification - valid, expired, tampered (port, source, expiry), flipped signature, signed by a worker certificate, by another CA's hub, foreign chain, issued in the future, incomplete; environment and ids.
- `internal/hub/portlinks_test.go` (`TestPortLinks`): creation rules (same worker, unknown worker, a client as target, ports, TTL bounds, another user's or unknown job), lists and signed grants pushed to both workers (grant verified against the CA; target endpoints; re-push on endpoint change), status reports only from the source worker, ownership (others: invisible, NotFound on remove, PermissionDenied for `--all`; admins see all), listen-port conflicts (others learn no id), job binding, expiry, quota, persistence (reopened hub), removal by owner / user removal / node revocation, hub audit. `TestTenancyRoleTable` extended.
- `internal/server`: `TestLinkTunnel` (target side: not-yet-active, no grant, another worker using the grant, grant for another target, worker-signed grant, grant/list mismatch, client certificate, data round trip with the acceptance frame, removal cuts an open stream, disabled owner, audit lines), `TestLinkEnv` (job env, caller wins, retried submit, other users get nothing), `TestForwardTargetsDefaultLoopback` (loopback ok, other hosts refused for users and admins, sshd check, `any` opt-in), `TestAuthzTable` / `TestReleaseRoleDeniedOnWorkers` (worker certificates: `LinkTunnel.Dial` only).
- `internal/relay`: `TestRelayWorkerPairs` (no pair: data and rendezvous dropped; pair: both directions + CallMeMaybe; a third worker not covered; removal cuts at once).
- `internal/platform`: `TestPeerIsAccount` (real loopback connections, IPv4 and IPv6; Windows and Linux), `TestProcNetSocketUID` (tcp and tcp6 incl. IPv4-mapped).
- e2e `TestPortLinks` (hub + 2 workers + users alice/bob, real processes): alice's HTTP server on wb reached from her `exec` and her job on wa through the link and its variables; `link ls` listening with traffic over `tcp/direct`; bob can't see / remove / squat it, gets no variables, and his client certificate can't call `LinkTunnel.Dial`; admin `--all`; works with the hub killed, kept by a hub restart; a `--job` link goes when the job is killed; removal cuts a long-lived connection and refuses new ones; hub and target audit.
- e2e `TestPortLinksRelay`: workers with only longnet legs and hole punching off: the link works through the hub's relay (`longnet/relay` in `link ls`). Negative control run: with the hub not setting relay pairs the test fails (timeout), so it exercises the relay policy.
- e2e `TestPerUserLinks` (opt-in, two per-user workers): alice's process on w1 reaches her server on w2 (running as her account there); bob's process on w1 is refused by alice's listener (counted as refused); alice's link to bob's server on w2 is refused by w2 ("not listened on by a process of yours", audited); bob can't take alice's listen port. CI runs it with the other `PerUser` tests (Linux as root, Windows as administrator).
- e2e `TestMCP`: `fleet_link` list and a refused create; tool list.
- web: `PortLinksPage` list / empty state / remove (DELETE), `until()`.

Results on this machine (Windows 11, elevated shell), after the review fixes:

| Run | Result |
|---|---|
| `go vet ./...` for windows/linux/darwin × amd64/arm64 | ok |
| `go test -count=1 ./...` | every package ok except e2e load flakes in full runs, each passing alone: `TestHubDownFast` (timing), `TestDaemonLegProxy` (known), `TestHubRevocation` (job output timeout under load); the link tests pass in every run |
| `LONGARM_PERUSER_TEST=1 go test -run TestPerUserLinks ./e2e` | `--- PASS: TestPerUserLinks (28.65s)`; temporary services, `lt????-*` accounts and profiles all removed afterwards |
| `scripts/privacy-check.sh`, gofmt | ok |
| web `pnpm typecheck`, `pnpm test` | ok (26 tests) |

The e2e package takes about 5 minutes on this machine with everything else
running in parallel (the new tests add about 20 s and 15 s, in parallel with
the rest); unit packages are under a minute each.

## Self-review

An independent review pass (correctness, concurrency, leaks, security) of
the branch found no authorization bypass; its findings and what was done:

| Finding | Resolution |
|---|---|
| Hub `Connect` queued the welcome after releasing the lock; a concurrent port-link push (or update hint / tenancy flip) could reach a new session first ("no welcome") or a stale list could follow a newer one | all initial messages are queued under the lock, before anything else can push; the tenancy flip runs after them |
| An endpoint change of the target (re-pushed on every hello) closed the gRPC connection and cut open link connections | target connections are reference-counted; a changed endpoint set gives new streams a new connection and retires the old one when its last stream ends |
| Listener on 127.0.0.1 only: another account could listen on `[::1]:<port>` (or hold the port) and catch the owner's `localhost` traffic | listeners bind 127.0.0.1 and ::1; job variables only for links whose listener is bound; a port held by another process shows as `error` |
| A shared-isolation source exposes a per-user target's private port to every local process | with users separated, only admins may create such a link |
| `[links] max_ttl` could exceed the hub certificate's renewal margin (grant certificate expiring mid-link) | `max_ttl` is capped at 720h |
| `LONGARM_LINK_<W>_<PORT>` could collide (`a-b` / `a_b`) | colliding per-link variables are left out (`LONGARM_LINKS` stays) |
| `--port` / `listen_port` wrapped silently through `int32` | range-checked in the CLI and MCP |
| `LinkTunnel.Dial` returns on removal while a receive goroutine is still in `Recv` | kept: that goroutine only receives and ends when gRPC finishes the stream (documented in `pipeStream`) |
| With tenancy off every caller acts as admin over gRPC (the panel's user logins don't) | kept: consistent with the rest of the hub in a one-person fleet (several devices of one person) |

## Design decisions and deviations

- **Name.** The hub already has "links" (`Fleet.ReportLinks/Links`, the
  panel's "Links" page: client↔worker network paths), so the feature is
  "port links" in the API (`PortLink`, `/api/port-links`, panel "Port
  links"); the CLI is `longarm link` as specified.
- **Signing key.** The hub certificate's key (CA-issued, role `hub`) signs
  grants; the certificate travels with the grant. No new key, no CA key
  needed online; a renewed hub certificate re-signs lazily.
- **Revocation = the hub's list.** B requires both a valid grant and the link
  in its hub-pushed list; the list is persisted so links work while the hub
  is down. A link removed during a hub outage ends when the hub is back or at
  its TTL (short default, 8h). A briefly retries `FailedPrecondition` for the
  race "A got the push before B".
- **Acceptance frame.** `LinkTunnel.Dial` answers the grant frame with an
  empty frame once it accepted and connected, so A can retry before any
  application byte is sent.
- **Target host** is always `127.0.0.1:<port>` on B (the link names a port,
  not a host); ownership is checked for the link's owner, not for A.
- **Listen port** defaults to the target port; ports below 1024 are refused;
  conflicts are checked per worker on the hub (the error names the link only
  to its owner and admins). A busy port on A shows as `error` in `link ls`
  and is retried every 10 s.
- **Per-user peer check** is done on accept by longarmd (uid / SID of the
  client socket), not with loopguard: loopguard (PR #4) keeps la-* accounts
  apart but does not cover connections to root/SYSTEM listeners, so the
  branch is based on `main` and does not depend on PR #4.
- **Shared isolation on A**: every local process can use the listener (it is
  a loopback port like any other) - documented.
- **Relay**: allowed pairs are set by the hub on its in-process relay; the
  standalone `longarm-relay` has no link information and keeps refusing
  worker↔worker (documented).
- **`[forward] targets` default `loopback`** applies to existing installs as
  well when they upgrade (a code default, not an installer setting): the safe
  choice; `"any"` restores the old shared-mode behaviour. Admins are bound by
  it too (it describes what the worker may reach).
- **Job binding** uses the hub's job index (A's reports): the link goes when
  the job is terminal or disappears from A's report.

## Known gaps

- Links carry TCP only (listeners on 127.0.0.1 and ::1).
- Another local process that grabs the listen port before longarmd binds it
  (link creation, longarmd restart) keeps it: the link then shows `error`
  and its variables are withheld from jobs, but a job that hard-codes the
  port would talk to that process.
- A link removed while the hub is unreachable keeps working until the hub is
  back or the TTL ends (bounded by `[links] max_ttl`).
- Disabling a user keeps their links listed; B refuses them (the policy says
  disabled) until the user is enabled or removed.
- macOS workers (no per-user isolation) and shared-isolation workers don't
  restrict who connects to a listener locally.
- Per-connection audit on B can be chatty for clients without keep-alive.
- The panel page was built and unit-tested, not driven with Playwright.
- **Intermittent, Linux CI, unexplained:** in 1 of 2 CI runs of
  `TestPerUserLinks` (two per-user longarmd on one ubuntu runner) the second
  worker stopped reporting to the hub right after connecting and its gRPC
  port stopped answering, while it still received hub pushes; the rerun
  passed (`--- PASS: TestPerUserLinks (36.41s)`), Windows (local and CI)
  always passed. No cause found by reading the code (no lock shared between
  the stuck paths). The Linux per-user e2e helper now sends SIGQUIT to
  longarmd when a test fails, so the next occurrence leaves a goroutine dump
  in the test log.
- `longarm link` needs the hub (no hub-less mode: grants come from the hub).

## Needs from a human

1. Review the PR (not merged, not tagged). When PR #4 (`azure-e2e-fixes`)
   merges first, the branch needs a rebase: expected conflicts in
   `docs/usage.md`, `docs/security.md`, `README.md`, `deploy/azure/E2E.md`,
   `internal/hub/config.go` and `internal/relay/server.go` (textual only).
2. Run the Linux per-user suite (CI does: `-run PerUser` as root) - a few more times: see the intermittent hang under "Known gaps" and, if
   wanted, the Azure E2E step 4b with two real workers on different
   networks.
3. Tailnets in use: add the worker → worker tcp:7701 rule (Headscale policy
   in `deploy/headscale/config/policy.hujson`; Tailscale grants in
   `deploy/hub/README.md`), or let links use the relay.
