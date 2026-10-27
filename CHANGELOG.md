# Changelog

## Unreleased

### Added

- `longarm run --repo <local checkout> --lfs skip|ship`: Git LFS files of a
  shipped checkout no longer fail the worktree (the smudge filter had no LFS
  server: "batch request: missing protocol"). Default `skip`: LFS files are
  pointer files on the worker (noted by longarm and in the job's log);
  `ship` also sends the commit's LFS objects the worker's mirror lacks, and
  the worktree gets their content (`git lfs checkout`). `longarm sync
  --lfs` alike ([docs/usage.md](docs/usage.md#port-forwarding-files-code-sync)).
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

### Changed

- `longarm run --repo <url>`: a fetch the remote refuses for lack of
  credentials (401/403, no prompt possible - e.g. Azure DevOps through Git
  Credential Manager on the worker) fails with an error naming the cause
  and the ways out (`--repo .` to ship from the client, or a non-interactive
  credential on the worker); worker-side git never waits for Git Credential
  Manager's sign-in (`GCM_INTERACTIVE=never`).
- `longarmd join` / `longarmd login` (and longarmd's start) warn when the
  data dir is under a `.claude` directory: Claude Code denies writes there,
  so its jobs could not edit their worktrees.

- `longarm login` / `longarmd login` no longer trust a hub CA on first use:
  without `--ca-fingerprint` they bootstrap it over HTTPS or fail
  (`longarm join` / `longarmd join` are unchanged).

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
