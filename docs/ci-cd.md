# CI/CD

What runs where, what a maintainer sets up once for an own deployment, how a
release is cut and rolled back. Everything runs on GitHub-hosted runners.

| Workflow | Trigger | Jobs |
|---|---|---|
| [`ci.yml`](../.github/workflows/ci.yml) | pull request, push to `main`, manual | **lint** (ubuntu): `scripts/privacy-check.sh`, gitleaks, gofmt, `go vet`, cross-compile (vet + build + e2e test binary) for windows/linux/darwin × amd64/arm64, shellcheck, actionlint · **test** (ubuntu-latest, windows-latest, macos-latest), parallel jobs per OS: *unit* (`go build`, `go test` of every package but `./e2e`, the per-user isolation tests) and *e2e-1..3* (*e2e-1..2* on macOS: `./e2e`'s top-level tests dealt out by `.github/scripts/test-shard.sh`, balanced by the durations in `.github/test-durations.txt`, refreshed with `.github/scripts/test-durations.sh <run ids>`); go test runs through `.github/gotest`, which puts failed tests (and the tests a `-timeout` panic caught running, with its goroutine dump in the log) into the job summary and annotations and uploads each job's test durations (artifact `test-timings-*`) · **msi** (windows-latest): the MSIs built with WiX 5, ICE-validated, extracted, their tables checked (`.github/scripts/msi-check.ps1`; `msi-sequence-check.ps1`: RemoveExistingProducts where Windows Installer allows it, never after a deferred action - error 2613, which the 0.1.13 MSI is shown to fail) · **msi-install** (windows-latest, the runner's administrator; `.github/scripts/msi-install-test.ps1`): the MSIs really installed - a fresh install (Default, `ROLES=both`, `LOGIN=1` with a dummy hub), repair, a refused service switch, an upgrade to the next build and the uninstall; upgrades from the released 0.1.6 and the latest release (session worker, admin worker, LocalSystem service) with the tray and worker running: they must stop and start again, the client daemon keep running, the new files be in place, the verbose log be free of errors 2613 / 1722 / 1603 and failed custom actions · **tests**: passes when every test job did (msi and msi-install included) - the required check of `main`'s ruleset, so `gh pr merge --auto --squash` merges once CI is green · **azure** (ubuntu): `bicep build` of deploy/azure/main.bicep must equal the committed `azuredeploy.json`, `bicep lint` without warnings, PSScriptAnalyzer on deploy.ps1 · **release-cache** (push to `main` only, ubuntu + windows): the Go build cache release.yml restores, see "Caches" below |
| [`ci-requeue.yml`](../.github/workflows/ci-requeue.yml) | a ci.yml run failed | re-runs ci.yml jobs GitHub never started (no hosted runner acquired - macOS capacity), and the `tests` check with them, when no job ran and failed; at most 3 attempts per run (`.github/scripts/ci-requeue.sh`). Jobs that failed for real are never re-run, so flaky tests stay visible |
| [`image.yml`](../.github/workflows/image.yml) | pull request, push to `main`, called by release.yml | **smoke** (ubuntu): build the hub image (deploy/hub/Dockerfile), `deploy/azure/test/container-smoke.sh` with Azurite (bootstrap, admin join, restore after the container is replaced, health) · **publish** (push to main, releases): `ghcr.io/<owner>/longarm-hub` for linux/amd64+arm64 with SBOM and provenance: `:edge` + `:sha-<commit>` from main, `:X.Y.Z` (+ `:latest` for stable releases) from a tag |
| [`release.yml`](../.github/workflows/release.yml) | tag `v*` | **meta** (tag check) → **build-unix** (ubuntu) and **build-windows** (windows-latest, MSIs) in parallel → **publish** (merges and checks them, signs) → **hub** → **rollout**; **image** (image.yml) runs next to the builds, **image-latest** after publish; hub and rollout only run when a deployment is configured. A manual run is a dry run (builds, gather, image smoke test; nothing signed, published or pushed) |
| [`deploy.yml`](../.github/workflows/deploy.yml) | manual | hub deploy / rollback / status, fleet push of a version already on the hub |

Nothing is assumed on the runners beyond what GitHub's images ship: Go comes
from `actions/setup-go` (version from `go.mod`), .NET 8 from
`actions/setup-dotnet`, GoReleaser v2.19.0 as the official prebuilt binary
pinned by SHA256, WiX 5.0.2 by `dotnet tool install`, shellcheck and gitleaks
as pinned, sha256-checked downloads, actionlint by `go run …@v1.7.12`. All
third-party actions are pinned by commit and kept current by Dependabot. The
opt-in tests (`LONGARM_SERVICE_TEST`, `LONGARM_MSI_TEST`, …) are forced off in
CI.

Pull requests from forks run `ci.yml` with the default read-only token and
no secrets. No workflow uses `pull_request_target`.

The privacy check (`scripts/privacy-check.sh`) checks the tree against a
**private deny-list** (real hostnames, addresses, account names a maintainer
wants kept out of the public tree): the repository secret `PRIVACY_PATTERNS`,
one extended regex per line (`w:<word>` for whole words). Locally the script
reads the gitignored file `.privacy-patterns`. Pushes fail without the
deny-list; fork pull requests, which don't get secrets, run only the generic
checks. The script never prints the patterns, only the matching lines.

Caches: **CI jobs** use the actions cache (setup-go) for Go modules and the
build cache. **Release jobs** (`release.yml`, `deploy.yml`) never *save* a
cache, and restore exactly one kind: the Go build cache that ci.yml's
**release-cache** job saves on pushes to `main` (keys
`go-build-main-<os>-<arch>-<go version>-<go.sum hash>-<commit>`,
[`.github/actions/go-build-cache`](../.github/actions/go-build-cache/action.yml),
`actions/cache/restore`). Why that is safe, and the only cache they take:

- GitHub scopes caches by ref. A run can restore caches of its own ref and of
  the default branch, nothing else: a pull request's caches (saved under
  `refs/pull/N/merge`) are invisible to tag runs, whatever key they use.
  Only runs on `main` - merged code - can write the cache a tag run restores;
  the release-cache job only runs there (push, or a manual run on `main`).
- setup-go's cache is not used: it would save a cache from the release job,
  and it restores the module cache, which Go doesn't re-verify. Go modules
  are downloaded fresh in every release job and checked against `go.sum` and
  the checksum database, as before; only compiler output is reused.
- The Go build cache is content-addressed: an entry's key is the hash of
  everything that went into it (sources, flags, Go version, GOOS/GOARCH,
  dependencies' keys), so a stale entry is never used, only missed. The
  job compiles what the release compiles - `.github/scripts/release-go-cache.sh`:
  the five commands of `.goreleaser.yaml` (+ the tray with `-tags production`)
  for the build jobs' targets with `CGO_ENABLED=0 -trimpath`, plus
  `packaging/hubrelease` - and saves only the entries it used (about 750 MB
  for Linux and 400 MB for Windows, compressed, per push to `main`; GitHub
  evicts the least recently used caches beyond the repository's 10 GB, and
  release jobs restore the newest). The build jobs print the hit rate (`release-go-cache.sh stats`) in their summary.
- The release's outputs don't change: the binaries are reproducible
  (`-trimpath`, no cgo), whether a package came from the cache or not.

Everything else starts fresh: GoReleaser and WiX are installed per job
(pinned, checksum-verified, no cache), and the `longarm` that handles the
signing key and the release certificate is built from the tag's source in
the job itself (never taken from the build artifact).

## 1. Releases without a deployment

Out of the box (a fork, or this repository without any variables) a `v*` tag
builds every package and publishes a **GitHub Release**: binaries for six
targets, MSIs, deb/rpm packages, the hub release directory and `SHA256SUMS`.

- Without the variable `LONGARM_UPDATE_PUBKEY` the binaries are built with
  self-update turned off (a warning says so).
- Without the secret `LONGARM_RELEASE_KEY` the files are not signed (a
  warning says so); self-updating workers only install signed releases.
- Without the variable `LONGARM_HUB` nothing is uploaded to a hub and the
  `hub` / `rollout` jobs are skipped.

## 2. One-time setup for an own deployment

### 2.1 Release key and update public key

Self-update trusts one **minisign key**: `longarmd` has the public key built in,
the hub has it as `[releases] public_key`. Create it on a maintainer's machine
(never on the hub):

```sh
longarm-hub release keygen            # ~/.config/longarm/release/longarm-release.{key,pub}
                                      # LONGARM_RELEASE_KEY_PASSWORD=... encrypts the key
```

The hub verifies uploads against the public key: put its last line into
hub.toml (`[releases] public_key = "RW…"`), or, for a hub on Azure Container
Instances (deploy/azure, whose hub.toml is generated), deploy it with
`./deploy.sh --name <hub> --release-public-key "$(tail -1 longarm-release.pub)"`
(`deploy.ps1 -ReleasePublicKey`; a re-run keeps it). Without it the hub stores
unsigned or wrongly signed files, and its install.sh / install.ps1 hand them
to new users.

### 2.2 The CI's hub credential: a `release` role certificate

The role `release` may call exactly: `HubAdmin.PublishRelease`,
`ListReleases`, `PushRelease`, `GetRollout`, `AbortRollout` - that is
`longarm admin release publish | ls | push | status | abort` - and even there
not `push --allow-downgrade` or `publish --replace` (admin only: a stolen CI
certificate can't move the fleet back to an older, maybe vulnerable, signed
version, or swap published files). Everything else is denied (default-deny
tables): no node or job administration, no join codes, no audit, no tailnet,
nothing on any worker (longarmd has no entry for the role), not usable at the
relay, and join codes can't mint it. The hub logs its calls as
`release/<name>` in the audit log, and `longarm admin revoke <name>` locks it
out.

Issue it where the CA key is (valid 180 days by default; use a dated name,
because a revoked name stays revoked):

```sh
# on the hub (the CA is in the hub's data dir):
sudo -u longarm-hub longarm-hub --config /etc/longarm-hub/hub.toml issue --role release --name ci-2026h2 --out /tmp/ci-tls
# or on the admin machine that holds the CA:
longarm admin issue --role release --name ci-2026h2 --out ./ci-tls [--ca-dir <CA dir>] [--days 180]
# a hub on Azure Container Instances (deploy/azure; exec takes no arguments):
# the secret's value directly, name ci-<date>, 180 days
MSYS_NO_PATHCONV=1 az container exec -g <rg> -n <hub> --container-name hub \
  --exec-command /usr/local/bin/longarm-hub-release-cert > ci-tls.b64   # (prints a private key)
```

(`longarm-hub issue --role release --b64` prints the same, the base64 of the
tar.gz below, instead of writing files.)

Turn the bundle into the secret (then delete the files):

```sh
tar -C ci-tls -czf - ca.crt node.crt node.key | base64 -w0 > ci-tls.b64     # Linux
gh secret set LONGARM_RELEASE_TLS --repo your-org/longarm < ci-tls.b64
shred -u ci-tls.b64 ci-tls/*; rmdir ci-tls
```

Rotation: issue `ci-2027h1`, replace the secret, then `longarm admin revoke ci-2026h2`.

### 2.3 Hub deploy user (SSH forced command, no shell)

On the hub (Linux, longarm-hub under systemd as in
[deploy/hub/README.md](../deploy/hub/README.md): binary
`/usr/local/bin/longarm-hub`, config `/etc/longarm-hub/hub.toml`, unit
`longarm-hub`):

```sh
# the script: root-owned, not writable by deploy
install -o root -g root -m 0755 deploy/hub/longarm-hub-deploy /usr/local/bin/longarm-hub-deploy

# the user: a login shell is required (sshd runs the forced command through it),
# no password, no sudo beyond the rule below
useradd --create-home --shell /bin/sh deploy
passwd -l deploy

# the CI's key: generate it anywhere, the private half only goes into the secret
ssh-keygen -t ed25519 -N '' -C longarm-ci -f hub-deploy
# root-owned, so the deploy user can't change its own restrictions; "restrict"
# also turns off ~/.ssh/rc and any future forwarding kind
install -d -o root -g root -m 0755 ~deploy/.ssh
echo "restrict,command=\"/usr/local/bin/longarm-hub-deploy\",no-pty,no-port-forwarding,no-agent-forwarding,no-X11-forwarding $(cat hub-deploy.pub)" \
  > ~deploy/.ssh/authorized_keys
chown root:root ~deploy/.ssh/authorized_keys; chmod 0644 ~deploy/.ssh/authorized_keys

# the one sudoers rule (longarm-hub-deploy re-runs itself as root with --root <cmd> [<version>]
# and validates the arguments again as root)
echo 'deploy ALL=(root) NOPASSWD: /usr/local/bin/longarm-hub-deploy --root *' > /etc/sudoers.d/longarm-hub-deploy
chmod 0440 /etc/sudoers.d/longarm-hub-deploy
visudo -cf /etc/sudoers.d/longarm-hub-deploy
```

- Keep sudo's defaults (`env_reset`, no `SETENV`, no `env_keep` for
  `LONGARM_HUB_DEPLOY_*`): the script's test hooks must not reach the root
  side (it also drops them itself when `SUDO_USER` is set).
- If `sshd_config` has `AllowUsers`/`AllowGroups`, add `deploy`. Optional
  hardening on top of the `authorized_keys` options:
  `Match User deploy` → `ForceCommand /usr/local/bin/longarm-hub-deploy`,
  `AuthenticationMethods publickey`, `PermitTTY no`, `AllowTcpForwarding no`,
  `X11Forwarding no`.
- hub.toml must have `[releases] public_key = "RW…"` (the key of 2.1): the
  script refuses to install anything without it. (A hub on Azure Container
  Instances has no deploy user - a `deploy.sh --image` re-run upgrades it -
  and gets the key with `deploy.sh --release-public-key "$(tail -1
  longarm-release.pub)"`, see 2.1.)
- Needs `curl` (health check, HTTP fallback).

What the forced command accepts (`SSH_ORIGINAL_COMMAND`; only `0-9 A-Z a-z . -`
and spaces, nothing is evaluated):

| command | effect |
|---|---|
| `<version>`, `deploy <version>` | fetch `longarm-hub-linux-<arch>` + `.minisig` of that version from the hub's release store (`<data_dir>/releases/<v>/` on disk, regular files only, so it works while the hub is down; else `http://<http>/releases/…`), verify the signature with the **installed** longarm-hub (`longarm-hub release verify`, or minisign) against `[releases] public_key` (bound to file name + version), check that the new binary reports `<version>`; then stop longarm-hub, snapshot `hub.db` (+ `-wal`/`-shm`) to `/var/lib/longarm-hub-deploy/db-before-<version>/`, copy the current binary to `longarm-hub.prev`, install (atomic rename), start, wait up to 60 s for `/healthz` to report `"ok":true` and `<version>`. Otherwise: stop, put the database snapshot and the previous binary back, start, exit 1 (data written by the failed version in those seconds is dropped with it). Re-running for the installed, healthy version does nothing. |
| `check <version>` | dry run: fetch + signature + version probe, changes nothing |
| `rollback` | swap `longarm-hub` and `longarm-hub.prev` (the database stays), restart, health check. If the older binary doesn't come up - typically because the newer one migrated the database schema - it swaps back and exits 1; going further back then means restoring the snapshot by hand (stop longarm-hub, copy `db-before-<v>/hub.db*` back, keep ownership `longarm-hub`) |
| `status` | installed / previous version, `/healthz` |

Exit codes: 0 ok, 1 failed and rolled back (or nothing changed), 2 refused
(bad command, signature, missing file, no public key), 3 failed and the
rollback failed too (look at the hub). Everything is logged to journald
(`journalctl -t longarm-hub-deploy`) and to stderr (the CI log). A lock
(`/run/longarm-hub-deploy.lock`) serializes runs; a dropped SSH session doesn't
interrupt a running deploy.

Pin the hub's host key for the CI (compare with
`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` on the hub):

