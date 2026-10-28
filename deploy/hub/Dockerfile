# longarm-hub container image: static Go binary + Litestream on distroless, non-root.
#   docker build -f deploy/hub/Dockerfile -t longarm-hub --build-arg VERSION=$(git describe --always) .
# Multi-arch (linux/amd64, linux/arm64) without emulation (buildx): Go
# cross-compiles on the build platform, Litestream is the release binary
# for the target. CI: .github/workflows/image.yml (ghcr.io/<owner>/longarm-hub).
#
# Azure Container Registry without docker (deploy/azure/deploy.sh --build-in-acr):
#   az acr build -r <registry> -t longarm-hub:<tag> --target acr -f deploy/hub/Dockerfile .
# ACR Tasks scans every FROM line up to the --target stage and only
# understands "FROM <image> [AS <name>]" (no --platform=...): the stages
# for it come first and build natively for the agent's platform; the
# default (last) stage is BuildKit's cross-compiling one.
#
# Default command `longarm-hub container` (cmd/longarm-hub/container.go):
# plain `serve`, unless the environment asks for Litestream
# (LONGARM_HUB_LITESTREAM_CONFIG) and/or the first-start bootstrap
# (LONGARM_HUB_BOOTSTRAP=1), as deploy/azure does.
ARG GO_IMAGE=golang:1.27-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

# build steps: deploy/hub/image-build.sh (OUT/bin, OUT/litestream, OUT/data, OUT/var)
FROM ${GO_IMAGE} AS build-native
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETARCH
RUN tr -d '\r' < deploy/hub/image-build.sh | sh -s /out

FROM ${RUNTIME_IMAGE} AS runtime
ARG VERSION=dev
LABEL org.opencontainers.image.title="longarm-hub" \
      org.opencontainers.image.description="Longarm control plane (CA, registry, scheduler, relay, panel API) with Litestream" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}"
COPY deploy/hub/litestream-abs.yml deploy/hub/litestream-abs-headscale.yml /etc/longarm-hub/
ENV LONGARM_HUB_DATA_DIR=/data LONGARM_HUB_CONFIG=/etc/longarm-hub/hub.toml
VOLUME /data
# 7443/tcp gRPC API + relay TLS fallback, 7443/udp relay (QUIC), 7444/udp second STUN port, 7480 panel/API,
# 443/80 panel over https (optional)
EXPOSE 7443/tcp 7443/udp 7444/udp 7480/tcp 443/tcp 80/tcp
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s CMD ["/usr/local/bin/longarm-hub", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/longarm-hub"]
CMD ["container"]

# az acr build --target acr
FROM runtime AS acr
COPY --from=build-native /out/bin/ /usr/local/bin/
COPY --from=build-native /out/litestream /usr/local/bin/litestream
COPY --from=build-native --chown=65532:65532 /out/data /data
COPY --from=build-native --chown=65532:65532 /out/var/lib/longarm-hub /var/lib/longarm-hub

# BuildKit (docker build, buildx --platform ...): cross-compiled on the build platform
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETARCH
RUN tr -d '\r' < deploy/hub/image-build.sh | sh -s /out

FROM runtime
COPY --from=build /out/bin/ /usr/local/bin/
COPY --from=build /out/litestream /usr/local/bin/litestream
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build --chown=65532:65532 /out/var/lib/longarm-hub /var/lib/longarm-hub
