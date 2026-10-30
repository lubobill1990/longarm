#!/bin/sh
# Build the macOS installer for one architecture: longarm-darwin-<arch>.pkg,
# a flat distribution package that installs longarm and longarmd into
# /usr/local/bin (scripts/postinstall: firewall, optional unattended join,
# restart of an installed LaunchDaemon).
#
#   VERSION=1.2.3 ARCH=arm64 BINDIR=dir-with-longarm-and-longarmd OUT=longarm-darwin-arm64.pkg \
#   [PKG_IDENTITY="Developer ID Installer: Your Name (TEAMID)" [KEYCHAIN=path]] \
#   sh packaging/macos/build-pkg.sh
#
# The binaries in BINDIR are packaged as they are: sign them first (Developer
# ID Application, hardened runtime, secure timestamp). The release does that,
# and notarizes and staples the package: .github/scripts/macos-sign.sh. Without
# PKG_IDENTITY the package is unsigned (MDM-managed Macs refuse it).
# Needs a Mac (pkgbuild, productbuild).
set -eu
# the payload's modes are installed as they are (also on /usr/local/bin):
# never inherit a caller's umask 077
umask 022
here=$(cd "$(dirname "$0")" && pwd)
: "${VERSION:?set VERSION}" "${ARCH:?set ARCH (amd64 or arm64)}" "${BINDIR:?set BINDIR}" "${OUT:?set OUT}"
case $ARCH in
amd64) host=x86_64 ;;
arm64) host=arm64 ;;
*)
	echo "build-pkg.sh: ARCH must be amd64 or arm64" >&2
	exit 2
	;;
esac
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
root="$work/root"
mkdir -p "$root/usr/local/bin" "$root/usr/local/share/longarm"
chmod 0755 "$root" "$root/usr" "$root/usr/local" "$root/usr/local/bin" "$root/usr/local/share" "$root/usr/local/share/longarm"
for b in longarm longarmd; do
	cp "$BINDIR/$b" "$root/usr/local/bin/$b"
	chmod 0755 "$root/usr/local/bin/$b"
done
cp "$here/com.longarm.longarmd.plist.example" "$root/usr/local/share/longarm/"
chmod 0644 "$root/usr/local/share/longarm/com.longarm.longarmd.plist.example"
mkdir -p "$work/scripts"
cp "$here/scripts/postinstall" "$work/scripts/postinstall"
chmod 0755 "$work/scripts/postinstall"

# root:wheel, whoever builds it
pkgbuild --root "$root" --identifier com.longarm.pkg --version "$VERSION" \
	--ownership recommended --scripts "$work/scripts" --install-location / \
	"$work/longarm-component.pkg"
sed -e "s/@VERSION@/$VERSION/g" -e "s/@HOSTARCH@/$host/g" "$here/distribution.xml" >"$work/distribution.xml"
set -- --distribution "$work/distribution.xml" --package-path "$work"
if [ -n "${PKG_IDENTITY:-}" ]; then
	set -- "$@" --sign "$PKG_IDENTITY" --timestamp
	if [ -n "${KEYCHAIN:-}" ]; then set -- "$@" --keychain "$KEYCHAIN"; fi
fi
productbuild "$@" "$OUT"
if [ -z "${PKG_IDENTITY:-}" ]; then
	echo "build-pkg.sh: $OUT is NOT signed (no PKG_IDENTITY)" >&2
fi
echo "built $OUT"
