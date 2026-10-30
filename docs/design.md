# Longarm design: a horizontally scalable remote-execution pool for Claude Code / Copilot CLI

> Status: design v1. Longarm replaces ad-hoc setups built from SSH, a hand-maintained host list, and wrapper scripts that launch agents on remote machines.
> Target scale: a handful (3–5) of interactive machines (where `claude` / `copilot` run) × roughly 10 workers (large-memory, many-core machines; Windows first-class, Linux and macOS supported).

---

## 0. Summary

| Decision | Choice | Rationale |
|---|---|---|
| Shape | **Three Go binaries**: `longarmd` (worker daemon), `longarm` (interactive-machine CLI + MCP server), `longarm-hub` (public server: control plane + relay) | Single static binaries, cross-compiled for Windows/Linux/macOS; the Tailscale and quic-go ecosystems are Go |
| Network | **Transport abstraction with two backends online at the same time**: (1) Tailscale (embedded `tsnet`), (2) our own `longnet` (QUIC + hole punching + hub relay) | Clients pick by policy or race them; if one network fails the other takes over — that in itself is reliability |
| Own tunnel | **A simplified magicsock**: end-to-end QUIC (mTLS) over a "virtual PacketConn" that switches between direct UDP and the hub relay; the relay only forwards ciphertext | No WireGuard, no TUN, no IP routing — we only need application-level streams, which cuts complexity by an order of magnitude |
| Protocol | gRPC (bidirectional streams) over the `net.Conn` provided by the Transport; application-level mTLS always on | Independent of the underlying network; authentication does not depend on tailnet ACLs |
| Job model | Jobs are **decoupled from connections**: the worker persists them to local disk and supervises the processes locally; clients resume reading logs by offset | Disconnects, path changes, or a hub outage do not affect jobs; this is the core of "reliable" |
| Control plane | Single `longarm-hub` instance + SQLite; **the worker is the source of truth for job state**, and the hub can rebuild its view from heartbeats | Ten machines do not need etcd/k8s; if the hub dies the data plane keeps working |
| Platforms | All three binaries come from one Go codebase, built for Windows / Linux / macOS (amd64 + arm64); the hub targets Linux/docker for deployment | Platform differences are confined to a few small interfaces (service management, process-tree isolation, PTY), see §11.1 |
| UI | **The core is fully headless**; `longarm-tray` (a tray app shared by workers and clients) and the hub web panel are just API clients sharing one frontend | Everything works without a UI installed; every UI action has a CLI equivalent, see §11.3 |
| Deployment | MSI / pkg / deb / rpm / docker packages + 6-character join codes (§11.2); headless one-liner: `irm https://hub/install.ps1 \| iex; longarmd join <token>` → installs the service + registers + installs the toolchain per profile + self-updates | Adding a machine by hand otherwise takes several manual steps |
| Compatibility | `longarmd` embeds an SSH server (on the overlay only); `longarm ssh-config` generates a ProxyCommand | Existing SSH-based scripts, VS Code Remote-SSH, and scp keep working unchanged, allowing a smooth migration |

Implementation phases: **P0 builds `longarmd`/`longarm` on the Tailscale backend first**, P1 adds the hub + scheduling + MCP, and P2 adds a second network leg. The self-built tunnel is a "second leg", not a launch dependency.

---

## 1. Requirements and constraints

**Functional**
1. Installing one client (`longarmd`) on a new machine registers it in the pool; no configuration changes are needed on interactive machines.
2. Claude Code / Copilot CLI on an interactive machine can:
   - (A) **Offload whole sessions**: run a complete `claude -p` / `copilot -p` session on a worker (the preferred mode for heavy work);
   - (B) **Remote tools**: the local agent uses MCP to run commands, read/write files, and run builds/tests on a worker;
   - (C) **Interactive attach**: a human on the interactive machine runs `longarm attach` into a PTY on the worker, runs an interactive `claude`, and can reconnect after a disconnect (tmux-style);
   - (D) **Port forwarding**: e.g. a `next dev :3131` on the worker is forwarded to a local browser.
3. Automatic worker selection (admission gate: free memory ≥ 20 GB and CPU < 70%; score `free − 6×jobs`, plus repository cache affinity).
4. The network can be Tailscale or the self-built tunnel; the self-built tunnel relays through a self-hosted public server and prefers P2P.

