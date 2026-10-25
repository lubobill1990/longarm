#!/bin/sh
# Build a Longarm release on Linux/macOS: GoReleaser (all platforms) and the
# flat, minisign-signed directory for `longarm-hub release add`. The MSI needs
# Windows (packaging/build.ps1); the macOS .pkg needs a Mac (packaging/macos).
#
#   sh packaging/build.sh --snapshot
#   LONGARM_UPDATE_PUBKEY=$(tail -1 keys/longarm-release.pub) LONGARM_RELEASE_KEY=keys/longarm-release.key sh packaging/build.sh
set -eu
cd "$(dirname "$0")/.."
snapshot=""
[ "${1:-}" = "--snapshot" ] && snapshot="--snapshot"
if [ -d cmd/longarm-tray ]; then export LONGARM_TRAY=1; else unset LONGARM_TRAY || true; fi
skip=""
command -v docker >/dev/null 2>&1 || skip="--skip=docker"
# shellcheck disable=SC2086
goreleaser release --clean $snapshot $skip
# The native Linux tray + its deb (dist-tray/): only on a Linux amd64 builder
# with the GTK 4 / WebKitGTK 6.0 / libsoup 3 dev packages (cgo).
if [ -d cmd/longarm-tray ] && [ "$(uname -s)" = Linux ] && [ "$(uname -m)" = x86_64 ] &&
	pkg-config --exists gtk4 webkitgtk-6.0 libsoup-3.0 2>/dev/null; then
	# shellcheck disable=SC2086
	goreleaser release --clean $snapshot --config packaging/linux/goreleaser-tray.yaml
else
	echo "note: no native Linux tray (needs Linux amd64 + libgtk-4-dev libwebkitgtk-6.0-dev libsoup-3.0-dev)"
fi
key_arg=""
[ -n "${LONGARM_RELEASE_KEY:-}" ] && key_arg="--key $LONGARM_RELEASE_KEY"
# shellcheck disable=SC2086
go run ./packaging/hubrelease --dist dist --out dist/hub-release $key_arg
version=$(sed -n 's/.*"version":"\([^"]*\)".*/\1/p' dist/metadata.json)
ls -l dist/hub-release
echo "publish from this machine: longarm admin release publish dist/hub-release --version $version (or on the hub: longarm-hub release add dist/hub-release --version $version)"
