# Linux package repositories (apt, dnf)

Every stable release publishes signed apt and yum/dnf repositories with the
`longarm` package (client + worker; amd64/x86_64 and arm64/aarch64) and,
for apt, `longarm-hub`:

    https://lubobill1990.github.io/longarm/        (index, install snippets)
    https://lubobill1990.github.io/longarm/deb     (apt: dists/stable, pool)
    https://lubobill1990.github.io/longarm/rpm     (dnf/yum: repodata, packages)
    https://lubobill1990.github.io/longarm/longarm.asc   (the signing key, armored)
    https://lubobill1990.github.io/longarm/longarm.gpg   (the same key, binary, for apt)

The signing key is **"Longarm Release Signing"**, RSA 4096, fingerprint

    CA72 074C C1D3 D23F 0770  4BF9 C9B6 D4E8 3310 5307

also in the repository as [packaging/linux/repo/longarm.asc](../packaging/linux/repo/longarm.asc).
It is not the minisign key that signs the release files for self-update
([ci-cd.md 2.1](ci-cd.md#21-release-key-and-update-public-key)).

## Install

### Debian, Ubuntu

```sh
sudo install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://lubobill1990.github.io/longarm/longarm.gpg | sudo tee /etc/apt/keyrings/longarm.gpg >/dev/null
gpg --show-keys /etc/apt/keyrings/longarm.gpg     # optional: compare the fingerprint above
sudo tee /etc/apt/sources.list.d/longarm.sources >/dev/null <<'EOF'
Types: deb
URIs: https://lubobill1990.github.io/longarm/deb
Suites: stable
Components: main
Architectures: amd64 arm64
Signed-By: /etc/apt/keyrings/longarm.gpg
EOF
sudo apt-get update && sudo apt-get install longarm      # the hub: longarm-hub
```

`Signed-By` limits the key to this repository: it can't vouch for Debian's
or Ubuntu's packages, and no other key can vouch for this repository. Older
apt (one-line format):

```sh
echo "deb [signed-by=/etc/apt/keyrings/longarm.gpg] https://lubobill1990.github.io/longarm/deb stable main" |
  sudo tee /etc/apt/sources.list.d/longarm.list
```

### Fedora, RHEL, Rocky, AlmaLinux

```sh
sudo tee /etc/yum.repos.d/longarm.repo >/dev/null <<'EOF'
[longarm]
name=Longarm
baseurl=https://lubobill1990.github.io/longarm/rpm
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://lubobill1990.github.io/longarm/longarm.asc
EOF
sudo dnf install longarm
```

dnf shows the key's fingerprint before it imports it the first time; compare
it with the one above. `gpgcheck=1` checks every rpm's own signature,
`repo_gpgcheck=1` the repository metadata's (`repomd.xml.asc`).

Updates then come with `apt upgrade` / `dnf upgrade`. The package installs
into `/usr/bin` (root-owned), so longarmd running as a user doesn't
self-update it: the package manager does. The postinstall script's join
options (`/etc/longarm/join.env`) work as with the downloaded packages
([README](../README.md#join-a-hub)).

## What is signed, and how

| what | signature | checked by |
|---|---|---|
| apt: `dists/stable/Release` (hashes of the `Packages` indexes, which hold the hashes of the `.deb`s) | `InRelease` (clearsigned) and `Release.gpg` (detached) | apt, against `Signed-By` |
| `.rpm` | inside the package (nfpm, at build time) | dnf/rpm, `gpgcheck=1` |
| yum: `rpm/repodata/repomd.xml` | `repomd.xml.asc` | dnf, `repo_gpgcheck=1` |

The `.deb`s carry no signature of their own; apt verifies them through the
signed Release file, as for Debian's own archive.

The repositories are rebuilt on every stable release by release.yml's
`linux-repo` job: the deb/rpm packages of the newest 3 stable GitHub Releases
(repository variable `LONGARM_REPO_KEEP` to change it; GitHub Pages sites
are limited to 1 GB, a release adds about 170 MB), checked against each
release's `SHA256SUMS`, rpms that the key didn't sign left out (releases
before the signing). The job installs from the result in Debian, Fedora and
AlmaLinux containers before it deploys it; ci.yml's `linux-repo` job runs the
same with a throwaway key on every pull request.

Prereleases are not published to the repositories.

## Maintainers: the key

The private key, created once, lives next to the minisign key on the
maintainer's machine (never in the repository):

    ~/longarm-release-keys/longarm-gpg.private.asc   (passphrase-protected)
    ~/longarm-release-keys/longarm-gpg.password
    ~/longarm-release-keys/longarm-gpg.public.asc    (= packaging/linux/repo/longarm.asc)
    ~/longarm-release-keys/longarm-gpg.revoke.asc    (revocation certificate)

and in two repository secrets:

| secret | content |
|---|---|
| `LONGARM_GPG_PRIVATE_KEY` | `longarm-gpg.private.asc`, verbatim |
| `LONGARM_GPG_PASSPHRASE` | `longarm-gpg.password` (empty: an unprotected key) |

```sh
gh secret set LONGARM_GPG_PRIVATE_KEY --repo your-org/longarm < ~/longarm-release-keys/longarm-gpg.private.asc
gh secret set LONGARM_GPG_PASSPHRASE  --repo your-org/longarm < ~/longarm-release-keys/longarm-gpg.password
```

The release checks that the secret is the key of the committed
`packaging/linux/repo/longarm.asc` and fails otherwise; the public key users
download from Pages is the committed one, never derived from the secret.
Without the secret a release still builds and publishes everything else:
the rpm is unsigned and the `linux-repo` job only warns.

One-time setup of GitHub Pages (repository admin):

```sh
R=your-org/longarm
gh api -X POST repos/$R/pages -f build_type=workflow       # Pages source: GitHub Actions
# release.yml deploys from v* tags: allow them in the github-pages environment
gh api -X POST repos/$R/environments/github-pages/deployment-branch-policies -f name='v*' -f type=tag
```

A fork publishes under its own Pages URL with its own key: generate one,
replace `packaging/linux/repo/longarm.asc` and the fingerprint in this file,
set the two secrets.

### Rotating the key

The key doesn't expire. Rotate it when it may have leaked, or to move to a
new algorithm:

1. Generate the new key (as the original: `gpg --quick-gen-key "Longarm
   Release Signing" rsa4096 sign never` in a temporary `GNUPGHOME`), export
   it to new files in `~/longarm-release-keys/`.
2. One pull request: replace `packaging/linux/repo/longarm.asc` and the
   fingerprint here; set the two secrets right before merging it. The next
   release signs with the new key; the repositories are re-signed as a whole
   (older rpms, signed with the old key, drop out of the yum repository).
3. Users fetch the new key once: the same `curl ... longarm.gpg` line
   (apt), `sudo rpm --import https://lubobill1990.github.io/longarm/longarm.asc`
   (dnf). Until then `apt-get update` reports `NO_PUBKEY` and dnf refuses
   the metadata - nothing unsigned is ever installed. Announce it in the
   release notes.
4. If the old key leaked: publish its revocation certificate
   (`longarm-gpg.revoke.asc`) with the announcement, and remove the old key
   from rpm's keyring: `sudo rpm -e gpg-pubkey-33105307-*` (the last 8 hex
   digits of the old fingerprint).

For a planned rotation without a gap, a transition period can ship both
keys: `longarm.gpg` with both public keys (apt accepts a keyring file with
several keys) and `gpgkey=` listing both URLs, while still signing with the
old key; then switch the signing key.
