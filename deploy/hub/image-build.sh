#!/bin/sh
# image-build.sh OUT: the build steps of deploy/hub/Dockerfile (shared by
# its two build stages), run in the Go image from the source root:
#   OUT/bin/longarm-hub (+ argument-free aliases), OUT/litestream,
#   OUT/data and OUT/var/lib/longarm-hub (owned by the image's nonroot user)
# Target: $TARGETARCH (BuildKit sets it), else the Go image's own
# architecture (ACR Tasks, the legacy builder). Version: $VERSION.
set -eu
out=${1:?usage: image-build.sh OUT}
arch=${TARGETARCH:-$(go env GOARCH)}

CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
	-ldflags "-s -w -X github.com/lubobill1990/longarm/internal/version.Version=${VERSION:-dev}" \
	-o "$out/bin/longarm-hub" ./cmd/longarm-hub
# https = ":443" (hub.toml, LONGARM_HUB_HTTPS) as user nonroot: only the
# right to bind ports below 1024 (deploy/azure without Caddy)
apt-get update -qq
apt-get install -y -qq --no-install-recommends libcap2-bin >/dev/null
setcap cap_net_bind_service=+ep "$out/bin/longarm-hub"
getcap "$out/bin/longarm-hub"
# argument-free names for az container exec (cmd/longarm-hub/container.go execAliases)
for a in longarm-hub-admin-code longarm-hub-user-add longarm-hub-users; do
	ln -s longarm-hub "$out/bin/$a"
done
mkdir -p "$out/data" "$out/var/lib/longarm-hub"
chown 65532:65532 "$out/data" "$out/var/lib/longarm-hub"

# Litestream (pinned release, SHA-256 from its checksums.txt)
v=${LITESTREAM_VERSION:-0.5.17}
case $arch in
amd64) la=x86_64 sum=cfb371176d164437ae869f8351cfde49bd1804ae71c61923f75c9cba9c9c006d ;;
arm64) la=arm64 sum=f8ca4a050095c1efbda2c4365172e61bf9d955ea0d9ac42f448b52e51819baa5 ;;
*)
	echo "no litestream for $arch" >&2
	exit 1
	;;
esac
[ "$v" = 0.5.17 ] || {
	echo "image-build.sh: update the Litestream checksums for $v" >&2
	exit 1
}
f="litestream-$v-linux-$la.tar.gz"
curl -fsSL --retry 3 -o "/tmp/$f" "https://github.com/benbjohnson/litestream/releases/download/v$v/$f"
echo "$sum  /tmp/$f" | sha256sum -c -
tar -xzf "/tmp/$f" -C "$out" litestream
chmod 0755 "$out/litestream"
rm -f "/tmp/$f"
