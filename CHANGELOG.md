# Changelog

## Unreleased

### Added

- **Agent sessions in the tray**: Claude Code / Copilot CLI on a worker's
  card opens a Sessions window listing every agent session of your workers
  (working, waiting for input, ended, outside Longarm), each in a tab with
  its live terminal (xterm.js over a WebSocket bridge in `longarm daemon`
  to `Sessions.Attach`), its conversation (the transcript, parsed and
  followed live) and a steer box (`Sessions.Send`, only when nobody else is
  typing); new sessions and "continue here" (resume / fork) of agents
  started outside Longarm. Works with existing workers. docs/usage.md
  "Agent sessions in the tray", docs/agent-sessions-ui.md.
- The tray shows its version in the title bar ("v0.1.21"): hover for a
  summary, click for the details - this tray, the client daemon, this
  machine's worker and admin worker, the hub and the newest release at the
  hub - with whatever is behind marked (the tray behind the newest release,
  a daemon or worker older or newer than the tray: not restarted, or not
  updated) and "Update to X" when the hub has a newer release. The hub's
  version comes from its public `/healthz`.
- **Signed apt and dnf/yum repositories** on GitHub Pages
  (`https://lubobill1990.github.io/longarm/`): `apt install longarm` /
  `dnf install longarm`, updates through the package manager. The rpm is
  GPG-signed (nfpm), the apt Release file (InRelease, Release.gpg) and the
  yum repomd.xml too, with the key "Longarm Release Signing" (secret
  `LONGARM_GPG_PRIVATE_KEY`; public key packaging/linux/repo/longarm.asc).
  Rebuilt on every stable release (the newest 3 versions), installed from
  in Debian, Fedora and AlmaLinux containers before it is deployed.
  docs/linux-repos.md.
- Sign-in with **Google, Apple and personal Microsoft accounts**
  (`[auth.google]`, `[auth.apple]`, `[auth.microsoft]` in hub.toml), next to
  Entra work and school accounts: the panel shows one button per provider,
  and `/link` - behind `longarm login`, `longarmd login`, the tray and the MSI's
  `LOGIN=1` - signs in the same way. The account (provider + subject) maps to
  one Longarm user, created on first sign-in, so the clients and workers one
  account links are that user's together. `/auth/info` lists the providers;
  the tray and the CLIs no longer say "Sign in with Microsoft" only.
  docs/usage.md "Sign in with a Google, Apple or personal Microsoft account".

- **Signed and notarized macOS releases**: the release builds
  `longarm-darwin-arm64.pkg` and `longarm-darwin-amd64.pkg` (longarm and
  longarmd in `/usr/local/bin`), signed with a Developer ID Installer
  certificate, notarized and stapled, so MDM-managed Macs install them; the
  darwin binaries (also the self-update files and those in the archives) are
  signed with a Developer ID Application certificate and the hardened
  runtime. A new job, build-macos, does it before the checksums and minisign
  signatures are made; without the `APPLE_*` secrets the files stay unsigned
  with a warning. The pkg's postinstall also reads a configuration profile
  (`com.longarm`), validates its values and restarts an installed
  LaunchDaemon on upgrades. docs/ci-cd.md 2.8 (credentials), docs/usage.md
  "macOS installer (.pkg) and MDM".

### Security