```sh
ssh-keyscan -t ed25519 hub.example.com > hub-known-hosts
ssh-keygen -lf hub-known-hosts
```

Try it from your machine: `ssh -i hub-deploy deploy@hub.example.com status`.

### 2.4 Secrets and variables

| secret | content |
|---|---|
| `LONGARM_RELEASE_KEY` | the minisign private key file (`longarm-release.key`), verbatim |
| `LONGARM_RELEASE_KEY_PASSWORD` | optional, if the key is encrypted |
| `LONGARM_RELEASE_TLS` | base64 of the tar.gz with `ca.crt node.crt node.key` of the release certificate (2.2) |
| `HUB_DEPLOY_SSH_KEY` | the private key `hub-deploy` (2.3) |
| `HUB_SSH_KNOWN_HOSTS` | the `hub-known-hosts` line(s) |

| variable (not secret) | content |
|---|---|
| `LONGARM_UPDATE_PUBKEY` | the public key line (`RW…`, last line of `longarm-release.pub`), built into longarmd |
| `LONGARM_HUB` | the hub's gRPC address, e.g. `hub.example.com:7443`; enables the upload and the `rollout` job |
| `HUB_SSH` | `deploy@hub.example.com`; enables the `hub` job |
| `LONGARM_DEFAULT_HUB` | the hub built into the release (a domain, e.g. `longarm.example.com`): `longarm login` / `longarmd login` / the tray without a hub, and the MSI's "Default" choice ([usage](usage.md#built-in-default-hub)); unset: no default (the release warns; the MSI offers Custom only) |
| `LONGARM_DEFAULT_HUB_CA` | optional: that hub's CA fingerprint (`longarm-hub ca show`), pinned instead of trusting `https://<hub>/ca.crt` |

