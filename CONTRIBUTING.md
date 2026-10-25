# Contributing to Longarm

Thanks for your interest! Bug reports, ideas and pull requests are welcome.

## Before you start

- For anything bigger than a small fix, open an issue first so we can agree on
  the approach. The architecture is described in [docs/design.md](docs/design.md).
- Security issues: see [SECURITY.md](SECURITY.md), not the issue tracker.

## Development setup

- Go ≥ 1.27.3 (with the default `GOTOOLCHAIN=auto` an older Go downloads it).
- Optional: node + pnpm for the web UI (`web/`), `buf` + `protoc-gen-go` +
  `protoc-gen-go-grpc` for `proto/`.

```sh
go build -o bin/ ./cmd/...        # longarm, longarmd, longarm-hub, longarm-relay, longarm-tray
go test ./...                     # unit + end-to-end tests (a few minutes)
go vet ./...
```

The end-to-end tests in `e2e/` start real hubs, workers and clients on
loopback; they need no root or administrator rights. Tests that install
services or MSIs, change firewall rules, use a live tailnet or a browser are
opt-in (`LONGARM_SERVICE_TEST=1`, `LONGARM_MSI_TEST=1`,
`LONGARM_PLAYWRIGHT=1`, … - see the test files) and never run in CI.

After changing:

- `proto/`: `buf generate` and commit the generated code;
- `web/`: `go generate ./internal/webui` and commit `internal/webui/dist`;
  `cd web && pnpm test`.

## Pull requests

- Keep changes focused; add tests (an end-to-end test for user-visible
  behaviour) and update [docs/usage.md](docs/usage.md) or the README.
- CI must pass: gofmt, `go vet`, cross-compilation for
  windows/linux/darwin × amd64/arm64, shellcheck, actionlint, the tests on
  Linux, Windows and macOS, and `scripts/privacy-check.sh`.
- Don't put real hostnames, IP addresses, user names or keys in code, tests or
  docs: use `example.com`, RFC 5737 addresses (`192.0.2.x`, `198.51.100.x`,
  `203.0.113.x`), `100.64.0.x` for tailnet addresses and names like
  `worker-1` / `alice`.
- By contributing you agree that your contribution is licensed under the
  [MIT License](LICENSE).