- Hub admins no longer see other users' machines. With multi-tenancy an
  admin certificate or an administrator's panel login sees machines and jobs
  exactly as its user does: `longarm ls` / `ls -o wide`, `worker ls`,
  `jobs`, `sessions`, the MCP tools, the tray's Workers tab, `longarm admin
  nodes`, the hub's event stream and the panel's "Nodes" and "Jobs" pages
  show the admin's own workers and devices, the workers shared with it and
  the fleet workers - before, an admin saw every user's private workers with
  process lists, agent sessions and jobs. The rule is the hub's, so it holds
  for every client version; there is no flag or toggle that widens it. The
  only fleet-wide view is the new anonymous summary (`longarm admin summary
  [--rows]`, the panel's "Fleet" page, `GET /api/admin/summary`,
  `HubAdmin.FleetSummary`; admins only, audited): per user the number of
  workers, admin workers and devices, how many are online, the workers'
  version histogram, and optionally each user's machines by version -
  aggregates only, never names, IDs, host names, OS, last-seen times,
  labels, CPU / memory, processes, sessions or jobs. Admin answers that named other users'
  machines are cut down the same way: operations by name (approve, revoke,
  drain, label, share, profile, remove, tailnet rekey, toolchain apply) on
  another user's machine answer "not found" word for word like an unknown
  name and do nothing; admins never share another user's worker; a worker
  certificate never acts as a user (whatever its name) and node names never
  take a user's; an admin's join code or panel login for a user is for a
  user without machines only; user listings drop other users' reserved
  memory; `admin user ls` / "People" and `admin
  user remove` give device and worker counts instead of names; join codes,
  rollouts (counts plus visible names), toolchain canaries, `admin tailnet
  status` and `admin links` name only machines the admin may see; a release
  push no worker can take fails with counts, not names; join codes are the
  admin's own, its user's and fleet worker codes, with `used_by` "used"
  unless the machine is visible; `link ls --all` and the panel's port links
  are the admin's own user's links; `admin logs` of another user's job is
  "not found". The audit log (`admin audit`, the panel, `Fleet.Audit`)
  shows only the records about the admin's own user and the hub actions it
  performed, and counts the rest by method; `users/remove` records count the
  revoked machines; reads go by the acting and affected user recorded when
  each record was written (schema migration 5), never by today's node
  ownership, so re-enrolling a revoked name doesn't reopen its records.
  Every outbound record type and every event kind goes through one
  projection (`internal/hub/projection.go`): actors appear as role and user
  ("admin bob"), never by device name - event messages, join codes, users,
  jobs' submitter, audit records; profile updates, rollouts and tailnet
  status are projected like listings; an admin's kill of another user's job
  is "not known" (checked before resolving its worker) and removing another
  user's link answers with its id only. The complete trail stays in the
  hub's database and log for the hub's operators (new `longarm-hub audit` on the hub host; on
  Azure, the subscription's owners). Auto placement already
  used only workers the caller's user may use, admins included; the admin's
  `exec auto` now picks from the same list (before, from every worker the
  hub listed to the admin). See docs/security.md "What admins see".
- The same holds for accounts of every sign-in provider (Entra, Google,
  Apple, personal Microsoft): an account is one user whatever its role, the
  same e-mail address at two providers is two users. An admin can't add its
  own user to a team; an admin's user-bound join code or panel login is
  refused for a user who signs in with a provider or has another user's
  worker shared with it (checked again when the code is used), and a panel
  administrator login can't take the name of any user (old certificates,
  worker owners included). Open event streams follow team changes at once.
  `/link` audit records name the provider (`google/<e-mail>`).

### Fixed

- Windows: the tray's "Update to ..." (and a user-session longarmd's machine
  update) refused the real Longarm Update service: "the update pipe is
  served by another program ... (pid N: Access is denied.)". The pipe
  server was checked by opening its process, which a user can't do to a
  LocalSystem service. Now the pipe server's PID must be the service's
  current PID as the service manager reports it, the service configured as
  LocalSystem running the installed `longarm-update.exe`, and the pipe
  owned by SYSTEM or Administrators - readable by any user; a squatter is
  still refused (docs/security.md "Update service").
- Signing in on a machine that is signed in already (a join code's
  `admin/<user>-<machine>` with its worker, an earlier sign-in, another
  hub) failed in the tray with "~\.longarm\tls already holds a certificate
  (use --force to enroll again)". The tray now lists what the machine is
  signed in as and what changes, and signs in again only when you confirm
  ("Replace and sign in"; the Account tab also offers "Sign in with an
  account..."). One approval replaces the device and the worker: the
  pairing proves their keys, the hub removes them and the worker keeps its
  name. Ownership never moves: another user's worker is removed, not given
  to you; your own keeps its sharing. `longarmd login --force` does the
  same; an expired sign-in is replaced without asking (docs/security.md
  "Signing in again").
- The hub on Azure crash-looped on every start (2026-10-29, 0.1.18 and
  0.1.16 alike): `litestream restore` ran out of memory. Litestream 0.5.17's
  Azure Blob client listed "the files from TXID X" as the blob name prefix
  X, so every compaction took exactly one file of the level below and fell
  further behind with every write: L0 files were never deleted, L2/L3 never
  moved, and the replica grew to 21,400 files. The restore opens every file
  since the newest snapshot at once (about 100 KB each), passed 1.5 GB
  (`--size small`) and was killed. The image now builds Litestream from the
  upstream commit with the fix (benbjohnson/litestream#1461, unreleased
  after 0.5.17), the Litestream configs spell out levels, L0 retention and
  hourly snapshots kept 24 h, and the image smoke test checks that
  compaction merges files. A replica that has already grown needs a
  bigger start once or a prune (deploy/azure/README.md "Replica size and
  memory").
- A failed restore now says why: `litestream restore killed by SIGKILL
  (out of memory? the container has 1536 MB)` (the cgroup's limit, and
  "the kernel's OOM killer ended it" when the cgroup counted it), and the
  entrypoint exits 128 + the signal (137) instead of 1, which ACI keeps
  across restarts. With `--storage-auth identity --diagnostics` the failure
  is also recorded in the diagnostics container (`hub-start-failures/`).
- deploy.sh / deploy.ps1: an update in place that fails (the 0.1.18 deploy:
  "no bootstrap values from this start", exit 1, the hub left
  crash-looping) is rolled back to the running configuration like a failed
  `--recreate`. While waiting they print every restart of the hub
  container with the exit code of the start before and stop after 3 (2 in
  CrashLoopBackOff) with "the hub is crash-looping"; before deploying they
  print the replica's files per level and warn above 5000 in levels 0-3.
  `--log-analytics` with `--telemetry` sends the container logs to the
  telemetry workspace (no second workspace); README "Logs of crashed
  starts" says how to get the log of a start ACI restarted.
- "Update to X" on a Windows MSI worker failed with "this installation
  can't update itself: C:\Program Files\Longarm is not writable for this
  account (package-managed install?)" although the machine has the Longarm
  Update service: the user-session longarmd (and the tray) asked the
  service manager for full access to look the service up, which a
  non-elevated user doesn't get, concluded there was none and tried to
  update itself. The service query now asks only for
  `SERVICE_QUERY_STATUS`; a Windows longarmd that can't update itself tries
  the service's pipe anyway (checked to be the real service) and, with no
  service, says to run the new MSI once instead of "package-managed
  install?". `longarm worker update` (the tray's button too) hands a
  refused update of an older user-session worker to the machine's admin
  worker, so 0.1.15 to 0.1.18 workers update from the client. Workers
  report whether the update can be installed remotely
  (`UpdateAvailable.method`): the button shows only when it can.
- After a Windows upgrade the tray ran the new version but the client
  daemon kept running the old one ("longarm-tray 0.1.17 · longarm daemon
  0.1.15"). `longarm daemon start` (and every command that starts the
  daemon) now replaces a running daemon older than its `longarm` - its port
  forwards move over - and the tray runs it when it finds an older daemon;
  a newer daemon is never replaced by an older `longarm` (an app's bundled
  copy just uses it). An exiting daemon removed `daemon.addr` even when a
  daemon started right after it had written its own ("daemon is not
  running" while it ran): the file is now removed before the channel
  closes and only while it is still the exiting daemon's (longarmd's too).
- Hub redeploys cost less: workers (and the `longarm daemon` event relay,
  hence the tray) retry an unreachable hub every 1, 2, 4, then 5 s (±20%
  jitter; up to 15 s after 3 minutes, 30 s when the hub refuses the node)
  instead of backing off to 30 s, so they are back within seconds of the new
  hub answering rather than up to 30 s later. A stopping hub announces the
  restart (a new, additive `HubCommand.restarting`, and a `hub` event for
  `Fleet.Events` subscribers that ask for it): workers close their sessions
  at once (the hub's shutdown no longer waits 3 s for them), log it as a
  restart rather than a failure and retry after about a second; the daemon
  forgets the remembered hub failure as soon as the hub answers again.
  Litestream takes hourly snapshots (was daily), so the restore on every
  container start reads fewer files. The container entrypoint and the hub
  log their startup phases (`restore took`, `bootstrap took`,
  `startup timing`), deploy.sh / deploy.ps1 print the redeploy phases and
  poll faster, the container smoke test prints the restart timings, and an
  e2e test restarts the hub under N workers and bounds their return
  (deploy/azure/README.md "Redeploy downtime").
- Windows MSI upgrades failed with error 2613 ("RemoveExistingProducts
  action sequenced incorrectly") for every upgrade by 0.1.8 to 0.1.13: the
  elevated stop step (upgrades with Longarm running) was a deferred action
  before RemoveExistingProducts, which Windows Installer refuses when the old
  product is removed inside the transaction. The new version now installs
  over the old one and removes it afterwards (`afterInstallExecute`; the
  components keep their GUIDs, and no released version's uninstall steps run
  during an upgrade), the stop step runs before StopServices / InstallFiles.
  An upgrade that would switch the worker between a service and the
  user-session worker is refused up front (uninstall first). CI now checks
  the built sequence (and shows that 0.1.13 fails the check) and really
  installs, repairs, upgrades (from 0.1.6 and the latest release, with the
  tray and the workers running) and uninstalls the MSIs on windows-latest.
- Windows: the firewall fix command pastes into PowerShell. The tray's
  warning, `longarm doctor`, `longarm firewall status` and the relay
  warnings printed `"C:\...\longarm.exe" firewall allow`, which PowerShell
  rejects (`Unexpected token 'firewall'`); a quoted path now gets the call
  operator (`& '...' firewall allow`, one helper for every printed command).
  Paths are single-quoted literals with every quote PowerShell recognizes
  (' and the typographic ‘ ’ ‚ ‛) doubled, so no directory name - `$`,
  backticks, “ ” „ - can end the argument and inject a command into an
  elevated shell; paths with control characters get no command. Doctor's
  remarks are shell comments (`# elevated`), and the tray quotes commands
  from older daemons again. The tray's firewall card has an **Allow**
  button: one administrator prompt runs the installed longarm.exe's
  `firewall allow` for the program the daemon or longarmd reports (only an
  existing longarm.exe / longarmd.exe), then checks again; a copy another
  app ships (TMIA Desktop's) is pointed out as such.
- deploy/azure: `--recreate` can no longer take a hub down. A live
  `--headscale --recreate` deleted the container group, then ACI couldn't
  pull the sidecar from Docker Hub, and the retry (no group left to read the
  settings from) fell back to storage keys, which the subscription's policy
  denies. Now `deploy.sh` / `deploy.ps1` validate the deployment (`az
  deployment group validate`) and check that every image can be pulled
  before changing anything (`--skip-image-check` skips the latter); save the
  hub's settings in the resource group's tags before the delete; read a
  missing group's settings from the last successful deployment, with only
  gaps filled from that (untrusted) snapshot and its security-relevant
  settings refused unless they match or are given explicitly, else stop
  and ask for them (never defaulting storage auth);
  and roll back to the previous configuration when the new one fails after
  the delete.
- deploy/azure pulls nothing from Docker Hub: the Caddy and Headscale images
  are mirrored by digest to `ghcr.io/lubobill1990/longarm-mirror` (a
  separate package, which must be made public once), as tags derived from
  the source and digest (`deploy/azure/mirror-images.txt`,
  `.github/workflows/mirror.yml`: allow-listed sources, pins verified
  read-only on pull requests, published from main only with a pinned
  crane, the mirror verified anonymously), and the template refers to the
  mirrors.
- Workers signed in by any path (longarmd login, the tray, the MSI's
  session and admin workers) listened on loopback only and were unreachable
  from other machines. A worker whose listeners are all loopback now also
  listens on the hub's longnet relay (announced in each hub session:
  existing installs pick it up when longarmd restarts or updates), with
  hole punching for a direct path; `[hub] relay = "off"` opts out.