```sh
R=your-org/longarm
gh secret set LONGARM_RELEASE_KEY   --repo $R < ~/.config/longarm/release/longarm-release.key
gh secret set LONGARM_RELEASE_TLS   --repo $R < ci-tls.b64
gh secret set HUB_DEPLOY_SSH_KEY    --repo $R < hub-deploy
gh secret set HUB_SSH_KNOWN_HOSTS   --repo $R < hub-known-hosts
gh variable set LONGARM_HUB           --repo $R --body hub.example.com:7443
gh variable set HUB_SSH               --repo $R --body deploy@hub.example.com
gh variable set LONGARM_UPDATE_PUBKEY --repo $R --body "$(tail -1 ~/.config/longarm/release/longarm-release.pub)"
gh variable set LONGARM_DEFAULT_HUB   --repo $R --body longarm.example.com   # moving the repository (e.g. to an internal GitHub): set it there
```

What limits the blast radius of a leaked secret:

- the hub credential is the `release` role (2.2): it can only publish
  releases and start/inspect/abort rollouts - no exec, no node or job admin,
  no downgrades, no replacing published files;
- the hub deploy key is a forced command (2.3): it can only install a
  longarm-hub that the hub itself verifies against `[releases] public_key`,
  with health check and rollback;
- the hub refuses uploads that the release key didn't sign (`[releases]
  public_key`; an ACI hub: `deploy.sh --release-public-key`, 2.1);
- workers only install releases signed by the release key, canary first,
  with automatic rollback.

### 2.5 Approval gate

Without further setup, **pushing a `v*` tag is the release decision**:
`release.yml` runs build → publish → hub → rollout without stopping, and
anyone with write access can push a tag (or change a workflow to read the
secrets). Keep write access to people you trust, and protect tags with a
ruleset (only maintainers may create `v*` tags).

For a manual approval, use a GitHub **environment** with required reviewers
(available for public repositories on every plan, and for private
repositories on paid plans):

1. Settings → Environments → New environment `release`; add required
   reviewers and restrict it to `v*` tags and `main`.
2. Move the secrets of 2.4 into the environment
   (`gh secret set NAME --env release --repo $R …`).
3. Add `environment: release` to the `publish`, `hub` and `rollout` jobs of
   `release.yml` and the `hub` and `fleet` jobs of `deploy.yml`.

The run then waits for an approval before anything is signed or deployed;
`meta`, the builds and the image (no secrets) run right away; the
versioned image `:X.Y.Z` is pushed before the approval (only `:latest`
waits for publish) - to hold it back too, add `publish` to the `needs` of
release.yml's `image` job (it then starts after the approval).

### 2.6 Ordering: hub first, then the fleet

`publish → hub → rollout`. The control plane goes first (the usual skew rule:
control plane ≥ nodes):

- the new hub serves old and new longarmd alike; a new longarmd never has to
  talk to an older hub (the hub-side counterpart of a new feature exists
  before any worker uses it);
- the rollout state machine itself then runs in the new hub;
- the hub restart (workers reconnect) can't fall inside a canary's health
  window, which requires a steady hub session for 60 s;
- the hub deploy downloads from the release store, so publish must come
  before it anyway; a failed hub deploy rolls back by itself and stops the
  workflow before any worker is touched.

Compatibility in both directions holds as long as the gRPC API only grows
(new fields/methods; old longarmd ignore them, a new hub treats missing
fields as defaults) - keep it that way.

## 3. Cutting a release

```sh
git checkout main && git pull
git tag -a v1.4.0 -m "Longarm 1.4.0"
git push origin v1.4.0
```

1. **meta** (no secrets): validates the tag (`vMAJOR.MINOR.PATCH[-pre]`;
   stable tags must be on `main`). Then, in parallel and without secrets:
   **build-unix** (ubuntu-latest): GoReleaser `release --skip=publish,docker`
   with `LONGARM_BUILD_SIDE=unix` - linux and darwin binaries, archives,
   deb/rpm - and `packaging/hubrelease`; **build-windows** (windows-latest):
   `packaging/build.ps1 -Side windows` - the Windows binaries and archives,
   the two MSIs (WiX 5) - and `packaging/hubrelease`. Both build with the
   same flags and `LONGARM_UPDATE_PUBKEY` / `LONGARM_DEFAULT_HUB` built in
   (`.goreleaser.yaml` has one build entry per side and binary): the
   artifacts `release-unix` and `release-windows` (unsigned `hub-release/`
   files with canonical names, the longarm-hub debs, each side's
   `checksums.txt`), kept 14 days (re-run publish within that time).
   **image** (next to the builds; it builds from the tag's source, not from
   their output): image.yml builds and smoke-tests the hub image, then pushes
   `ghcr.io/<owner>/longarm-hub:<v>` with the job's `GITHUB_TOKEN` (packages:
   write); **image-latest** moves `:latest` to it once publish succeeded
   (stable tags). The first push creates a private package: make it public
   once so [deploy/azure](../deploy/azure/README.md) can pull it anonymously.
2. **publish**: merges the two artifacts into one release directory with one
   `checksums.txt`; `.github/scripts/release-files.sh` stops the release
   unless the file names are exactly those of the releases so far and
   `checksums.txt` matches the published copies (a dry run does this in its
   **gather** job). Builds `longarm` from the tag, signs every file
   (`<file>.minisig`, trusted comment = file name + version) and - with
   `LONGARM_HUB` - uploads them with the release certificate; the hub
   verifies each signature against its public key. Then the GitHub Release
   (notes generated from the commits; all files, signatures and `SHA256SUMS`).
3. **hub**: `check <v>`, then `deploy <v>` over SSH.
4. **rollout**: builds `longarm` from the tag;
   `longarm admin release push <v> --canary 1 --wait 15m -f`: one canary
   (online, least busy) updates; once it ran `<v>` and stayed connected for
   15 min, the other workers follow; the job fails when the rollout halts (a
   worker failed its health check and rolled itself back) or is aborted.
   Workers that can't self-update (no key in their build, package-managed,
   MSI installs under Program Files, user-session workers) are skipped and
   named. With no updatable worker at all there is nothing to roll out: the
   job runs the push with `--allow-none`, which lists those workers and
   exits 79; the job turns that into a **warning** and passes. Those workers
   get the version as a notice and update through the tray's "Update to
   <v>" banner (MSI, checked against the hub's sha256 and the release
   signature) or the install script; `longarmd status` and the panel's
   Nodes page show them as outdated. A real rollout failure - a canary
   that halts, a worker that rolls back, an abort - still fails the job.

**Prereleases** (`v1.5.0-rc1`, any `-` suffix): channel `beta`, GitHub
prerelease, and no hub deploy or fleet rollout. Beta workers
(`[update] channel = "beta"` in longarmd.toml) get it with
`longarm admin release push 1.5.0-rc1 --canary 1 --wait 15m` by hand (or
deploy.yml).

Use **Re-run failed jobs** after a failure: publish skips identical files (and
fails if the hub has *different* files under that version) - unless the hub
lost some of them (a hub without `[releases] blob_url` whose data dir was
replaced): then it uploads them again and the hub restores the version as it
was (`longarm admin release ls` marks lost files `(missing)`; re-run the
release's publish job, **Re-run all jobs** would rebuild different MSIs), the hub deploy
does nothing for the installed healthy version, rollout follows a running
rollout of the same version or accepts a finished one. Don't use "Re-run all
jobs" once publish succeeded: it rebuilds, and the new MSIs differ (WiX gives
every build a new package code), so publish refuses them. Don't re-tag a
version either; tag the next patch version.

### Stopping a release

Cancel the run in Actions (before publish nothing reached the hub; after
publish the files stay on the hub but nothing is rolled out), or
`longarm admin release abort` during the rollout (updated workers keep the
new version; roll back with deploy.yml or
`longarm admin release push <old> --allow-downgrade`, admin only).

## 4. Rolling back

| What | How |
|---|---|
| hub, automatic | `deploy` restores `longarm-hub.prev` when `/healthz` doesn't report the new version within 60 s, and the job fails (the rollout never starts) |
| hub, later | Actions → **deploy** → hub `rollback` (swaps `longarm-hub` and `longarm-hub.prev`), or hub `deploy` + an older version from the store. On the hub itself: `sudo longarm-hub-deploy rollback` / `status` |
| a worker, automatic | the watchdog rolls longarmd back when the new version fails its health check; the version goes on that worker's skip list and the rollout halts (the job fails) |
| the fleet | **Roll forward**: revert on `main` and release the next patch version (the normal path, canary first). Moving workers to an *older* version is admin-only - the CI's release certificate can't downgrade: with an admin certificate, `longarm admin release abort`, then `longarm admin release push <old> --allow-downgrade --canary 1 --wait 15m -f`. (Actions → **deploy** → fleet pushes a version that is not older than the workers'.) |
| the GitHub Release | edit / delete it in the UI (`gh release delete v1.4.0`); the tag stays |

`longarm admin release ls | status` (admin or release certificate) shows what
the hub has and where a rollout stands.

## 5. Local equivalents

```sh
scripts/privacy-check.sh                              # the privacy gate
go test ./...                                         # what the test jobs run
go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12        # + shellcheck on PATH
shellcheck deploy/hub/longarm-hub-deploy .github/scripts/*.sh scripts/*.sh packaging/build.sh packaging/linux/*.sh
go test -run 'TestHubDeployScript|TestHubReleaseVerify|TestReleaseRole|TestCIScripts' -v ./e2e/
pwsh packaging/build.ps1 -SkipPublish -Snapshot       # the release build jobs (both sides; -Side windows|unix for one)
gh workflow run release.yml --ref <branch>             # the release as a dry run (CI; nothing published)
```
