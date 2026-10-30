# macOS .pkg

`build-pkg.sh` (on a Mac) makes `longarm-darwin-<arch>.pkg`, a flat
distribution package for one architecture (`hostArchitectures` arm64 or
x86_64, macOS 12+): `longarm` and `longarmd` in `/usr/local/bin`, the
LaunchDaemon example in `/usr/local/share/longarm`, and `scripts/postinstall`.
With `PKG_IDENTITY` it is signed (`productbuild --sign`, Developer ID
Installer). It packages the binaries as given: sign them first.

Releases build it in release.yml's **build-macos** job with
[`.github/scripts/macos-sign.sh`](../../.github/scripts/macos-sign.sh): the
binaries signed with a Developer ID Application certificate (hardened
runtime, secure timestamp, identifiers `com.longarm.<tool>`), the pkg signed,
both notarized with `notarytool` and the ticket stapled to the pkg. The
credentials and how to get them: [docs/ci-cd.md 2.8](../../docs/ci-cd.md#28-macos-signing-and-notarization-apple-developer-id);
installing it and what an MDM administrator needs:
[docs/usage.md](../../docs/usage.md#macos-installer-pkg-and-mdm).
[`macos-pkg.yml`](../../.github/workflows/macos-pkg.yml) tests both on pull
requests with self-signed stand-in certificates.

`certs/`: Apple's Developer ID intermediate CAs (G2 and the older one),
from <https://www.apple.com/certificateauthority/>, pinned by SHA256 in
macos-sign.sh; imported next to the `.p12` so codesign embeds the full chain.

By hand on a Mac (binaries from `goreleaser build`):

```sh
for b in longarm longarmd; do
  codesign --force --options runtime --timestamp --identifier com.longarm.$b \
    --sign "Developer ID Application: Your Name (TEAMID)" bin/$b
done
VERSION=1.2.3 ARCH=arm64 BINDIR=bin OUT=longarm-darwin-arm64.pkg \
  PKG_IDENTITY="Developer ID Installer: Your Name (TEAMID)" sh packaging/macos/build-pkg.sh
xcrun notarytool submit longarm-darwin-arm64.pkg --key AuthKey_KEYID.p8 --key-id KEYID --issuer ISSUER --wait
xcrun stapler staple longarm-darwin-arm64.pkg
```

`postinstall` (as root): adds longarm/longarmd to the application firewall
when it is on (the macOS equivalent of the Windows inbound rules: without
it, unsolicited inbound UDP - hole punching, tailscale direct - is dropped);
restarts an installed `com.longarm.longarmd` LaunchDaemon on an upgrade; and
the optional unattended setup from the preference domain `com.longarm`
(`Roles`, `Hub`, `JoinCode`, `User`, `CAFingerprint`; a configuration
profile's managed preferences or a root-owned
`/Library/Preferences/com.longarm.plist`), validated before use, like the
deb's `/etc/longarm/join.env`. Without it the pkg only installs the binaries.

Uninstall: `sudo longarmd service uninstall; longarmd leave --kill-jobs; sudo rm /usr/local/bin/longarm /usr/local/bin/longarmd; sudo rm -rf /usr/local/share/longarm; sudo pkgutil --forget com.longarm.pkg`.