- Clients skip a remote worker's loopback endpoints (and say why when
  that's all it has); the relay's TLS fallback uses the hub's TCP port.
- The tray's ssh buttons run `longarm ssh <worker>` (new): OpenSSH with
  longarm's ProxyCommand built on the fly, no ~/.ssh/config entries.
- Tray worker cards say where CPU/MEM come from and how old they are
  (grayed out when stale), and "loopback only: unreachable from here"
  instead of a bare "down".

### Security

- The Longarm Update service (`LongarmUpdate`, every MSI install) installs
  a Longarm MSI only when it comes from the hub recorded in HKLM (never one
  named in a request), is signed with the release key built into the
  service (the trusted comment naming that file and version), matches the
  hub's sha256/size record, is strictly newer than the installed version
  and is for this machine's architecture. Its pipe is created as the first
  instance; clients check that the server is the installed service running
  as SYSTEM. Remote updates take a certificate of the worker's owner, end
  to end: the hub has no command that triggers one and can't turn
  automatic updates on (docs/security.md "Update service").
- MSI custom actions take their values as strictly parsed data
  (`install-helper msi <step> #LONGARM-MSI#k=v|...`), never as options; run
  only the installed, administrator-only binaries; refuse a
  non-administrator's repair or change, a service or admin worker set up
  for a non-administrator, and tasks for other users (docs/security.md
  "Installer"). `DATADIR` must be below `%ProgramData%\Longarm`; the admin
  worker's service name and dirs are fixed (no `ADMIN_*` properties).
- Data dirs planted in ProgramData before the install are moved aside and
  recreated; the admin worker pins the hub CA itself, records it in
  `HKLM\Software\Longarm\Provenance` and refuses to serve with another.
- The admin worker ignores hub-initiated toolchains, updates, port links,
  relayed job requests and purges; the hub refuses toolchain profiles and
  rollouts for it.

### Added

- Direct-first clients while the hub is down: the daemon's pool streams
  metrics directly from the workers first (up to 32,
  `LONGARM_POOL_MAX_DIRECT`; jobs first, then the fastest) and uses the
  hub's only for the others; it idles without a viewer for 5 minutes (no
  direct streams, probes every 30 s). `longarm ls` uses the daemon's direct
  metrics and shows their source and age in `-o wide` (METRICS). A
  forwarded connection opened while the worker is briefly unreachable waits
  for it (up to 30 s, `LONGARM_FORWARD_WAIT`) instead of failing. The hub
  being down reads "hub unavailable; direct connections to workers still
  work" (CLI, tray). An e2e measures attach, a log stream, a port forward
  and the pool view through a hub restart and a relay restart.
- External relays: relayed connections that survive hub restarts
  (docs/usage.md "External relays"). `longarm-relay --hub <hub> --advertise
  <host>` registers with the hub (`HubNode.RelaySync`, role relay), which
  keeps it in its database and advertises a relay set - external relays
  first, its embedded relay last - to workers in the welcome and to running
  sessions on every change. Loopback-only workers listen on every relay of
  the set (cached in `relay-set.json`, used when the hub is down at start);
  an external relay's endpoint carries its TLS fallback (`&tcp=`), which
  clients use. The relay enforces the hub's revocation list and port-link
  pairs on its own, cached on disk (pairs for 24 h without the hub; a relay
  without a list refuses sessions), closes revoked nodes' sessions, renews
  its certificate through the hub (keeping its host names), and serves
  `/healthz` (`longarm-relay healthcheck`). `longarm admin relays` shows the
  set and how many online workers still only know the embedded relay (older
  versions, by version). Older nodes are unaffected: they ignore the new
  fields and keep the embedded relay, which stays; nobody signs in again.
- deploy/azure: `deploy.sh --relay` / `deploy.ps1 -Relay` deploys the
  external relay as its own container group `<name>-relay` (relay.bicep:
  7445/udp, 7446/udp, 7443/tcp; 0.5 vCPU / 0.5 GB, about 16 USD/month) with
  the hub's image; its certificate comes from the hub's CA
  (`longarm-hub-relay-cert`, `longarm-hub issue --role relay --host ... --b64`)
  as a secure parameter. Validated first, waits for the relay's registration
  with the hub, rolls an upgrade back to the previous relay image;
  `--relay-remove` deletes it. The hub image now contains `longarm-relay`.
