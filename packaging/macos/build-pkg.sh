#!/bin/sh
# Build the signed + notarized macOS installer (longarm-<version>.pkg).
# Runs on a Mac with Xcode command line tools and a Developer ID.
#
#   VERSION=1.2.3 DIST=dist \
#   APP_IDENTITY="Developer ID Application: Your Name (TEAMID)" \
#   PKG_IDENTITY="Developer ID Installer: Your Name (TEAMID)" \
#   NOTARY_PROFILE=longarm-notary \
#   sh packaging/macos/build-pkg.sh
#
# One-time: xcrun notarytool store-credentials longarm-notary --apple-id you@example.com --team-id TEAMID
# Without identities the script builds an unsigned pkg (local testing only).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
: "${VERSION:?set VERSION}"
DIST=${DIST:-dist}
OUT=${OUT:-$DIST/longarm-$VERSION.pkg}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
root="$work/root"
mkdir -p "$root/usr/local/bin" "$root/Library/LaunchDaemons"

# Universal binaries from GoReleaser's darwin builds.
for b in longarm longarmd; do
  # shellcheck disable=SC2012 # the build dir name has a variable suffix (_v1, _v8.0)
  a=$(ls "$DIST"/${b}_darwin_amd64*/"$b" | head -1)
  # shellcheck disable=SC2012
  r=$(ls "$DIST"/${b}_darwin_arm64*/"$b" | head -1)
  lipo -create -output "$root/usr/local/bin/$b" "$a" "$r"
  chmod 0755 "$root/usr/local/bin/$b"
  if [ -n "${APP_IDENTITY:-}" ]; then
    codesign --force --options runtime --timestamp --sign "$APP_IDENTITY" "$root/usr/local/bin/$b"
  fi
done
cp "$here/com.longarm.longarmd.plist.example" "$root/Library/LaunchDaemons/com.longarm.longarmd.plist.example"

pkgbuild --root "$root" --identifier com.longarm.pkg --version "$VERSION" \
  --scripts "$here/scripts" --install-location / "$work/longarm-component.pkg"
sed "s/@VERSION@/$VERSION/g" "$here/distribution.xml" > "$work/distribution.xml"
if [ -n "${PKG_IDENTITY:-}" ]; then
  productbuild --distribution "$work/distribution.xml" --package-path "$work" --sign "$PKG_IDENTITY" "$OUT"
else
  productbuild --distribution "$work/distribution.xml" --package-path "$work" "$OUT"
  echo "warning: unsigned pkg (set APP_IDENTITY / PKG_IDENTITY)" >&2
fi
if [ -n "${NOTARY_PROFILE:-}" ] && [ -n "${PKG_IDENTITY:-}" ]; then
  xcrun notarytool submit "$OUT" --keychain-profile "$NOTARY_PROFILE" --wait
  xcrun stapler staple "$OUT"
  spctl --assess --type install -vv "$OUT"
fi
echo "built $OUT"
