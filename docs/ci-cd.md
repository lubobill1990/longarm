# CI/CD

What runs where, what a maintainer sets up once for an own deployment, how a
release is cut and rolled back. Everything runs on GitHub-hosted runners.

| Workflow | Trigger | Jobs |
|---|---|---|
| [`ci.yml`](../.github/workflows/ci.yml) | pull request, push to `main`, manual | **lint** (ubuntu): `scripts/privacy-check.sh`, gitleaks, gofmt, `go vet`, cross-compile (vet + build + e2e test binary) for windows/linux/darwin × amd64/arm64, shellcheck, actionlint · **test** (ubuntu-latest, windows-latest, macos-latest), parallel jobs per OS: *unit* (`go build`, `go test` of every package but `./e2e`, the per-user isolation tests) and *e2e-1..3* (*e2e-1..2* on macOS: `./e2e`'s top-level tests dealt out by `.github/scripts/test-shard.sh`, balanced by the durations in `.github/test-durations.txt`, refreshed with `.github/scripts/test-durations.sh <run ids>`); go test runs through `.github/gotest`, which puts failed tests (and the tests a `-timeout` panic caught running, with its goroutine dump in the log) into the job summary and annotations and uploads each job's test durations (artifact `test-timings-*`) · **msi** (windows-latest): the MSIs built with WiX 5, ICE-validated, extracted, their tables checked (`.github/scripts/msi-check.ps1`; `msi-sequence-check.ps1`: RemoveExistingProducts where Windows Installer allows it, never after a deferred action - error 2613, which the 0.1.13 MSI is shown to fail) · **msi-install** (windows-latest, the runner's administrator; `.github/scripts/msi-install-test.ps1`): the MSIs really installed - a fresh install (Default, `ROLES=both`, `LOGIN=1` with a dummy hub), repair, a refused service switch, an upgrade to the next build and the uninstall; upgrades from the released 0.1.6 and the latest release (session worker, admin worker, LocalSystem service) with the tray and worker running: they must stop and start again, a running job (`longarmd conhost` and its command) keep running, the new files be in place, the verbose log be free of errors 2613 / 1722 / 1603 and failed custom actions · **tests**: passes when every test job did (msi and msi-install included) - the required check of `main`'s ruleset, so `gh pr merge --auto --squash` merges once CI is green · **azure** (ubuntu): `bicep build` of deploy/azure/main.bicep must equal the committed `azuredeploy.json`, `bicep lint` without warnings, PSScriptAnalyzer on deploy.ps1 · **release-cache** (push to `main` only, ubuntu + windows): the Go build cache release.yml restores, see "Caches" below |
| [`ci-requeue.yml`](../.github/workflows/ci-requeue.yml) | a ci.yml run failed | re-runs ci.yml jobs GitHub never started (no hosted runner acquired - macOS capacity), and the `tests` check with them, when no job ran and failed; at most 3 attempts per run (`.github/scripts/ci-requeue.sh`). Jobs that failed for real are never re-run, so flaky tests stay visible |
| [`image.yml`](../.github/workflows/image.yml) | pull request, push to `main`, called by release.yml | **smoke** (ubuntu): build the hub image (deploy/hub/Dockerfile), `deploy/azure/test/container-smoke.sh` with Azurite (bootstrap, admin join, restore after the container is replaced, health) · **publish** (push to main, releases): `ghcr.io/<owner>/longarm-hub` for linux/amd64+arm64 with SBOM and provenance: `:edge` + `:sha-<commit>` from main, `:X.Y.Z` (+ `:latest` for stable releases) from a tag |
| [`release.yml`](../.github/workflows/release.yml) | tag `v*`; push to `main` (a **dev** build, see "Release channels"; not for docs-only changes) | **meta** (tag check; the dev version) → **build-unix** (ubuntu) and **build-windows** (windows-latest, MSIs) in parallel, **build-macos** (macos-latest, after build-unix: Developer ID signing, the `.pkg` installers, notarization; 2.8) → **publish** (merges and checks them, signs) → **hub** → **rollout**; **image** (image.yml) runs next to the builds, **image-latest** after publish; hub and rollout only run when a deployment is configured (and never for dev builds or release candidates). A manual run is a dry run (builds, gather, image smoke test; nothing signed, published or pushed) |
| [`deploy.yml`](../.github/workflows/deploy.yml) | manual | hub deploy / rollback / status, fleet push of a version already on the hub |
| [`mirror.yml`](../.github/workflows/mirror.yml) | pull request or push to `main` changing `deploy/azure/mirror-images.txt` or the workflow, manual | **verify** (ubuntu, read-only, `contents: read`): each third-party image the Azure template runs (the sidecars) comes from an allowed source repository and its pinned digest resolves there (crane). Nothing is published: `deploy/azure/deploy.sh --acr NAME` imports the pinned digests into the hub's Azure Container Registry itself, before anything is deleted (deploy/azure/README.md "Third-party images"); the former GHCR package `longarm-mirror` is retired |
| [`mirror-gim.yml`](../.github/workflows/mirror-gim.yml) | push to any branch, branch delete, same-repository pull request, every 15 min, manual | mirrors branches, `main` and pull requests into the internal copy gim-home/longarm (`.github/scripts/mirror-gim.sh`), see "6. Internal mirror (gim-home)" |

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
- Without the `APPLE_*` secrets (2.8) the macOS binaries, archives and
  `.pkg` installers are not signed or notarized (a warning says so);
  MDM-managed Macs refuse unsigned packages.
- Without the variable `LONGARM_HUB` nothing is uploaded to a hub and the
  `hub` / `rollout` jobs are skipped.
- Without the Trusted Signing variables (2.7) the Windows binaries and MSIs
  are not Authenticode-signed (a notice says so; SmartScreen warns on the
  download). Minisign signatures and checksums don't depend on it.

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
| `APPLE_DEVELOPER_ID_APP_P12` | base64 of the Developer ID Application certificate + private key (`.p12`) (2.8) |
| `APPLE_DEVELOPER_ID_APP_P12_PASSWORD` | its export password |
| `APPLE_DEVELOPER_ID_INSTALLER_P12` | base64 of the Developer ID Installer certificate + private key (`.p12`) (2.8) |
| `APPLE_DEVELOPER_ID_INSTALLER_P12_PASSWORD` | its export password |
| `APPLE_NOTARY_API_KEY_P8` | the App Store Connect API key file (`AuthKey_<KEYID>.p8`), verbatim (2.8) |
| `APPLE_NOTARY_KEY_ID` | its key ID |
| `APPLE_NOTARY_ISSUER_ID` | the issuer ID shown above the keys |
| `LONGARM_GPG_PRIVATE_KEY` | the GPG key (armored) that signs the rpm and the apt/yum repositories on GitHub Pages ([linux-repos.md](linux-repos.md)); unset: unsigned rpm, no repositories |
| `LONGARM_GPG_PASSPHRASE` | its passphrase (optional) |

| variable (not secret) | content |
|---|---|
| `LONGARM_UPDATE_PUBKEY` | the public key line (`RW…`, last line of `longarm-release.pub`), built into longarmd |
| `LONGARM_HUB` | the hub's gRPC address, e.g. `hub.example.com:7443`; enables the upload and the `rollout` job |
| `HUB_SSH` | `deploy@hub.example.com`; enables the `hub` job |
| `LONGARM_DEFAULT_HUB` | the hub built into the release (a domain, e.g. `longarm.example.com`): `longarm login` / `longarmd login` / the tray without a hub, and the MSI's "Default" choice ([usage](usage.md#built-in-default-hub)); unset: no default (the release warns; the MSI offers Custom only) |
| `LONGARM_DEFAULT_HUB_CA` | optional: that hub's CA fingerprint (`longarm-hub ca show`), pinned instead of trusting `https://<hub>/ca.crt` |
| `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID` | the Entra app that signs (GitHub OIDC, no secret; 2.7) |
| `TRUSTED_SIGNING_ENDPOINT`, `TRUSTED_SIGNING_ACCOUNT`, `TRUSTED_SIGNING_PROFILE` | the Trusted Signing account's endpoint (`https://<region>.codesigning.azure.net`), its name and the certificate profile; all six set: Authenticode signing on (2.7) |
| `TRUSTED_SIGNING_SUBJECT` | optional: the certificate's CN (the validated name); every signed file is checked against it |
| `APPLE_TEAM_ID` | the Apple Team ID (10 characters) of the Developer ID certificates; build-macos refuses certificates of another team (2.8) |
| `LONGARM_REPO_KEEP` | optional: how many stable releases the apt/yum repositories keep (default 3; [linux-repos.md](linux-repos.md)) |

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

### 2.7 Authenticode: Azure Trusted Signing (Windows)

The Windows exes (`longarm`, `longarmd`, `longarm-tray`, `longarm-hub`,
`longarm-relay`, the MSI's `longarm-update` and upgrade helper) and the MSIs
are Authenticode-signed with [Azure Trusted Signing](https://learn.microsoft.com/azure/artifact-signing/)
("Artifact Signing") when the six variables of 2.4 are set; without them the
release is built unsigned with a notice. Only the Windows build job is
involved:

- **Order.** GoReleaser's build hook (`.goreleaser.yaml`, `hooks: *winsign`)
  signs each exe right after it is built - before the zips and
  `checksums.txt`; `packaging/windows/build-msi.ps1` signs the binaries it
  builds, packs the signed exes into the MSI, then signs the MSI. The
  minisign signatures (publish) and `SHA256SUMS` are made afterwards, over
  the signed files. A check step verifies every published exe and MSI
  (signature, RFC 3161 timestamp from `http://timestamp.acs.microsoft.com`,
  and the publisher when `TRUSTED_SIGNING_SUBJECT` is set).
- **Tools.** signtool from the runner's Windows SDK with the dlib
  `Microsoft.ArtifactSigning.Client` from nuget.org, pinned by SHA256
  (`packaging/windows/sign.ps1`; every release build fetches and checks it,
  signed or not).
- **Credential.** No secret: `azure/login` with GitHub OIDC. The job runs in
  the environment `release-signing` only for a real release with
  `TRUSTED_SIGNING_PROFILE` set; the app's single federated credential trusts
  exactly `repo:<owner>/<repo>:environment:release-signing`, and the
  environment accepts deployments from `v*` tags only (no branch, no pull
  request, no dry run). The app's only role is *Artifact Signing Certificate
  Profile Signer* on the account. Its token lives in the build job only.
- **Updates are unaffected.** The LongarmUpdate service and the tray accept
  an MSI only with a valid minisign signature by the release key built into
  them (name and version in the trusted comment) and the hub record's
  sha256 - signed or unsigned Authenticode alike, so upgrades from and to
  unsigned builds keep working. Authenticode is for Windows (SmartScreen,
  the UAC prompt's publisher), not a second trust root: the update path does
  not check it, because a check that only applies "when a signature is
  present" adds nothing an attacker can't strip, and requiring it would
  break every installation updating from an unsigned release.

**Setup** (once; `deploy/azure/signing/trusted-signing.sh`, needs `az` on the
right subscription and `gh` with admin rights):

```sh
S=deploy/azure/signing/trusted-signing.sh R=your-org/longarm
$S account  my-rg my-signing eastus     # provider + account, Basic SKU
$S verifier my-rg my-signing            # you: "Artifact Signing Identity Verifier"
# identity validation in the portal (below), then:
$S ci       my-rg my-signing $R         # app + federated credential + Signer role + environment + 5 variables
$S profile  my-rg my-signing longarm-public <identity validation id> $R   # Public Trust profile; sets TRUSTED_SIGNING_PROFILE
```

**Identity validation** (portal only, by a person with the Identity Verifier
role; 1-20 business days for organizations): Azure portal → the Trusted
Signing account → *Identity validations* → *New identity* → *Public*, then
either

- *Organization*: a legal entity in the US, Canada, the EU, the UK,
  Australia, New Zealand, Japan, South Korea, Singapore, Switzerland, Norway
  or Israel, with its legal name, website, an email on its domain, business
  ID and address; or
- *Individual*: a developer located in the US or Canada. The form is filled
  from the subscription's **billing account, which must be of type
  Individual** with the legal name and sold-to address of the person's
  government ID; the person then completes a Microsoft Entra Verified ID
  check (AU10TIX, phone + ID + face check) and presents it.

Not possible: free, trial or sponsored subscriptions, and subscriptions billed
to an employer's internal or enterprise billing account for an individual
validation. The certificate's CN is the validated legal name; it can't be
customized.

**Cost.** Basic SKU: about USD 9.99 per month per account (5,000 signatures;
billed in full for the month, not pro-rated). A release signs about 16 files.

**Rotation.** Nothing to rotate in GitHub: there is no secret, and the
service renews the short-lived certificates itself (every signature is
timestamped, so it stays valid after the certificate expires). To replace
the CI identity: `$S ci ...` again (re-creates the federated credential;
delete the old app registration). To replace the profile (a new validation,
a new name): `$S profile ...` with the new values; the identity validation
must be renewed before it expires (reminders start 60 days ahead) or
signing stops. To turn signing off: delete the variable
`TRUSTED_SIGNING_PROFILE` (the release goes back to unsigned with a notice);
to stop paying: delete the account.

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

### 2.8 macOS signing and notarization (Apple Developer ID)

MDM-managed Macs install only packages signed with a **Developer ID
Installer** certificate, and Gatekeeper wants the code inside signed with a
**Developer ID Application** certificate (hardened runtime) and **notarized**
by Apple. release.yml's **build-macos** job (macos-latest, right after
build-unix, next to build-windows) does that with
[`.github/scripts/macos-sign.sh`](../.github/scripts/macos-sign.sh), on the
`release-unix` artifact, before publish computes `checksums.txt` /
`SHA256SUMS` and the minisign signatures:

1. imports the two `.p12` into temporary keychains (deleted at the end;
   Apple's Developer ID intermediate CAs come from `packaging/macos/certs`,
   pinned by SHA256) and checks that both certificates are `APPLE_TEAM_ID`'s;
2. signs the eight darwin binaries (`longarm`, `longarmd`, `longarm-hub`,
   `longarm-relay` × amd64/arm64) with `codesign --options runtime
   --timestamp --identifier com.longarm.<tool>` and verifies them (hardened
   runtime, timestamp, team);
3. repacks `longarm-darwin-<arch>.tar.gz` with the signed `longarm` and
   `longarmd` (after checking the archive held exactly the unsigned
   binaries) and puts the new sums into build-unix's `checksums.txt`;
4. builds `longarm-darwin-<arch>.pkg` ([packaging/macos](../packaging/macos/README.md):
   `/usr/local/bin`, a postinstall like the deb's), signed with
   `productbuild --sign … --timestamp`;
5. submits the binaries (one zip) and both pkgs to `xcrun notarytool` with
   the API key, in parallel (`--wait`; a rejection prints Apple's log and
   fails the job), staples the tickets to the pkgs and checks them with
   `spctl --assess --type install` (`source=Notarized Developer ID`);
6. replaces the `release-unix` artifact with the result (a re-run of the job
   finds it done and changes nothing).

Self-update keeps working: workers download exactly these signed
`longarmd-darwin-<arch>` files (checked against the release key) and swap
them in by rename, so the installed binary's code signature stays valid.

Without any `APPLE_*` secret (forks, a dry run) the files stay unsigned, the
pkgs are built unsigned, and the job warns; a partial setup warns about what
is missing. A secret that is set but doesn't work (wrong password, another
team, notarization rejected) fails the release.
[`macos-pkg.yml`](../.github/workflows/macos-pkg.yml) tests the script on
pull requests with self-signed stand-in certificates (signing, repacking, a
real `installer -pkg`, the no-secrets path, a wrong team); notarization
needs the real credentials.

**Getting the credentials** (once; the certificates last 5 years, the API
key until it is revoked):

1. **Apple Developer Program** (99 USD a year): enrol at
   <https://developer.apple.com/programs/enroll/> with an Apple Account that
   has two-factor authentication.
   - *Organization* (recommended for a company): needs a D-U-N-S number for
     the legal entity and the authority to sign for it; the certificates and
     macOS's prompts show the organization's name. Apple verifies it (days
     to weeks).
   - *Individual*: your own name appears in the certificates and prompts;
     usually approved within a day or two.

   Only the **Account Holder** can create Developer ID certificates (an
   organization's Account Holder can delegate it to Admins). The **Team ID**
   is under Membership details at <https://developer.apple.com/account>:
   set it as `APPLE_TEAM_ID` and give it to the MDM administrators
   ([usage](usage.md#macos-installer-pkg-and-mdm)).
2. **Two certificates: Developer ID Application and Developer ID
   Installer.** Each needs a certificate signing request (CSR). On a Mac:
   Keychain Access → Certificate Assistant → Request a Certificate From a
   Certificate Authority → your e-mail, a name, "Saved to disk" (twice). Or
   with openssl on any machine (the private keys stay there):

   ```sh
   umask 077
   openssl req -new -newkey rsa:2048 -nodes -keyout devid-app.key -out devid-app.csr -subj "/CN=Longarm Developer ID Application"
   openssl req -new -newkey rsa:2048 -nodes -keyout devid-installer.key -out devid-installer.csr -subj "/CN=Longarm Developer ID Installer"
   ```

   At <https://developer.apple.com/account/resources/certificates/add>
   choose **Developer ID Application** (profile type "G2 Sub-CA"), upload
   `devid-app.csr`, download the `.cer`; again for **Developer ID
   Installer** with `devid-installer.csr`. Then export each as a `.p12`
   with a strong password:
   - Keychain Access (CSR made there): double-click the `.cer`; under "My
     Certificates" right-click "Developer ID Application: <name> (<TEAMID>)"
     → Export → `.p12`, set a password. The same for the Installer one.
   - openssl (CSR made with openssl; `-legacy` writes the format macOS's
     `security import` reads - OpenSSL 3; with LibreSSL leave it out):

     ```sh
     openssl x509 -inform der -in developerID_application.cer -out devid-app.crt
     openssl pkcs12 -export -legacy -inkey devid-app.key -in devid-app.crt -out devid-app.p12              # asks for the password
     openssl x509 -inform der -in developerID_installer.cer -out devid-installer.crt
     openssl pkcs12 -export -legacy -inkey devid-installer.key -in devid-installer.crt -out devid-installer.p12
     ```

   Keep the `.p12` files and their passwords in a password manager; delete
   the `.key` files once the `.p12` are checked
   (`openssl pkcs12 -info -nokeys -in devid-app.p12`).
3. **An App Store Connect API key for notarization**: App Store Connect →
   Users and Access → Integrations → App Store Connect API → Team Keys
   (<https://appstoreconnect.apple.com/access/integrations/api>; the Account
   Holder requests API access once). Generate a key with the **Developer**
   role, e.g. "longarm-notary". Note the **Issuer ID** (above the list) and
   the **Key ID**, and download `AuthKey_<KEYID>.p8` - Apple offers the
   download only once.
4. **The secrets**, from the files, never as command-line arguments
   (`gh secret set` without `--body` reads the value from stdin, and at a
   terminal prompts without echoing):

   ```sh
   R=your-org/longarm
   base64 -i devid-app.p12 | gh secret set APPLE_DEVELOPER_ID_APP_P12 --repo $R         # Linux: base64 -w0 devid-app.p12 | ...
   gh secret set APPLE_DEVELOPER_ID_APP_P12_PASSWORD --repo $R                          # prompts, no echo
   base64 -i devid-installer.p12 | gh secret set APPLE_DEVELOPER_ID_INSTALLER_P12 --repo $R
   gh secret set APPLE_DEVELOPER_ID_INSTALLER_P12_PASSWORD --repo $R
   gh secret set APPLE_NOTARY_API_KEY_P8 --repo $R < AuthKey_ABCDE12345.p8
   gh secret set APPLE_NOTARY_KEY_ID --repo $R                                           # prompts: the Key ID
   gh secret set APPLE_NOTARY_ISSUER_ID --repo $R                                        # prompts: the Issuer ID
   gh variable set APPLE_TEAM_ID --repo $R --body ABCDE12345                             # not a secret
   ```

   With the approval gate (2.5), put them into the `release` environment
   (`--env release`) and add `environment: release` to `build-macos` as well.
5. **Check** with the next tag (a dry run never signs): build-macos's
   summary lists the signed files and their sums; on a Mac,
   `pkgutil --check-signature longarm-darwin-arm64.pkg` names the
   Developer ID Installer certificate and "Notarization: trusted by the
   Apple notary service", and `spctl --assess --type install -vv
   longarm-darwin-arm64.pkg` says `source=Notarized Developer ID`.

Renewal and leaks: a Developer ID certificate expires after 5 years; what
was signed (with a timestamp) and notarized before stays valid - replace the
`.p12` secrets before then. If the API key leaks, revoke it in App Store
Connect (it can only submit notarizations). If a `.p12` leaks, ask Apple
Developer Support to revoke the certificate - which also invalidates what it
signed - and release again with a new one.

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
   **build-macos** (macos-latest, after build-unix, 2.8; the only job with
   the `APPLE_*` secrets): signs the darwin binaries, repacks the darwin
   archives, builds and signs `longarm-darwin-<arch>.pkg`, notarizes and
   staples, and replaces `release-unix` with the result.
   **image** (next to the builds; it builds from the tag's source, not from
   their output): image.yml builds and smoke-tests the hub image, then pushes
   `ghcr.io/<owner>/longarm-hub:<v>` with the job's `GITHUB_TOKEN` (packages:
   write); **image-latest** moves `:latest` to it once publish succeeded
   (stable tags). The first push creates a private package: make it public
   once so [deploy/azure](../deploy/azure/README.md) can pull it anonymously.
2. **publish**: merges the two artifacts (release-unix as build-macos left
   it) into one release directory with one
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

**Prereleases** (`v1.5.0-rc.1`, any `-` suffix): channel `preview`, GitHub
prerelease, and no hub deploy or fleet rollout - see "Release channels"
below. Self-updating preview workers (`[update] channel = "preview"`) get it
with `longarm admin release push 1.5.0-rc.1 --canary 1 --wait 15m` by hand
(or deploy.yml); MSI machines on preview or dev pick it up themselves.

### Release channels

Every release goes to a **channel**; every machine follows one and takes
the newest release of its channel *or of a more tested one*
(`internal/update/channel.go`):

| channel | made by | version | GitHub Release | hub deploy | fleet rollout |
|---|---|---|---|---|---|
| `stable` | a tag `vX.Y.Z` on `main` | `X.Y.Z` | yes | yes | yes |
| `preview` | a tag `vX.Y.Z-<pre>` (e.g. `v0.1.22-rc.1`) | `X.Y.Z-rc.1` | prerelease | yes | no |
| `dev` | every push to `main` (merged pull requests; not docs-only) | `<next patch>-dev.<run>` | no | no | no |

- **Versions sort in release order.** A dev build's version is the next
  patch after the highest version tagged so far (stable or prerelease),
  with `-dev.<run number of release.yml>`: after `v0.1.21` main builds
  `0.1.22-dev.57`, `0.1.22-dev.58`, ...; `v0.1.22-rc.1` sorts above them,
  `v0.1.22` above that, and the next dev builds become `0.1.23-dev.N`.
  Nothing on a channel ever needs a downgrade. Tags with a `-dev` suffix
  are refused (dev versions come from main only).
- **Who gets what.** Stable machines (everybody, by default) only see stable
  releases: the tray's banner, the update service's automatic updates,
  `longarm worker update`, the hub's "update available" notices
  (`GET /releases/latest.json`, `CheckUpdate.latest_version`) and the
  install scripts all mean the newest stable. A machine on `preview`
  takes release candidates and stable releases, on `dev` everything.
- **Choosing a channel.** Windows (MSI installs): the tray's Client tab →
  Updates → Release channel (the machine's owner or a local administrator;
  stored by the update service in `HKLM\Software\Longarm\Update`, value
  `Channel`, and reported by the machine's longarmd to the hub). Elsewhere,
  or to pin one worker: `[update] channel = "preview"` in longarmd.toml.
  `beta`, the old name of `preview`, still works.
- **Going back** to a more tested channel takes effect with that
  channel's next release (no downgrades): a machine on `0.1.22-dev.9` that
  moves to stable stays there until `0.1.22` is released.
- **The hub follows preview.** There is one hub for every channel, so it
  runs a release candidate as soon as one is tagged (the `hub` job: the SSH
  forced command, health check, automatic rollback); a dev build must work
  with the hub as it is - the gRPC API only grows, so new clients and
  workers talk to an older hub, and what needs the hub's side lights up with
  the next release candidate. A hub on Azure Container Instances
  (`deploy/azure`) can't be deployed from CI (the tenant refuses GitHub's
  OIDC tokens): the run's summary (job `hub-manual`) gives the command,
  `deploy.ps1 -Name <hub> -ResourceGroup <rg> -Image
  ghcr.io/<owner>/longarm-hub:<version>`, to run from a checkout of the tag
  where `az` is signed in. To try something hub-side before an rc, deploy
  main's image `ghcr.io/<owner>/longarm-hub:sha-<12 hex>` (image.yml) the
  same way, briefly.
- **The hub** answers `GET /releases/latest.json?channel=preview|dev` with
  the newest complete release for that channel; an older hub ignores the
  parameter and answers its latest stable, so machines fall back to stable
  until the hub runs a version with channels.
- **MSIs** of one `X.Y.Z` (its dev builds, release candidates and the
  release) share the MSI version `X.Y.Z`; `MajorUpgrade
  AllowSameVersionUpgrades` makes each replace the one before. Which one is
  newer is decided before msiexec runs.
- **Dev builds** run the same workflow as a release (build, sign, upload
  with the release certificate) in their own concurrency group
  (`longarm-release-dev`: they queue among themselves, never behind or in
  front of a release; a burst of merges builds the first and the newest),
  and never touch the hub deploy, the fleet rollout, the GitHub Releases or
  the image tags (image.yml pushes `:edge` for main itself).

Typical flow: merge → dev build on the hub minutes later → the people on
`dev` run it → tag `vX.Y.Z-rc.1` when it looks good → the hub and preview
machines get it → tag `vX.Y.Z` → everybody.

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
go test -run 'TestHubDeployScript|TestHubReleaseVerify|TestReleaseRole|TestCIScripts|TestMirrorGimScript' -v ./e2e/
pwsh packaging/build.ps1 -SkipPublish -Snapshot       # the release build jobs (both sides; -Side windows|unix for one)
gh workflow run release.yml --ref <branch>             # the release as a dry run (CI; nothing published)
```

## 6. Internal mirror (gim-home)

[`mirror-gim.yml`](../.github/workflows/mirror-gim.yml) keeps the
Microsoft-internal copy https://github.com/gim-home/longarm in step with this
repository, without anyone's machine involved
([`.github/scripts/mirror-gim.sh`](../.github/scripts/mirror-gim.sh)). It runs
on every push to a branch, every branch delete, every pull request event of a
same-repository pull request, every 15 minutes and by hand
(`gh workflow run mirror-gim.yml`). Each run reconciles the whole state rather
than one event, in one concurrency group (`mirror-gim`, never cancelled), so a
missed, superseded or out-of-order run heals on the next.

What is mirrored:

| here | in gim-home |
|---|---|
| every branch but `main` | the same branch, force-pushed |
| `main` | merged into gim-home's `main` (commit "Sync main from <owner>/longarm"), a fast-forward there, never forced. gim-home's `main` has commits of its own (Start Right governance: `.github/acl/access.yml`, `.github/compliance/inventory.yml`, `.github/policies/jit.yml`, `.github/ISSUE_TEMPLATE/JitAccess.yml`); a merge conflict fails the run and is resolved there by hand |
| an open pull request #N | a pull request with the same head branch, base, title and body, plus the footer `Mirrored from <owner>/longarm#N` (this repository); title, body and base kept in sync; closed there when #N is closed or merged here (reopened with it). Nothing is merged in gim-home: the commits arrive through `main` |
| an open pull request from a fork | its head (`refs/pull/N/head`) as branch `pr-N` and a pull request from it - by the schedule, as fork events get no secrets; git objects only, the fork's code is never checked out or run here. Not mirrored when it changes `.github/` (there it would be a same-repository branch whose workflows run with gim-home's token): it gets a notice, review it here |
| a deleted branch | deleted there |
| tags | **not mirrored**: gim-home never releases (and has no `LONGARM_HUB`) |

gim-home's own things are never touched: pull requests without the footer
(e.g. its own Dependabot's) are never edited or closed, and their head
branches are never pushed to or deleted; a branch is only deleted there if
its tip is a commit of this repository (one the mirror pushed), never
`main` or `dependabot/*`. The workflow does not run in gim-home itself.

Note that gim-home runs its own copy of ci.yml on the mirrored pull
requests, as same-repository pull requests - including mirrored fork pull
requests' code - so gim-home should hold no secrets that ci.yml would hand to
pull request code.

### 6.1 The token: `GIM_MIRROR_TOKEN`

A fine-grained personal access token of the maintainer's **EMU account**
(the `…_microsoft` account that is a member of gim-home), limited to the one
repository. Without the secret the workflow prints a notice and passes; the
schedule catches up once it is set.

1. Sign in to github.com as the EMU account. Settings → Developer settings →
   Personal access tokens → **Fine-grained tokens** → Generate new token.
2. Token name `longarm mirror to gim-home`; expiration: the
   longest gim-home's policy allows (note the date: see rotation below).
3. **Resource owner: `gim-home`**. Repository access: **Only select
   repositories** → `gim-home/longarm`.
4. Repository permissions:
   - **Contents: Read and write** (push branches and `main`, delete branches)
   - **Pull requests: Read and write** (open, edit, close the mirrored pull requests)
   - **Workflows: Read and write** - fine-grained tokens have this
     permission (it only comes as write); without it GitHub rejects any push
     that adds or changes a file under `.github/workflows/`, i.e. most `main`
     syncs
   - Metadata: Read-only (added automatically)
5. Generate, copy the token (`github_pat_…`). If gim-home requires approval
   of fine-grained tokens, the token shows as *pending* until a gim-home
   owner approves it (Organization settings → Personal access tokens →
   Pending requests); until then pushes fail with 403.
6. Store it in **this** repository (never in gim-home):

   ```sh
   gh secret set GIM_MIRROR_TOKEN --repo <owner>/longarm # paste the token at the prompt
   gh workflow run mirror-gim.yml --repo <owner>/longarm # first run, then check its summary
   ```

The token's user needs push access to gim-home/longarm, and - if gim-home's
`main` has a ruleset requiring pull requests - must be allowed to bypass it,
as for the manual sync before. If gim-home's enterprise restricts tokens by
IP allow list or conditional access, GitHub-hosted runners can't use the
token at all (403 on every call): the workflow then needs a runner inside
the allowed network.

Rotation: create a new token as above (both are valid meanwhile), `gh secret
set GIM_MIRROR_TOKEN` with the new one, run the workflow by hand and check
it passes, then delete the old token (Settings → Developer settings →
Fine-grained tokens). A token that expired or was revoked fails the runs
with 401 - nothing is lost meanwhile: the next run after the new secret
mirrors everything.
