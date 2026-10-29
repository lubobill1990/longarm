#!/bin/sh
# image-build.sh OUT: the build steps of deploy/hub/Dockerfile (shared by
# its two build stages), run in the Go image from the source root:
#   OUT/bin/longarm-hub (+ argument-free aliases), OUT/bin/longarm-relay, OUT/litestream,
#   OUT/data and OUT/var/lib/longarm-hub (owned by the image's nonroot user)
# Target: $TARGETARCH (BuildKit sets it), else the Go image's own
# architecture (ACR Tasks, the legacy builder). Version: $VERSION.
set -eu
out=${1:?usage: image-build.sh OUT}
arch=${TARGETARCH:-$(go env GOARCH)}

CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
	-ldflags "-s -w -X github.com/lubobill1990/longarm/internal/version.Version=${VERSION:-dev}" \
	-o "$out/bin/longarm-hub" ./cmd/longarm-hub
# the external relay (deploy/azure deploy.sh --relay runs this image with it)
CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath 	-ldflags "-s -w -X github.com/lubobill1990/longarm/internal/version.Version=${VERSION:-dev}" 	-o "$out/bin/longarm-relay" ./cmd/longarm-relay
# https = ":443" (hub.toml, LONGARM_HUB_HTTPS) as user nonroot: only the
# right to bind ports below 1024 (deploy/azure without Caddy)
apt-get update -qq
apt-get install -y -qq --no-install-recommends libcap2-bin >/dev/null
setcap cap_net_bind_service=+ep "$out/bin/longarm-hub"
getcap "$out/bin/longarm-hub"
# argument-free names for az container exec (cmd/longarm-hub/container.go execAliases)
for a in longarm-hub-admin-code longarm-hub-user-add longarm-hub-users longarm-hub-release-cert longarm-hub-relay-cert; do
	ln -s longarm-hub "$out/bin/$a"
done
mkdir -p "$out/data" "$out/var/lib/longarm-hub"
chown 65532:65532 "$out/data" "$out/var/lib/longarm-hub"

# Litestream, built from source at a pinned upstream commit (module
# version; go verifies it against the checksum database, sum.golang.org).
# Not the 0.5.17 release: its Azure Blob client lists a level "from TXID X"
# as the blob name prefix X, so every compaction took exactly one file of the
# level below and fell behind for good - L0 was never cleaned up, L2+ never
# advanced, the replica grew without bound and the restore on every start,
# which opens every file of its plan at once, ran out of memory (the
# 2026-10-29 incident, deploy/azure/README.md "Replica size and memory").
# Fixed upstream in benbjohnson/litestream#1461 (24ead772, after 0.5.17);
# go back to a release binary once one has it. Static like the release
# binaries (CGO_ENABLED=0, .goreleaser.yml).
lsmod=github.com/benbjohnson/litestream
lsver=${LITESTREAM_VERSION:-v0.5.18-0.20261008011441-24ead772b436}
CGO_ENABLED=0 GOOS=linux GOARCH=$arch GOBIN='' go install -trimpath \
	-ldflags "-s -w -X main.Version=$lsver" "$lsmod/cmd/litestream@$lsver"
# a native build lands in GOPATH/bin, a cross-compiled one in GOPATH/bin/linux_<arch>
gobin=$(go env GOPATH)/bin
[ -f "$gobin/litestream" ] || gobin=$gobin/linux_$arch
install -m 0755 "$gobin/litestream" "$out/litestream"
rm -f "$gobin/litestream"
