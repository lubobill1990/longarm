# Changelog

## Unreleased

### Added

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