- Agent credentials (`--agent-auth`, capability `agent-auth`):
  - When longarm starts Claude Code or Copilot CLI on a worker, it
    forwards the agent's credentials from the client with the job:
    `run`, `exec`, `session start`, `attach` (when it creates the
    session), and the MCP job tools (`agent_auth`, limited by
    `LONGARM_MCP_AGENT_AUTH`). Workers need no agent logins, and nothing
    is stored on them.
  - `--agent-auth auto|claude|copilot|gh|none`, repeatable or
    comma-separated. The default is `auto`: forward when `argv[0]` is
    `claude` or `copilot`. `[agent_auth] default = "none"` in workers.toml
    turns this off.
  - Claude Code's credentials come from the `env` block of
    `settings.json` (honours `CLAUDE_CONFIG_DIR`; tokens and keys go as
    secrets, the rest as plain env) and from `ANTHROPIC_API_KEY` /
    `CLAUDE_CODE_OAUTH_TOKEN`. A subscription's refresh token is never
    copied: longarm suggests `claude setup-token` instead.
  - Copilot's token comes from `COPILOT_GITHUB_TOKEN` / `GH_TOKEN` /
    `GITHUB_TOKEN`, else `[agent_auth] copilot_gh_user` (`gh auth token
    --user`), else `gh auth token`, else the token pool
    `~/.copilot/accounts/tokens.json` (`--copilot-account`, or
    round-robin).
  - `--agent-auth gh` (opt-in) forwards `GH_TOKEN`.
  - On the worker, the credentials are handled like `--secret-env`: in
    memory only, in the process environment, and redacted from logs and
    session transcripts. `jobs --json` / `sessions --json` (`agentAuth`),
    `job.json` and the audit log name the agents only.
  - Forwarded Copilot credentials come with a private `COPILOT_HOME` per
    job, session or exec (`<data>/agent-home`), removed with the job.
  - longarm refuses to forward to somebody else's worker with shared
    isolation unless `--agent-auth-shared-ok` is given, and forwards to
    admin workers only when the agent is named. `Node.Info` reports the
    worker's `owner` and `admin_worker`.
  - With end-to-end trust on the client, a worker counts as yours only
    if your own user key endorsed it. The hub's ownership records, and the
    worker's hub-pushed owner, don't count. Isolation and the admin flag
    are the worker's own report.
  - The forwarding decision is pinned to the checked node (its node key,
    else its certificate) for the whole operation. A reconnect to another
    node fails before anything is sent. `attach` sends credentials only in
    the open that creates the session, never on a re-attach.
  - Docs: docs/usage.md "Agent credentials" (with recommended unattended
    invocations), docs/security.md "Forwarded agent credentials".

- A self-hosted Tailscale control plane next to the hub on Azure Container
  Instances: `deploy.sh --headscale` / `deploy.ps1 -Headscale` (bicep
  `enableHeadscale`, kept on re-runs; switching it needs `--recreate`) adds
  a Headscale v0.29.5 sidecar (pinned image digest) and makes it the hub's
  tailnet provider. The hub serves Headscale's node endpoints (`/ts2021`,
  `/key`, `/derp`, ...) on its own https port with its certificate; nothing
  else of Headscale is public and the group gets no new port. The hub
  writes Headscale's config, ACL policy and DERP map, keeps its Noise and
  DERP keys and its own API key in the hub's database (created over
  Headscale's unix socket: no manual step, no secret in the environment or
  the logs), and Litestream replicates Headscale's database to the blob
  container `headscale` (managed identity or SAS). Relayed traffic stays on
  the hub's embedded DERP (`--headscale-public-derp on` adds Tailscale's);
  STUN comes from the external STUN servers the hub already advertises
  (ACI source-NATs inbound UDP and allows 5 ports per IP). Existing nodes
  join the tailnet on their next hub session. `/healthz` and `longarm admin
  tailnet status` report Headscale's state; deploy.sh prints it.
- hub.toml `[tailnet.headscale] proxy`, `sidecar_dir`, `public_derp` and the
  environment overrides `LONGARM_HUB_TAILNET_PROVIDER`,
  `LONGARM_HUB_HEADSCALE_URL`, `LONGARM_HUB_HEADSCALE_API_URL`,
  `LONGARM_HUB_HEADSCALE_API_KEY_FILE`, `LONGARM_HUB_HEADSCALE_DIR`,
  `LONGARM_HUB_HEADSCALE_PROXY`, `LONGARM_HUB_HEADSCALE_PUBLIC_DERP`.
- `TailnetKey` refuses with Unavailable, without charging the node's rate
  limit, while the provider can't mint yet (a starting sidecar).
- deploy/azure/test/acirules checks ACI's limit of 5 ports per IP address.
- Updating machines from your client: `longarm worker update <worker>
  [--latest | --version X] [--wait] [--json]` (the worker's owner only; exit
  codes 2 / 3 / 75 / 77 / 255) reports progress and the result
  ("cpc-1: 0.1.8 → 0.1.9; 2 jobs and 1 session kept running"). On Windows
  MSI installs the new Longarm Update service installs the newer signed MSI
  without a UAC prompt (the user-session and admin workers restart, jobs
  and sessions keep running); other installs update longarmd itself.
  Workers too old for it get a clear message (`machine-update` feature).
- Automatic updates (off by default): `longarm worker update <worker> --auto
  on`, the tray's Settings, or `longarm-update auto on` - the machine's owner
  or an administrator only. The service then installs the hub's latest
  stable release every 6 hours once no job has started for 5 minutes.
