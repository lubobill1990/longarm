# longarm-hub container image: static Go binary on distroless, non-root.
#   docker build -f deploy/hub/Dockerfile -t longarm-hub --build-arg VERSION=$(git describe --always) .
FROM golang:1.27 AS build
ENV GOTOOLCHAIN=auto
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/lubobill1990/longarm/internal/version.Version=${VERSION}" \
      -o /out/longarm-hub ./cmd/longarm-hub
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/longarm-hub /usr/local/bin/longarm-hub
COPY --from=build --chown=65532:65532 /out/data /data
ENV LONGARM_HUB_DATA_DIR=/data LONGARM_HUB_CONFIG=/etc/longarm-hub/hub.toml
VOLUME /data
# 7443/tcp gRPC API + relay TLS fallback, 7443/udp relay (QUIC), 7444/udp second STUN port, 7480 panel/API
EXPOSE 7443/tcp 7443/udp 7444/udp 7480/tcp
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s CMD ["/usr/local/bin/longarm-hub", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/longarm-hub"]
CMD ["serve"]