**Environmental facts that shape the design**
- Interactive machines often sit on a home connection behind **carrier-grade NAT**; cloud workers are often behind **hard NAT**; direct connectivity is intermittent, and relayed paths can add hundreds of milliseconds of latency.
- UDP across long-haul or filtered networks may be throttled or interfered with → the self-built tunnel must have a **TCP/TLS 443 fallback** and cannot rely on UDP alone.
- Workers can "disappear for a few minutes" (cloud VM resizes/reboots) → they must self-heal after restart and must not leave zombie `running` states behind.
- Credentials are **injected per job and never written to the worker's disk**.
- Windows pitfalls: processes started from an SSH session die when it disconnects, scheduled tasks lose stderr, the console's OEM code page garbles output, and npm `.ps1` shims are subject to execution policy — addressed at the root by using a **resident service + Job Objects + ConPTY + explicit UTF-8**.

**Non-goals** (of v1): GPU scheduling, distributed builds across workers, replacing CI. Multi-tenancy was a v1 non-goal; level 2 (semi-trusted users sharing a hub, OS account per user on workers) was added later - see [security.md](security.md).

---

## 2. Overall architecture

```
  interactive machines ×N (Win/Mac/Linux)   public server (longarm-hub)               workers ×10
 ┌───────────────────────────┐          ┌────────────────────────────────┐       ┌──────────────────────────┐
 │ claude / copilot          │          │ control   : registry/certs/    │       │ longarmd (service)       │
 │   └─ MCP: longarm mcp ────┤          │             scheduling/state/  │◄─hb───┤  ├ exec / PTY            │
 │ longarm CLI (run/attach/  │◄─ctrl───►│             audit/panel        │       │  ├ job supervisor        │
 │   ls/forward/ssh)         │          │ rendezvous: candidate exchange │◄relay►│  ├ workspace/git cache   │
 │ longarm-tray              │          │ relay     : ciphertext forward │       │  ├ metrics               │
 └─────────┬─────────────────┘          │ STUN      : reflexive address  │       │  ├ ssh server (compat)   │
           │                            └────────────────────────────────┘       │  └ self-update           │
           │  data plane (direct preferred): (1) tsnet  (2) longnet (QUIC direct → hub relay)  └────────┬─────┘
           └──────────────────────────────── gRPC + mTLS ──────────────────────────────────────────────┘
```

Three principles:
1. **The data plane does not go through the control plane**: exec, log, and file streams are direct gRPC between interactive machine and worker; the hub only does discovery, scheduling, and fallback relaying.
2. **The worker is the source of truth for jobs**: the job directory and state machine live on the worker; what the hub stores is a cache.
3. **Every stream is resumable**: logs and file transfers carry offsets / content hashes, so any disconnect is just "reconnect + continue from offset". This makes path switching (relay → direct) trivial.

---

## 3. Components

### 3.1 `longarmd` (worker daemon)

Runs as a Windows Service (`golang.org/x/sys/windows/svc`, **logged on as a regular user account** rather than LocalSystem, so global paths for git/gh/npm and `%APPDATA%\npm` are correct) or a Linux systemd unit. The service restarts automatically on failure (SCM recovery / `Restart=always`).

Modules:

| Module | Responsibility | Notes |
|---|---|---|
| `transport` | Listens on both the tsnet and longnet Listeners | See §4 |
| `api` | gRPC services: Exec, PTY, Job, Files, Forward, Metrics | mTLS, authorization by client certificate |
| `supervisor` | Spawns child processes, writes logs, maintains the state machine | **One shim process per job** (`longarmd shim <job>`, the containerd-shim pattern): the shim owns the job's Job Object (KILL_ON_JOB_CLOSE + optional memory limit) / Linux cgroup v2 and writes out.log/exit — so a crash, restart, or self-update of `longarmd` itself **does not kill jobs**; after restarting, `longarmd` reconnects to shims via a local named pipe / unix socket. `longarm kill` = tell the shim to close the Job Object, killing the whole process tree (including node/next descendants). stdout/stderr are both persisted to disk (fixing lost stderr from scheduled tasks); encoding problems are avoided by reading raw pipe bytes rather than going through a console; ConPTY is used for interactive sessions |
| `jobs` | Job directory `<data dir>\jobs\<id>\{job.json,state,out.log,result.md,exit}` | Simple, script-readable layout. On startup it **reconciles**: state=running but the pid is gone / the process start time does not match → `lost`; the job's retry policy decides whether to resume (claude `--resume <session-id>` / copilot `--continue`) |
| `workspace` | One bare mirror per repository: `<data dir>\mirrors\<repo>.git`; jobs use `git worktree` | New workspaces in seconds; shared pnpm store / uv cache; LRU disk GC (threshold e.g. C: free < 15%) |
| `metrics` | CPU / memory / top processes / number of agent sessions (kernel32 sampling + leaf-process counting) | Sampled every 2 s and pushed to the hub (piggybacked on heartbeats); also exposes `/metrics` for Prometheus |
| `toolchain` | Aligns tool versions (git/gh/node/pnpm/uv/pwsh/claude/copilot) according to a **profile** pushed by the hub | Declarative and idempotent; upgrades one worker at a time as a canary |
| `sshd` | gliderlabs/ssh, bound to the overlay address only | Compatibility layer: existing scripts, VS Code Remote-SSH, scp |
| `update` | Self-update | Download → verify signature (minisign) → keep old and new binaries side by side → restart the service → automatic rollback if the 60 s health check fails |