- Versions everywhere: `longarm ls` has a VERSION column (`0.1.8↑` and a
  "0.1.9 available" line for workers that report a newer release they
  can't install themselves: `NodeInfo.update_available`); the tray's
  Workers tab shows each machine's worker and admin-worker versions and,
  for the owner, an "Update to X" button with progress and the result; the
  panel's My machines page says `longarm worker update <name>` next to an
  outdated version. The tray's update banner installs through the service
  when the machine has it (no UAC prompt).
- The hub serves a version's release record
  (`GET /releases/<version>/release.json`, next to `/releases/latest.json`)
  and `CheckUpdate` takes `want` (an owner-requested version).
- `longarm version --json` features `worker-update`, `ls-version`.
- Clearer Windows installer roles: the Default page says what each choice
  installs - "This is my everyday computer: send jobs to workers" (client)
  or "This machine runs jobs (Dev Box, build machine)" (client + your
  user-session worker) - and preselects "runs jobs" on Cloud PCs / Dev
  Boxes (`CPC-` names, the Dev Box agent service). The admin worker comes
  only with a worker role (`ROLES=client ADMIN_WORKER=1` is refused). The
  Finish page lists what was installed.
- The tray's Account tab changes this computer's roles after the install
  (send jobs / run jobs / admin commands): one administrator prompt
  (`longarmd install-helper roles`, recorded where the MSI's upgrade, repair
  and uninstall find it), then one approval signs in what was added.
- The tray starts the sign-in by itself after an install (and on the first
  start of a new version) and keeps its popup open while it runs: the code
  large, copied to the clipboard, a notification with it; the result names
  exactly what the machine became ("Signed in as X: client device ...,
  worker ... (user session), admin worker ..."), as `longarm login` /
  `longarmd login` now print. The popup stays open until closed by default
  (auto-hide is the pin button's choice, remembered). An expired sign-in
  shows "Your sign-in expired on <date>: Sign in again", with a
  notification three days before when renewal fails.
- The tray popup behaves as an app: no WebView2 context menu (its own Copy /
  Paste for text), no browser keys, zoom, status bar or file drops; devtools
  only in development builds (release builds use the `production` tag);
  links open in the system browser.
- `install.ps1 --login` starts the tray right away (in your session, not
  elevated) instead of at the next logon.
- One machine, one entry: a user-session worker and its admin worker (runs
  as SYSTEM, `<worker>-admin`) show together - one tray card with an
  "admin commands: on" badge, the per-capability state ("worker online ·
  admin offline 2h") and the labels as chips; `longarm ls` folds the admin
  worker into its worker's row (ADMIN column); the panel lists it right
  under its worker. attach / ssh go to the session worker; "run as admin"
  (the card's ⋯ menu) names the admin worker. The hub records the pair at
  sign-in (`NodeInfo.companion`, `NodeInfo.machine` - additive JSON fields);
  older registrations pair by `<worker>-admin`, owner and host name.
- Tray: workers offline for over an hour move to a collapsed "Offline (n)"
  section, one line each with "last seen", the error behind "details";
  online machines first. "Remove" (with a confirmation) revokes your own
  worker on the hub (`longarm worker remove`; admins: any worker), through
  the daemon's new `POST /v1/workers/{name}/remove` (LocalClient.RemoveWorker).
- Tray Settings: notifications for your workers / your jobs can be turned
  off; the client's version, "Open logs folder", the telemetry switch
  (`longarm telemetry off|on`) and "Collect diagnostics" (`longarm diag`,
  nothing uploaded). "This worker": "Start worker" (the session task or the
  service) and "Open log folder" when it isn't running; a machine installed
  for a hub points at the Account tab instead of the join-code form (still
  there under "Use a join code").

- Update notices for installs that can't update themselves: workers (MSI
  installs, user-session workers, the admin worker) ask the hub anyway and
  report `update_available` (version, why not automatic) in `longarmd
  status`, the hub's node list and the panel; `longarm` prints a notice on a
  terminal once a day (`LONGARM_NO_UPDATE_NOTICE=1` turns it off); the tray
  shows "Update to X", downloads the MSI, checks the hub's sha256 and the
  release signature, and runs it. The hub serves `GET /releases/latest.json`
  and fills `WhoamiResponse.latest_version` / `latest_path`.
- `longarm admin release push --allow-none`: when no worker of the channel
  can self-update (MSI installs, user-session workers) it lists them and
  exits 79 instead of failing; release.yml's rollout job turns that into a
  warning (they update through the tray's MSI banner), while a halted or
  rolled-back rollout still fails the job.
- Panel: "Sign in with Microsoft" is the login page's first button (the
  password + TOTP form folds under "Panel account"; no injected link any
  more), users get "My machines" (their workers and devices, one row per
  machine: labels, sharing, removal) and "Get Longarm" (the newest MSI, the
  one-liners, three steps), outdated versions are marked on Nodes, and a
  kill asks first (job, worker, owner).
- `longarm handoff --to <worker|local> [--from ...] (--session ID | --latest)`
  ([docs/usage.md](docs/usage.md#hand-off-an-agent-session)): move an
  in-progress Claude Code or Copilot CLI conversation local -> worker,
  worker -> worker or worker -> local together with its code - a snapshot
  commit of HEAD + uncommitted + untracked files made with a temporary index
  (the user's branch and index untouched), a new worktree on the target on
  branch `handoff/<id>` with the changes uncommitted again (workers: remote
  first with `--git-auth forward`; worker -> worker: only the delta relayed
  through the client; local: `git worktree add` next to the checkout) - and
  resume it there (`claude --resume` / `copilot --resume=` in a longarm
  session, then attach; locally in the terminal). The source longarm session
  is stopped once idle; a local agent that still has the session open is
  refused (exit 75). The transcript and its state are copied with the paths
  rewritten structurally (known path fields, plus exact source-path prefixes
  at path boundaries), the session id kept, the source copy kept and marked
  handed off. Preflight: the agent on the target (missing or older: exit 2),
  logged in, MCP servers the target lacks (warnings), `--max-size` (64M).
  Only through the worker-direct channels: the hub never sees a transcript.
  Running processes (tool calls, background shells, MCP servers) are not
  carried over. Copilot CLI support is experimental.
  Session file names from the source are untrusted: validated as plain
  relative paths inside the session's own files (no `..`, absolute, drive,
  backslash, control characters; Windows device names, `:` and trailing
  dots on Windows targets), no symlink / junction on the destination path
  (`Files.Stat` `no_follow`, worker feature `file-lstat`), exclusive
  creation, file count and per-file size bounded.
  How the code travels: `--via relay|remote|auto` (default auto: relay
  below `--via-threshold`, 20M of objects the target lacks, else remote;
  `[handoff]` in workers.toml). Relay worker -> worker goes directly
  between the workers (Handoff service, worker feature `handoff-direct`: a
  one-time, single-peer, token-bound, 10-minute offer registered by the
  owner's client, the target dials the source over the port-link legs under
  the port-link owner and trust rules; falls back to the client relay).
  Remote pushes the snapshot to `longarm/handoff/<user>/<id>-<time>` of the
  https remote (a worker source with the forwarded credential,
  `Workspace.PushHandoff`, worker feature `handoff-push`: write access for
  that push only, hooks off, only that namespace), the target fetches it,
  and the branch is deleted afterwards (`--keep-branch`; `handoff
  --cleanup`). A built-in secret check of changed and untracked files, files
  over 50 MB and refused pushes fall back to relay (`--allow-secrets`).
- Handoff only between your own machines: another user's worker (shared
  with you or not, admin certificates included) is refused as source and
  target (exit 77), by the client and by the worker's handoff methods.
  Worker -> worker transfers go through the hub's relays - embedded and
  external - too: the hub lists the pair in the relay policy for the
  transfer (`Fleet.HandoffPair`, the caller's own workers only, at most 10
  minutes, released afterwards).
- `longarm agent-sessions [<worker>|local] [--repo-path DIR] [--json]`: the
  resumable agent sessions of a machine (agent, id, project directory, last
  modified, title or first prompt, size, handed-off state).
- Sessions get `LONGARM_WORKER` and `LONGARM_SESSION` in their environment
  (worker feature `session-env`); `handoff` uses them for its default
  `--from`. Capabilities `agent-handoff`, `agent-sessions`.
- Windows installer for password-less machines (Entra-only sign-in with
  Windows Hello or a security key, e.g. Microsoft Dev Box): the MSI's first
  page is **Default (recommended)** - the hub built into the release, a
  sign-in with Microsoft afterwards, only "Use this computer" / "Also run
  jobs on this computer" to pick - or **Custom** (hub, join code or "Sign in
  with Microsoft", CA fingerprint, the worker as a service under Advanced).
  Silent: `MODE=default|custom`, `RUN=session|service`, `ADMIN_WORKER=1`,
  `SESSION_SID`, `LAUNCH_TRAY=0` (all remembered where it matters; the
  comment at the top of `packaging/windows/longarm.wxs`).
- User-session worker (`RUN=session`, the default; [docs/usage.md](docs/usage.md#the-user-session-worker-runsession-the-default)):
  longarmd runs as the signed-in user, started at logon by a per-user
  scheduled task (`LongarmWorker-<SID>`, interactive token: no password) whose
  host (`longarm-tray --worker-host`, windowless, or `longarmd session-mode
  run`) restarts it when it exits; one host per user; data dir `~\.longarm`.
  Keeps running while a remote desktop is disconnected, stops at logoff /
  reboot. `longarmd session-mode install|uninstall|start|stop|status|run`;
  self-update mode `supervised` (exit 75, the host restarts; the watchdog
  leaves the restart to it). Service installs keep their service on upgrade.
- The tray signs in: "Sign in with Microsoft" (`/trayapi/signin/*`,
  `signin.SignIn` with the device, the user-session worker and the admin
  worker as targets - one code, one approval), with the code, progress,
  cancel and errors; then "Signed in as ...", the worker's state, Sign out,
  Open panel. The MSI starts the tray at the end of the install in the
  installing user's session (not elevated, through a one-off task:
  `longarm-tray --signin`). `--serve` is unchanged.
- Built-in default hub: `version.DefaultHub` / `version.DefaultHubCA`
  (ldflags from the repository variables `LONGARM_DEFAULT_HUB` /
  `LONGARM_DEFAULT_HUB_CA`; `build-msi.ps1 -DefaultHub -DefaultHubCA`);
  `longarm login`, `longarmd login` and the tray fall back to it after the
  installer-recorded hub (`signin.FallbackHub`); `longarm version --json`
  shows `default_hub`.
- Admin worker, Windows side: the MSI checkbox / `ADMIN_WORKER=1` installs
  the LocalSystem service `longarmd-admin` (own data dir and listener) that
  hands its CSR to the tray's sign-in through an ACL'd hand-off dir, checks
  the returned certificate (its key, the hub's CA, `longarm.io/admin=true`)
  and pins `[access]` to its owner (`admins = "none"`). "Disable admin
  worker" in the tray / `longarmd admin-worker disable|status`.
- `install.ps1`: `-Run session|service` (default session: `longarmd
  session-mode install`, no password); with the MSI, `-Login` installs with
  `MODE=custom LOGIN=1 RUN=session` and signs in in the console; the binary
  install also fetches `longarm-tray.exe` (the logon task's host).
- CI: a `msi` job builds the MSI with WiX 5 (pinned) with and without a
  built-in hub, ICE-validates it, extracts it with an administrative install
  and checks its tables (never installs it); part of the required `tests`.

- Admin worker (Windows; [docs/usage.md](docs/usage.md#admin-worker-windows),
  [docs/security.md](docs/security.md#admin-worker-windows)): a pairing may
  carry an admin worker's CSR (`StartLinkRequest.admin_worker_csr_pem` /
  `admin_worker_name`, or kind `admin` alone; `JoinResponse.admin_worker`):
  one approval enrolls it as the approver's worker `<worker>-admin`, marked
  by the hub (`longarm.io/admin=true`, `NodeInfo.admin_worker`). The hub
  enforces: owner-only (never shared; admin certificates and `--owner`
  refused), never placed automatically (`sched.Pick`), no hub-relayed
  commands, and `longarm.io/...` labels are reserved (`worker label`,
  `admin label`, join codes, sign-in labels and worker hellos can't set or
  remove them). The `/link` page warns that it runs commands as SYSTEM.
  `hubclient`: `JoinOptions.CSR` (a pre-made CSR, no key),
  `LinkOptions.AdminWorker`, `JoinResult.AdminWorker`, `ErrNoAdminLink`
  (an older hub: nothing installed); `signin`: `Options.AdminWorker`,
  `CSRSource`, `Code.AdminWorker`, `Result.AdminWorker`. `longarm ls` /
  `worker ls` / the panel mark admin workers.
- Sessions for programs ([docs/usage.md](docs/usage.md#sessions-for-programs)):
  `longarm session start [<worker>|auto] -s NAME [--repo ... --ref ... --lfs ... --git-auth forward] [--json] -- cmd`
  creates a session without attaching (idempotent by name: same spec =
  `"created": false`, another = exit 2; placed by the hub like `run`
  without a worker; `--repo` gives it a worktree `worktrees/s.<name>.<generation>` that
  lives as long as the session's data). `longarm session send` types input
  (`--text`, `--stdin-file`, `--key esc|enter|ctrl-c|...`, `--raw`) as one
  contiguous terminal write that never interleaves with an attached user's
  keystrokes; `--if-idle-ms N` writes only if nobody typed for N ms (else
  exit 75). Input is never stored (audit: its size). `longarm session stop
  --grace 10s`: Ctrl-C to the program, then kill. `longarm sessions` without
  a worker lists all your workers; `--json` has `source`, `worker`, every
  field, the worktree, and terminal activity (`lastOutput`, `lastInput`,
  `lastInputSource` attach|send, `inputSeq`, `lastActivity`); `-s NAME`
  (exit 3 when unknown). `run` / `exec --session NAME` run in the session's
  directory. `longarm tail <worker> <path> [-f] [--json]` streams a growing
  file with `get`'s access. New `version --json` features `session-start`,
  `session-send`, `session-send-if-idle`, `session-stop`,
  `sessions-json-v2`, `tail`, `run-session`; workers announce
  `session-start`, `session-send`, `session-send-if-idle`, `session-stop`,
  `session-info`, `file-tail` (API: `Sessions.Start/Get/Send/Stop`,
  `Files.Tail`, new `Session` fields).
- `longarm run --repo <https url> --git-auth forward`: the worker fetches
  the repository from its host with a short-lived credential of yours,
  forwarded with the job: Azure DevOps - an Entra token for Azure DevOps
  (`499b84ac-1321-427f-aa17-267ca6975798`) from `az account
  get-access-token` in the organization's tenant (from the
  `X-VSS-ResourceTenant` header of the organization URL, not az's default
  tenant), else `git credential fill`; GitHub - `gh auth token` (long-lived),
  else `git credential fill`; other hosts - `git credential fill`. Like the
  `--stdin-file` payload it goes only in the worker-direct submit (never
  the hub), stays in longarmd's memory, reaches git and git-lfs through a
  credential helper (longarmd, over loopback with a one-time key) only for
  the exact protocol, host and repository path of the fetched URL - never
  in their environment or config; redirects refused, the LFS endpoint
  pinned against `.lfsconfig`, no submodules, traces off - is zeroed
  afterwards and masked in logs and errors; the job never sees it; job records and the
  audit log say `git_auth: true` only. `--ref <full commit id>` fetches a
  commit that is no branch tip by id (falling back to the remote's
  branches); a commit the mirror has is not fetched again, but on a
  shared-isolation worker the job proves its own access to the remote first
  (`git ls-remote`, no objects), so another user's fetch never lets a job
  skip the remote's authorization. `--lfs fetch`:
  the worker also fetches the commit's Git LFS objects (git-lfs, same
  credential). No credential here: exit 77; an older worker: exit 2
  (workers announce `git-auth-forward` in `Node.Info` features)
  ([docs/usage.md](docs/usage.md#fetching-with-your-credential---git-auth-forward),
  [docs/security.md](docs/security.md#forwarded-git-credentials)).
- `--git-auth forward` with a local checkout (`longarm run --repo <path>`,
  `longarm sync`): remote first - the worker fetches the commits of the
  checkout's remote (the branch's upstream, else origin; GitHub / Azure
  DevOps ssh remotes as https) that the commit builds on
  (`Workspace.Fetch`), and only the unpushed delta is shipped. Falls back to
  shipping everything, with a note, when that is not possible (no https
  remote, no credential, an older worker, the remote unreachable from the
  worker or refusing the credential).
- `longarm version --json`: feature `git-auth-forward`.

- `longarm run --repo <local checkout> --lfs skip|ship`: Git LFS files of a
  shipped checkout no longer fail the worktree (the smudge filter had no LFS
  server: "batch request: missing protocol"). Default `skip`: LFS files are
  pointer files on the worker (noted by longarm and in the job's log);
  `ship` also sends the commit's LFS objects the worker's mirror lacks, and
  the worktree gets their content (`git lfs checkout`); LFS attributes in
  nested `.gitattributes` count too. The worker refuses sparse and other
  non-regular tar entries and bounds each upload (default 4 GiB per object,
  16 GiB / 50000 objects in total; `[workspace] lfs_max_*` in
  longarmd.toml). `longarm sync --lfs` alike ([docs/usage.md](docs/usage.md#port-forwarding-files-code-sync)).
- `--ref <full commit id>` for local checkouts (`longarm run --repo`,
  `longarm sync`): the commit is shipped (also one on no branch) and checked
  out detached. Short ids are refused with the full id in the message.
- `longarm worker label <worker> KEY=VALUE KEY-`: owners set and delete
  their own workers' labels (admins: any worker), checked and audited by the
  hub (`Fleet.SetWorkerLabels`, `workers/label`).
- `longarmd login --label KEY=VALUE` (repeatable): the hub records the
  worker's labels at the sign-in (az and the `/link` pairing, where the
  approver sees them), as `longarm worker add --label` does for codes;
  install.sh `--login --label k=v`, install.ps1 `-Label 'k=v,k2=v2'` /
  `LONGARM_LABELS`, the MSI's `LABELS` (with `LOGIN=1`; recorded in
  `HKLM\Software\Longarm` like `HUB`, used by the tray and `longarmd login`).

- Port links (`longarm link`): a user's processes on one worker reach a
  loopback port of another worker as `127.0.0.1:<port>`
  ([docs/usage.md](docs/usage.md#port-links-worker-to-worker)).
  Both workers apply their own policy to the link's owner - `[access]`
  users, the owner and shares of a user-owned worker, disabled users, and
  with end-to-end trust the owner's pinned key, the owner's signed consent
  (`longarm link create` signs the link on an endorsed device) and an
  endorsement of the other worker by that key - so the hub can narrow,
  never widen, what a worker allows, and can't create a link there
  ([docs/security.md](docs/security.md#port-links)). Link
  connections are in the chained audit log; telemetry counts them without
  names, ports or addresses.
- `[forward] targets` in longarmd.toml: what port forwards may reach.
- `[links] max_conns` in longarmd.toml: open connections per port link
  (default 64).
- Domain-only sign-in: `longarm login longarm.example.com` and
  `longarmd login longarm.example.com` need no port and no
  `--ca-fingerprint` - the hub's CA is fetched from
  `https://<host>/ca.crt` over the system's HTTPS roots and pinned (the
  gRPC address from the hub's new `/.well-known/longarm-hub`); hubs
  without a public HTTPS panel ask for `--ca-fingerprint`
  ([docs/security.md](docs/security.md#first-contact-domain-only-sign-in)).
- One approval for both roles: `longarmd login --role both` (and the
  install scripts' `--login --role both`) makes a machine the user's client
  device and own worker with one code; the `/link` page lists both.
- `signin.SignIn`: the sign-in flow as a Go API with progress / code
  callbacks and cancellation, for the tray; `longarmd.EnsureWorkerService`
  / `InstallService` for its service step.
- MSI `LOGIN=1`: no join code; the hub (and fingerprint) are recorded and
  the service waits for the sign-in the tray or `longarmd login` does.
- `longarm run --stdin-file <file|->`: up to 1 MiB for the job's stdin that
  is never stored - not in `job.json`, the hub's job index, the audit log or
  any log (memory only: longarm -> worker over mTLS -> shim -> the job's
  pipe; records carry its size, `stdin_bytes`; each copy is zeroed once
  handed on). A job whose payload was lost before the hand-off (longarmd
  restarted, the shim didn't start) never runs: it fails with "stdin payload
  lost before start; resubmit", and resubmitting the id with the payload
  replaces it. Needs a worker announcing the new `job-stdin` feature
  (`Node.Info` `features`)
  ([docs/usage.md](docs/usage.md#job-stdin-payload---stdin-file)).
- `longarm jobs --json` says who answered: `"source": "hub" | "workers"`
  (and `"unreachable"` workers with the fallback); `--hub-only` never falls
  back to the workers (exit 69 without an answering hub); `--id <job>` lists
  one job and exits 3 when it is not known
  ([docs/usage.md](docs/usage.md#jobs---json)).
- `longarm version [--json]`: `{"version": ..., "features": [...]}` for
  programs that check what this longarm supports (`exit-codes`,
  `jobs-source`, `jobs-hub-only`, `stdin-file`, `lfs`, `ref-sha`,
  `worker-label`) ([docs/usage.md](docs/usage.md#capabilities-longarm-version---json)).

### Fixed

- **Upgrading the MSI with Longarm running showed "The setup was unable to
  automatically close all requested applications"** (and could need a
  reboot): Restart Manager can't close the tray's WebView2 window, the
  windowless worker host and `longarmd run`. Restart Manager is now off
  for the MSI (`MSIRESTARTMANAGERCONTROL=Disable`) and an upgrade helper in
  the MSI stops Longarm's own processes instead: the installing user's tray
  before the in-use check, then (elevated, before the old version goes) the
  worker services (`longarmd-admin`, a `RUN=service` `longarmd`) and every
  user's tray, worker host and `longarmd run` - asked to stop through a
  per-process stop event (`Global\LongarmStop-<pid>`), ended by process id
  only when they have none (older versions) or after 20 s, and only
  processes whose image is in the install dir. Running jobs and sessions
  survive: binaries their shims (and `longarm` clients) still hold are
  moved to `<install dir>\old` instead of being replaced in place. After
  the install, what ran starts again: services, the users' worker tasks,
  the trays in their users' sessions (unelevated); a failed upgrade puts
  the old binaries back and restarts them. The tray quits on Windows'
  end-of-session / Restart Manager messages and registers for restart
  (docs/usage.md "Upgrading with Longarm running").

- **Release files of a hub on Azure Container Instances with
  `--storage-auth identity` were lost on every restart**: its data dir is
  the container's own disk, so `/releases/<v>/<file>` answered 404 while
  `/install.sh` / `/install.ps1` still named the version, and workers
  couldn't download updates. The template now creates a blob container
  `releases` (Storage Blob Data Contributor for the hub's identity) and
  sets `LONGARM_HUB_RELEASES_BLOB_URL`; hub.toml `[releases] blob_url`
  (an Azure Blob Storage container or `file:///<dir>`) keeps published
  files there as `<version>/<file>` (+ `.minisig`), with the data dir as
  a cache refilled from the container and checked against the recorded
  sha256 (the HTTP route and worker self-update). Cache writes go through
  a temp file of their own, checked (size, sha256) before the rename, and
  publishes of a version are serialized; downloads from the container run
  two at a time with a 10 minute limit, refuse a blob of the wrong size
  before reading it (and never read more than the recorded size), and a
  failed one backs off (1 minute, doubling to 30) until the file is
  published again. At start the hub
  lists the container, uploads files only its data dir has, and logs
  every file found nowhere (`RELEASE FILES MISSING`); install scripts
  then hand out the newest complete stable version (or "no release
  yet"), `release push` refuses an incomplete version, and `longarm admin
  release ls` marks lost files `(missing)` (`ListReleases` leaves
  incomplete versions out unless `include_incomplete`). Publishing a
  known version again with exactly its files (no `--replace`, allowed to
  the release role; `--skip-identical` doesn't skip an incomplete version)
  stores them again without changing the version. A known version with
  other files is still refused, now after the upload ("exists on the hub
  with other files"). To heal a hub deployed before this: redeploy
  (`deploy.sh` re-run), then re-run the publish job of its latest release
  or publish a new version ([deploy/azure/README.md](deploy/azure/README.md#without-storage-account-keys)).
- Workspace fetches with `--git-auth forward` wrote 300+ lines of curl
  trace (the remote's response headers, `WWW-Authenticate`, `X-VSS-*`,
  FedAuth redirect URLs) into the job's log: longarmd set
  `GIT_CURL_VERBOSE=0` to turn tracing off, and git turns it on when the
  variable is set at all. Now every git / git-lfs run of the worker gets
  an environment without `GIT_TRACE*`, `GIT_CURL_VERBOSE`,
  `GIT_TRANSFER_TRACE` (removed ignoring case; trace2 `=0`,
  `GIT_TRACE_REDACT=1`), forwarded output drops lines with
  Authorization / Cookie headers and masks bearer / basic tokens.
  `[workspace] git_trace = true` re-enables inherited traces for debugging.
- `longarm logs <id>` (and `wait`, `kill`, ... without a worker) right
  after a run could say `job ... not found on any known worker` (exit 3)
  for a job the hub knows: when an earlier longarm had found the hub slow
  and remembered it as down, longarm asked only the workers it knew without
  the hub (none with an empty node cache) and gave up after 8 s per worker.
  Now the hub is asked again before giving up, workers get 15 s like `jobs`,
  and workers that did not answer make it exit 255 (outcome unknown)
  instead of "not found".
- `longarm run --id <id> --repo <local checkout>` again after "stdin payload
  lost before start; resubmit" was refused with `AlreadyExists` (exit 2):
  every run uploads the checkout anew under a new staging id, which was part
  of the compared spec. The job's identity is now what the workspace
  resolves to (repository, URL, ref, commit, a digest of the uploaded
  uncommitted and untracked files, `--lfs fetch`), not the upload's id or
  whether a credential was forwarded: an identical retry replaces the job
  that never started or returns the existing one; other uncommitted or
  untracked content is another job (`AlreadyExists`), never the old result.
- The job's `GH_TOKEN` secret no longer passes through git's environment
  (`http.extraheader`) either: the same credential helper, scoped to the
  exact URL, redirects refused.
- `longarm attach` running inside a ConPTY on Windows (a desktop app's
  portable-pty / xterm.js, a nested session) attached to a Windows worker
  lost all typed input: the worker's pseudo console asks its terminal for
  win32-input-mode (`ESC[?9001h`), and the outer ConPTY then swallowed
  plain keystrokes. longarm no longer passes that request on.

### Changed

- `longarm ls` prints a compact table by default: WORKER STATE OS CPU% MEM
  GB JOBS LABELS (ADMIN, OWNER); offline workers say when they were last
  seen ("offline 3d"). `-o wide` is the previous table (sessions, disk,
  uptime, top processes, PATH). `longarm worker ls` lists an admin worker
  right after its worker and says "offline 3d" too.
- Tray: the title, tabs and source line stay put; only the list scrolls.
  The drain button shows for admins only (it needs an admin certificate).
  Kills ask first (job, owner, worker) - in My jobs and on This worker.
  Notifications cover your own workers and your own jobs only, not other
  people's machines. The icon and tooltip count machines and leave out
  workers offline for over an hour.

- `longarm run --repo <url>`: a fetch the remote refuses for lack of
  credentials (401/403, no prompt possible - e.g. Azure DevOps through Git
  Credential Manager on the worker) fails with an error naming the cause
  and the ways out (`--repo .` to ship from the client, or a non-interactive
  credential on the worker); worker-side git never waits for Git Credential
  Manager's sign-in (`GCM_INTERACTIVE=never`). URL credentials (user and
  password, or a token as the user) are masked in workspace git errors,
  the job's log and its failure reason.
- `longarmd join` / `longarmd login` (and longarmd's start) warn when the
  data dir is under a `.claude` directory: Claude Code denies writes there,
  so its jobs could not edit their worktrees.

- `longarm login` / `longarmd login` no longer trust a hub CA on first use:
  without `--ca-fingerprint` they bootstrap it over HTTPS or fail
  (`longarm join` / `longarmd join` are unchanged).
- longarm's exit codes are a documented contract
  ([docs/usage.md](docs/usage.md#exit-codes)): 2 usage error / request that
  can't succeed as given (was 255), 3 not found, 69 hub unavailable
  (`--hub-only`), 75 refused for capacity, quota or rate - e.g. `run
  --no-queue` when no worker passes the gate (was 255), 77 not allowed; 255
  stays for transport and other errors. Job exit codes are mirrored as
  before.
- `longarm jobs --json` prints `"jobs": []` instead of leaving the key out
  when there are none.
- The worker answers a failed import / checkout of an uploaded `--repo`
  (`Workspace.Sync`) and a `put` that can't resume a partial file with
  `Aborted` instead of `FailedPrecondition`: they can pass, so longarm
  exits 255 (retry), not 2.
- `--ref` / `--lfs` / `--repo` validation errors of `run` and `sync` exit 2.
- `longarm run --id <id>` again for a job the hub already placed and the
  worker reports returns the job (exit 0) instead of failing with
  "already runs on"; another spec under the id still fails (exit 2).

### Breaking changes

- **Port forwards reach the worker's own loopback ports only by default.**
  `longarm forward`, ssh `-L`/`-D` through the built-in sshd (proxied and
  direct listeners) and port links now refuse other hosts, for every caller
  including admins. Before, a worker in shared isolation forwarded to any
  host:port it could reach (its LAN, other machines). See
  [Upgrading](#upgrading).

## Upgrading

### To the release with port links

- **Forwards to other hosts.** If you forward through a worker to
  machines on its network (e.g. `longarm forward w1 5432:db.lan:5432`), add
  this to the worker's `<data>/longarmd.toml` and restart longarmd:

  ```toml
  [forward]
  targets = "any"
  ```

  Without it such forwards fail with `PermissionDenied: this worker forwards
  to its own loopback ports only`, and longarmd logs a warning naming
  `[forward] targets = "any"` the first time it refuses one. Keep the
  default where users do not fully trust each other
  ([docs/security.md](docs/security.md)). With per-user isolation non-admins
  still only reach loopback ports of their own processes either way.
- **Hub database.** longarm-hub migrates its database to schema 4 (a
  `port_links` table) at start; an older longarm-hub refuses the migrated
  database. Back it up before upgrading if you may need to roll back.
- **Mixed versions.** Port links need both workers on this release (older
  workers ignore the hub's link lists and don't serve `LinkTunnel.Dial`).
