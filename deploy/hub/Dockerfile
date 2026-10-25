# longarm-hub container image: static Go binary + Litestream on distroless, non-root.
#   docker build -f deploy/hub/Dockerfile -t longarm-hub --build-arg VERSION=$(git describe --always) .
# Multi-arch (linux/amd64, linux/arm64) without emulation: Go cross-compiles
# on the build platform, Litestream is the release binary for the target.
# CI: .github/workflows/image.yml (ghcr.io/<owner>/longarm-hub).
#
# Default command `longarm-hub container` (cmd/longarm-hub/container.go):
# plain `serve`, unless the environment asks for Litestream
# (LONGARM_HUB_LITESTREAM_CONFIG) and/or the first-start bootstrap
# (LONGARM_HUB_BOOTSTRAP=1), as deploy/azure does.
ARG GO_IMAGE=golang:1.27-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/lubobill1990/longarm/internal/version.Version=${VERSION}" \
      -o /out/bin/longarm-hub ./cmd/longarm-hub
# argument-free names for az container exec (cmd/longarm-hub/container.go execAliases)
RUN for a in longarm-hub-admin-code longarm-hub-user-add longarm-hub-users; do ln -s longarm-hub /out/bin/$a; done
RUN mkdir -p /out/data /out/var/lib/longarm-hub && chown 65532:65532 /out/data /out/var/lib/longarm-hub

# Litestream (pinned release, SHA-256 from its checksums.txt)
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS litestream
ARG LITESTREAM_VERSION=0.5.17
ARG TARGETARCH
RUN set -eu; \
    case "$TARGETARCH" in \
      amd64) arch=x86_64; sum=cfb371176d164437ae869f8351cfde49bd1804ae71c61923f75c9cba9c9c006d ;; \
      arm64) arch=arm64;  sum=f8ca4a050095c1efbda2c4365172e61bf9d955ea0d9ac42f448b52e51819baa5 ;; \
      *) echo "no litestream for $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    f="litestream-${LITESTREAM_VERSION}-linux-${arch}.tar.gz"; \
    curl -fsSL --retry 3 -o "/tmp/$f" "https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/$f"; \
    echo "$sum  /tmp/$f" | sha256sum -c -; \
    mkdir -p /out && tar -xzf "/tmp/$f" -C /out litestream && chmod 0755 /out/litestream

FROM ${RUNTIME_IMAGE}
ARG VERSION=dev
LABEL org.opencontainers.image.title="longarm-hub" \
      org.opencontainers.image.description="Longarm control plane (CA, registry, scheduler, relay, panel API) with Litestream" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/bin/ /usr/local/bin/
COPY --from=litestream /out/litestream /usr/local/bin/litestream
COPY deploy/hub/litestream-abs.yml /etc/longarm-hub/litestream-abs.yml
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build --chown=65532:65532 /out/var/lib/longarm-hub /var/lib/longarm-hub
ENV LONGARM_HUB_DATA_DIR=/data LONGARM_HUB_CONFIG=/etc/longarm-hub/hub.toml
VOLUME /data
# 7443/tcp gRPC API + relay TLS fallback, 7443/udp relay (QUIC), 7444/udp second STUN port, 7480 panel/API
EXPOSE 7443/tcp 7443/udp 7444/udp 7480/tcp
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s CMD ["/usr/local/bin/longarm-hub", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/longarm-hub"]
CMD ["container"]