### 3.2 `longarm` (interactive-machine CLI + MCP)

```bash
longarm ls                                  # pool status (ask the hub if available; if the hub is down, probe the cached worker list in parallel)
longarm run [--on auto|worker-3] [--repo .] [--ref HEAD] --agent claude --wait prompt.md
longarm run --on auto -- pnpm test          # any command; also a job: can detach / logs / wait
longarm logs <job> [-f] ; longarm wait <job> ; longarm kill <job> ; longarm result <job>
longarm attach worker-3 [--session dev]     # interactive PTY; detach and re-attach to the same session
longarm exec worker-3 -- git status         # short command, returns synchronously (no job created)
longarm forward worker-3:3131 [:3131]       # port forwarding
longarm link create worker-4:8080 --on worker-3   # port link: worker-3's jobs reach worker-4:8080 at 127.0.0.1:8080
longarm sync . worker-3:C:/work/proj        # includes uncommitted changes (see §5.3)
longarm ssh-config >> ~/.ssh/config         # Host longarm-* → ProxyCommand longarm proxy %h
longarm mcp                                 # stdio MCP server for claude / copilot
```

`longarm wait` is **push-based** (a gRPC stream blocks until the job ends). Combined with Claude Code's `run_in_background`, this gives "wake up on completion" instead of polling — and `longarm run` hints at `--wait` semantics by default so agents do not sit idle waiting on finished jobs.

MCP tools (for the agent on the interactive machine):

| Tool | Purpose |
|---|---|
| `fleet_status` | Pool overview (who is idle, who has which repository cached) |
| `fleet_exec` | Run a command on a chosen/automatically selected worker; returns exit code + truncated output (large output goes to the job log and a handle is returned) |
| `fleet_job_submit / wait / logs / kill` | Long-running jobs (including whole claude/copilot sub-agents) |
| `fleet_read / fleet_write / fleet_sync` | Read/write remote files, sync the local workspace over |
| `fleet_forward` | Open a port forward, returns a local URL |
| `fleet_link` | Port links between workers (create / list / remove) |

This lets a local Claude Code have "its brain local and its hands remote": heavy `tsc`, `next build`, and test matrices are dispatched to workers; or 8 sub-agents fan out to 8 machines at once.

### 3.3 `longarm-hub` (public server)

One binary, one SQLite database (continuously backed up with Litestream to object storage), deployed with docker compose or systemd. It contains:

- **control**: node registration and certificate issuance (built-in small CA), worker inventory, heartbeat and metrics aggregation, job index, scheduling, audit log, web panel (tray apps subscribe to the hub's SSE stream instead of probing workers over SSH).
- **rendezvous**: candidate address exchange for longnet (similar to Tailscale disco's CallMeMaybe).
- **relay**: the longnet relay, forwarding **already encrypted** QUIC packets by node ID; two carriers: UDP 443/40000 (fast) and **TLS-over-TCP 443 / WebSocket** (fallback when UDP is interfered with; it looks like plain HTTPS).
- **STUN**: returns reflexive addresses.

Blast radius of a hub outage (designed to degrade, not to fail):
- Running jobs and established direct connections: unaffected;
- New connections: the Tailscale backend works as usual (it has its own control plane); longnet can retry directly using cached peer candidates;
- Scheduling: `longarm` falls back to client-side selection (same algorithm, querying each worker's Metrics directly in parallel);
- Registering new workers / issuing certificates: temporarily unavailable (acceptable).

> Placement: any small cloud VM with a public IP is enough. Prefer a dedicated small instance rather than co-hosting with production services, ideally in a region close to both the interactive machines and the workers. Relay traffic is control/log-sized; large file syncs prefer direct paths.

---

## 4. Network layer

### 4.1 Transport abstraction

```go
type Transport interface {
    Name() string                                      // "tailscale" | "longnet"
    Listen(ctx) (net.Listener, error)                  // used by longarmd
    Dial(ctx, node NodeID) (net.Conn, PathInfo, error) // used by longarm; PathInfo = direct/relay/derp + RTT
}
```

- `longarmd` **listens on both backends simultaneously**, with the gRPC server attached to both Listeners.
- `longarm`'s path policy (configurable, overridable per call): `prefer=tailscale|longnet`, or `race` (dial both, the first to complete the handshake wins, the other is closed). The default is `race` — which naturally turns "which one?" into "which one is better right now?".
- The upper layer is always gRPC + application-level mTLS (certificates signed by the hub CA), so authentication is independent of the network backend; the overhead of double encryption is negligible for this kind of traffic.

### 4.2 Backend (1): Tailscale (`tsnet`)

- `longarmd` embeds `tailscale.com/tsnet` and joins the tailnet as `longarm-<host>` with tag `tag:longarm-worker` using a pre-authorized auth key; **it does not depend on `tailscaled` being installed**, and userspace networking needs neither admin rights nor TUN.
- Benefits from any Tailscale Peer Relays already configured in the tailnet (as long as the relay policy covers the new nodes).
- `ControlURL` is configurable: Tailscale SaaS by default, or a self-hosted **Headscale** (see §4.4).
- `longarm` on interactive machines also uses tsnet (`longarm-client-<host>`), or the local `tailscaled` if one is detected (one node fewer).

### 4.3 Backend (2): self-built `longnet` (a simplified magicsock)

It only serves "application-level streams between interactive machines and workers", so the heaviest parts of Tailscale (WireGuard, TUN, subnet routing, DNS, ACL engine) can be dropped.

**Structure**

```
 gRPC ── quic-go (TLS 1.3, mTLS, multiplexed streams) ── pathConn (custom net.PacketConn)
                                                          ├─ direct: local UDP socket → peer candidate addresses
                                                          └─ relay : hub (UDP datagram forwarding | TLS/TCP framed forwarding)
```

The QUIC connection is **end-to-end** (the certificate is the node identity); `pathConn` only decides which path each packet takes — exactly the core idea of magicsock. With QUIC:
- The relay never sees plaintext and does not need to be trusted;
- Path switching does not require re-establishing the connection (QUIC naturally tolerates 4-tuple changes, and quic-go supports a custom PacketConn).

**Connection setup**

1. Each node keeps a persistent control stream to the hub (QUIC over UDP 443; falls back to TLS/TCP 443) and reports: local LAN addresses, the reflexive address obtained via STUN, and NAT type probe results (whether the mapped port varies by destination).
2. When `longarm` wants to reach worker W, it **immediately establishes a QUIC connection through the relay** (first-packet latency = RTT via the relay, so the user can start right away).
3. Meanwhile the hub sends `CallMeMaybe{candidates}` to both sides, and both sides send disco probes to all of the other's candidates (signed to prevent spoofing, resent every ~200 ms for ~5 s).
4. Once a probe succeeds in either direction → `pathConn` marks that address as direct and subsequent packets go direct; the relay stays as a hot standby.
5. The direct path gets keepalive/RTT measurements every ~2 s; on sustained loss → fall back to the relay and periodically retry hole punching in the background (30 s, backing off exponentially to 5 min).
6. If both sides are behind symmetric NAT → stay on the relay (consistent with Tailscale's default behavior; no port spraying, the payoff is low).

**Relay carriers**
- UDP: the hub forwards datagrams by a `dst node-id` header (TURN-like, but with no allocation flow; authorization is based on the already-authenticated control stream).
- TCP/TLS 443: datagrams are framed into a TLS stream (the DERP approach); QUIC over it suffers head-of-line blocking, but it is only used as a fallback.

**Size estimate**: pathConn + disco ~1.5k lines, hub rendezvous/relay ~0.8k lines, NAT probing ~0.3k lines, plus tests. The designs of `tailscale/wgengine/magicsock`, `tailscale/derp`, and `tailscale/net/stun` can be used as references (not vendored).

### 4.4 The pragmatic "half self-hosted" option

If you want independence from a SaaS control plane without maintaining hole-punching code: run **Headscale + a self-hosted derper** on the hub and point the tsnet `ControlURL` of `longarmd`/`longarm` there. The "own tunnel" then means your own control plane + your own relays, while P2P, hole punching, and reconnection all use Tailscale's mature implementation.

| | Tailscale SaaS | Headscale + self-hosted DERP | longnet |
|---|---|---|---|
| Effort | None | Half a day to deploy | 2–3 weeks + ongoing maintenance |
| P2P quality | Best (years of hardening, Peer Relay) | Same (same client code) | Good enough (standard hole punching + relay) |
| External dependency | Yes | No | No |
| Interference resistance (TCP 443 fallback) | DERP has it | Self-hosted DERP has it | Yes |
| Customizable (per-job QoS, path-aware scheduling) | No | No | Yes |

**Recommendation**: use the Headscale option as the second leg first (lowest cost, immediate reliability gain); treat longnet as optional, worth investing in only when deep customization is needed or to drop the Tailscale client entirely. Architecturally both are just `Transport` implementations and can be added or swapped at any time.

---

## 5. Jobs and data

### 5.1 Job state machine

```
queued ──schedule──► assigned ──longarmd accepts──► preparing(clone/worktree/sync) ──► running ──► done | failed | killed
                        │                                                                │
                        └── worker offline ≥ 90 s and not started → back to queued       └── worker restarts → lost → (retries>0) → running(resume)
```

- Job IDs are **generated by the client** (ULID) → submission is idempotent; network retries never start a job twice.
- State-change events flow: worker → hub (persisted) → subscribers (`longarm wait`, tray, MCP). When the hub is unavailable, `longarm wait` subscribes to the worker directly.
- Resource declarations: `mem_gb` (defaults by type: 2 GB for a claude session, 8 GB for `build`, overridable) and `cpu`; the scheduler deducts reservations so a single machine does not get flooded at once.

### 5.2 Scheduling

`score = free_gb − reserved_gb − 6×running + 10×has_mirror(repo) + 5×has_warm_deps`; among workers that pass the gate (free ≥ 20 GB, CPU < 70%) the highest score wins; if none pass → the job is queued (not failed) with an estimated wait. Label constraints can be added (`os=linux`, `region=eu`).

### 5.3 Code and artifacts

- Preferred: the worker fetches from GitHub into its mirror (credentials injected per job as `GH_TOKEN`).
- Local unpushed commits: `longarm sync` sends a `git bundle` (containing only the objects the worker's mirror lacks; the worker tells us which refs it has).
- Uncommitted changes: additionally ship a `git diff --binary` patch + a tar of untracked files, which the worker applies on the worktree.
- Returning artifacts: when a job ends, the worker can `git push` to a branch (recommended; leaves a natural trail) or `longarm result --fetch` pulls back patches/files.

### 5.4 Credentials

- The interactive machine holds all secrets (Claude settings env, Copilot token pool, gh token) and sends them to the worker over mTLS with the job submission; `longarmd` **keeps them in memory only**, passes them to the child process as environment variables, and discards them when the job ends; they are never written to disk or logs (logs are redacted of known secret values before being written).
- Interactive `longarm attach` sessions work the same way: secrets are injected on attach and cleared when the session ends.

---

## 6. Security

A port that "executes commands" is a remote code execution service, so:

1. **Never listen on the public internet**: `longarmd` only listens on tsnet / longnet (both require a certificate or tailnet identity); the hub only exposes 443 (+ UDP relay port) publicly, and the relay only forwards packets between authenticated nodes.
2. **mTLS + small CA**: the hub issues node certificates (workers 90 days, clients 30 days, auto-renewed); the revocation list is distributed with heartbeats; certificates carry a role (`worker` / `client` / `admin`), the identity URI `longarm://<role>/<name>` and the node's immutable ID (`urn:longarm:node:<uuid>`; names are display names that can change - docs/security.md "Node identity"); revocation is by node ID.
3. **Join tokens**: `longarm admin token --ttl 1h --uses 1` generates a one-time join token; `longarmd join` exchanges it for a certificate. Newly joined workers are `pending` by default, optionally requiring approval in the panel.
4. **Audit**: every exec/attach/job/file write is logged both locally on the worker and on the hub (who, which machine, what command, exit code).
5. **Least privilege**: client certificates can be restricted to worker labels; dangerous operations (uninstalling/upgrading `longarmd`, profile changes) are `admin`-only.
6. Worker process isolation goes as far as OS accounts: the service account (single-tenant), or one account per user with per-user isolation (multi-tenancy level 2, [security.md](security.md)); container-level isolation is level 3.
7. **Port links** (joint debugging across workers): workers get no rights on each other; the hub signs a grant per link (id, owner, from, to, ports, expiry) with its CA-issued hub certificate and pushes each worker the authoritative list of links naming it; the target worker accepts a worker's `LinkTunnel.Dial` only with a grant naming both and a link still in its list, and checks the target port for the owner like `Forward.Open`; the relay carries worker ↔ worker traffic only for linked pairs ([security.md](security.md#port-links)).

---

## 7. Deployment and operations

### 7.1 Adding a worker (goal: one command, five minutes)

```powershell
# Windows (elevated PowerShell)
irm https://hub.example.com/install.ps1 | iex; longarmd join --hub https://hub.example.com --token <one-time-token> --profile dev-windows
```
```bash
# Linux
curl -fsSL https://hub.example.com/install.sh | sh -s -- join --hub https://hub.example.com --token <token> --profile dev-linux
```

What `join` does: generate the node key → exchange for a certificate → join the tailnet (the hub issues a tagged auth key) → install and start the service → install the toolchain per profile (git/gh/node/pnpm/uv/pwsh7/claude/copilot, with known Windows pitfalls such as the MSI-installed pwsh, ExecutionPolicy, and `administrators_authorized_keys` handled internally) → self-test (run `claude --version` and `copilot --version`, write a 1 GB file to test the disk) → report `ready`.

### 7.2 Updates

- The hub hosts releases (a mirror of GitHub Releases, signed with minisign).
- Rolling: 1 canary first → healthy for 30 min → the rest in batches. `longarmd` itself can restart at any time (jobs live in shims and are unaffected).
- Toolchain profiles work the same way (e.g. upgrading the claude CLI), but a toolchain upgrade **waits until that worker has zero jobs** (replacing a running exe fails on Windows); meanwhile the scheduler marks it as draining and assigns no new work.

### 7.3 Observability

- Hub panel: node list / path type (direct/relay/derp + RTT) / resource charts / job list / audit.
- Tray (`longarm-tray`): subscribes to the hub's SSE stream; when the hub is unreachable it falls back to each worker's Metrics stream directly.
- Alerts (tray notifications + optional email): worker offline, memory ≥ 90%, disk < 10%, job lost, certificate about to expire.

### 7.4 Reliability checklist

| Failure | Behavior |
|---|---|
| Interactive machine loses network / sleeps | Jobs keep running; after resuming, `longarm logs -f` continues from the offset |
| Direct path breaks | longnet falls back to the relay within seconds; tsnet falls back to peer relay/DERP; the application layer does not notice |
| An entire network backend is unavailable | The `race` policy automatically uses the other one |
| Hub outage | See §3.3: the data plane is unaffected, scheduling falls back to the client side |
| Worker restart/resize | Service autostarts → reconcile → `lost` jobs resume per policy → notification |
| `longarmd` crash / self-update | The service manager restarts it; jobs are held by their own shims and are unaffected; after restarting, `longarmd` enumerates shim sockets and reclaims them. If a shim itself died (rare) → decide by pid + start time: if the process is still alive, reclaim read-only; otherwise mark `lost` |
| New `longarmd` version has a bug | Failed health check triggers automatic rollback; canaries limit the blast radius |
| Disk full | LRU GC of worktrees/mirrors; the scheduler avoids nodes low on disk |

---

## 8. Migrating from ad-hoc SSH setups

1. `longarmd` uses a simple, documented job directory layout, so existing scripts that inspect job output can keep working during a transition (through `longarmd`'s built-in sshd or the system OpenSSH).
2. A hand-maintained host list can be imported into the hub's node table once; agent skills/instructions that shell out over SSH are rewritten to call `longarm` (or use the MCP tools).
3. Monitoring that probes workers over SSH is replaced by the hub's SSE stream or `longarm-tray`.
4. Existing workers are migrated by re-registering them with `longarmd join`; OpenSSH can be kept as an emergency channel.

---

## 9. Implementation phases

| Phase | Scope |
|---|---|
| **P0** | `longarmd` (service, Exec/PTY/Job/Files/Metrics, reconcile, sshd compatibility) + `longarm` CLI; Tailscale backend only; mTLS with manually issued certificates |
| **P1** | `longarm-hub` control (registration/CA/join codes/heartbeats/scheduling/job index), local control API, `longarm mcp`, one-line install scripts, toolchain profiles |
| **P1.5** | UI (§11.3): shared web frontend → hub panel + `longarm-tray`; GoReleaser producing MSI / pkg / deb / rpm / docker (§11.2) |
| **P2** | Second network leg: Headscale + self-hosted derper deployed with the hub, `race` path selection; self-update + canaries; Windows/macOS code signing |
| **P3 (optional)** | longnet: relay first (TCP/UDP) → hole punching → path switching; path-aware scheduling |

---

## 10. Design decisions and defaults

1. **Where the hub runs**: by default a dedicated small VM with a public IP; co-hosting with other services is possible but not recommended.
2. **Which second leg first**: by default Headscale + derper first, longnet later; if full independence/customizability matters more, build longnet directly (relay first).
3. **Whether joining workers need manual approval**: required by default (one click in the panel); can be relaxed to "a valid token is trusted".
4. **Default artifact return**: `git push` to a `longarm/<job-id>` branch.
5. **Windows installer signing**: signed releases avoid SmartScreen warnings; unsigned builds still install, the user just has to click "Run anyway" the first time.
6. **Tray technology**: Wails v3 by default (Go + the system WebView, frontend shared with the hub panel), validated by a short spike (tray + popup window on all three platforms); if it falls short, fall back to `fyne-io/systray` + a local browser page.

---

## 11. Platforms, packages, and UI

### 11.1 Platform support

Everything is Go in a single codebase; GoReleaser builds all targets in one pass (windows/amd64+arm64, linux/amd64+arm64, darwin/arm64+amd64). Platform differences are confined to a few small interfaces (`internal/platform/{windows,linux,darwin}`):

| Capability | Windows | Linux | macOS |
|---|---|---|---|
| Resident service | SCM service (logged on as a user account) | systemd unit | launchd (LaunchDaemon, `UserName` set to the user) |
| Job process-tree isolation / whole-tree kill | Job Object | cgroup v2 (falls back to process groups) | Process group + `setrlimit` (**no reliable memory limit**; can only monitor and kill when exceeded) |
| Interactive terminal | ConPTY | pty | pty |
| Local control API channel | Named pipe (ACL: current user/administrators only) | Unix socket (0600) | Unix socket (0600) |
| Metrics collection | kernel32 | /proc | `host_statistics` / sysctl (gopsutil) |

Support level per role:

| | Windows | Linux | macOS |
|---|---|---|---|
| `longarm` (interactive machine) | ✅ Primary | ✅ | ✅ Primary |
| `longarmd` (worker) | ✅ Primary | ✅ Primary | ✅ Supported (e.g. a Mac on the LAN can join the pool to run iOS-related work), but memory limits are weak |
| `longarm-hub` | Runs (for development) | ✅ **Deployment target** (docker / deb) | Runs (for development) |

A machine can **be both an interactive machine and a worker** (e.g. a primary workstation that picks up work when idle); roles are just a matter of which components are installed + configuration switches.

### 11.2 Packages: one-click install

**One package installs all client-side components** (`longarm` + `longarmd` + `longarm-tray`), with roles chosen at install time; the hub is released separately.

| Platform | Package | One-click | Headless/bulk |
|---|---|---|---|
| Windows | `longarm-x.y.z.msi` (WiX, built by GoReleaser); winget package | Double-click → choose role (client / worker / both) → afterwards the tray prompts "enter join code" | `msiexec /i longarm.msi /qn ROLES=worker HUB=https://hub… JOIN_CODE=K7P-Q2M` |
| macOS | `.pkg` (signed with an Apple Developer ID + notarized); Homebrew tap | Double-click the pkg → same as above | `brew install <tap>/longarm && longarmd join …` |
| Linux | `.deb` / `.rpm` (nfpm) + `install.sh` | — | `curl -fsSL https://hub…/install.sh \| sh -s -- --roles worker --join K7P-Q2M` |
| hub | docker image + compose file; also a `.deb` | `docker compose up -d` | Same |

**Join codes (replacing long tokens)**: click "Add machine" in the hub panel → a 6-character, 10-minute, single-use join code is generated (underneath it is still the §6 join token, just human-friendly). After installing, enter the join code in the tray (or CLI `longarmd join K7P-Q2M`) → certificates are exchanged → the node appears in the panel pending approval → approve → the toolchain is installed per profile → `ready`. The panel also shows the Windows/Linux one-liner for that machine, convenient for installing remotely over SSH.

The packages contain no secrets; the hub address can be baked in as a default at build time (for self-built distributions) or entered at install time.

Upgrades: after installation, `longarmd` self-update (§7.2) takes over and the package does not need to be run again; packages only handle first install and uninstall (uninstall runs `longarmd leave` first to revoke the certificate).

### 11.3 UI: decoupled from the core

**Principles**

1. **The core is always headless**: `longarmd`, `longarm`, and `longarm-hub` link no GUI libraries; Linux servers, Windows Server Core, and SSH-only environments work fully with just the core installed.
2. **The UI is only an API client**: UI processes are never on a data path — closing, crashing, or not installing the UI affects no job, connection, or scheduling.
3. **Every UI action has a CLI equivalent**: the UI calls the same APIs the CLI uses (local control API or hub API); there is no "UI-only" feature, which also lets agents do the same things via MCP.
4. **One frontend, two shells**: the same web frontend (TypeScript + component library) serves as the hub's web panel and is embedded in the tray app, so nothing is written twice.

**API layering**

```
  longarm-tray (desktop)  ──┐                                   ┌── hub web panel (browser)
  longarm CLI / MCP       ──┼─► local control API               │
                            │   (pipe / unix socket)            │
                            │   longarmd: local worker state    │
                            │   longarm : local client state    │
                            └──────────────► hub API (HTTPS + cert/login) ◄──┘
```

- **Local control API**: `longarmd` and `longarm` (the client session daemon, which owns long-lived connections such as port forwards and notification subscriptions) each expose one, reachable only from the local machine; gRPC + the same proto, plus gRPC-gateway JSON/SSE for the web frontend.
- **Hub API**: the whole-pool view; panel login uses either an admin certificate (imported into the browser) or a hub-local account + TOTP.

**What each interface covers**

| Interface | Runs on | Features |
|---|---|---|
| `longarm-tray` (worker mode) | Worker desktop | Join/leave the pool (enter join code), **pause taking work (drain) / resume**, local resource limits (max N jobs, reserved memory), running jobs and logs, path status (direct/relay/RTT, which leg is in use), toolchain versions and "upgrade now", local audit log |
| `longarm-tray` (client mode) | Interactive machine desktop | Pool overview, CPU/memory charts, Claude/Copilot session counts, my jobs list + completion/failure notifications, one-click attach (opens a terminal), port-forward management (open/close, open in browser), hub and network backend selection (tailscale / longnet / race) |
| Hub web panel | Browser | All nodes (approve joins, labels, revoke, drain, remote upgrade), all jobs (filter, logs, kill), join code generation, toolchain profile editing and staged rollout, certificates and audit, network topology (current path for each node pair) |

Workers and clients use **the same tray program**, which shows tabs for whichever roles are installed locally; when both are present they are merged.

**The tray's look** follows our desktop apps (TMIA Desktop): calm warm neutrals, one deep-teal accent, the system font, an 8 px grid, status as small dots and pills. Its design tokens are `web/src/tray/tokens.css` (light and dark), its components `web/src/tray/tray.css`; the hub panel keeps its own dark palette. The theme follows Windows' app mode (`prefers-color-scheme`) unless Settings > Appearance picks Light or Dark (kept in the tray's `tray.json`). In the ~460 px popup the tabs are a segmented bar under the title; a window resized to 760 px or more gets a left navigation rail and the machine cards in columns. `pnpm shots` (web/shots, with demo data; CI's `tray-screenshots` artifact) renders every tab in both themes and sizes.

**Technology choice**: the tray shell defaults to **Wails v3** (native Go, uses the system WebView — WebView2 on Windows, WKWebView on macOS, WebKitGTK on Linux — small package size, tray APIs on all three platforms). It is in alpha/beta, so it was validated with a short spike first; the fallback is `fyne-io/systray` (a mature tray-only library) + opening a local page in the default browser.
