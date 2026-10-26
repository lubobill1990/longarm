# AZURE-E2E report: fixes from the first real Azure run

Branch `azure-e2e-fixes`, PR #4 against `main` (not merged, not tagged).
The first two commits of the PR (five fixes: `az()` recursion, port
7445/7446, Caddy memory 0.3 GB, registry server of `--registry-user`, the
caller's `AZURE_STORAGE_*`) are described in the PR; this report covers the
remaining findings of the real run (ACI eastasia, hub image built with
`az acr build`, Ubuntu 24.04 worker, Windows laptop).

## Done

| # | Finding | Fix | Where |
|---|---|---|---|
| 1 | `az acr build` failed on the Dockerfile ("failed to scan dependencies") | ACR Tasks' scanner (Azure/acr-builder `scan/dependencies.go`) only understands `FROM <image> [AS <name>]` on every FROM line up to the target. The Dockerfile now has native stages first and an `acr` target (`az acr build --target acr`); the default (last) stage stays BuildKit's cross-compiling one (`--platform=$BUILDPLATFORM`, multi-arch without emulation, as CI publishes). Build steps shared in `deploy/hub/image-build.sh` (arch from `TARGETARCH`, else the Go image's own). `deploy.sh --build-in-acr` / `deploy.ps1 -BuildInAcr`: creates a Basic ACR in the hub's resource group (tagged, reused), `az acr build` from the checkout, deploys `<acr>.azurecr.io/longarm-hub:build-<UTC>` with the registry's admin login; re-runs find that login by themselves | `deploy/hub/Dockerfile`, `deploy/hub/image-build.sh`, `deploy/azure/deploy.{sh,ps1}`, `.github/workflows/image.yml` (builds and starts the `acr` stage), `e2e/dockerfile_test.go` (parses the Dockerfile like ACR Tasks) |
| 2 | Per-user mode on Linux couldn't start: data dir `/root/.longarm`, `/root` not traversable | Root's default data dir on Linux is the system dir `/var/lib/longarmd` (LocalSystem on Windows: `C:\ProgramData\Longarm\longarmd`); `longarmd join` as root uses it and prints it; `longarmd service install --isolation per-user` uses it, migrates an enrollment found in `~root/.longarm` (copied safely: 0600/0700, staged + renamed, sources must be root-owned and not group/world-writable, no symlinks, home from passwd not `$HOME`), refuses a data dir below a non-traversable directory with the blocking path, `--dry-run`. The system dir is checked/locked down when it exists (Windows: owner and DACL; Unix: root-owned, not group/world-writable). Legacy root installs with only `~root/.longarm` keep working in shared mode | `internal/longarmd/datadir*.go`, `cmd/longarmd`, `internal/platform/windows/trust.go`, `internal/hub/install.go`, `packaging/linux/*`, `packaging/windows/*`, `docs/usage.md` "Data directory" |
| 3 | ACI source-NATs inbound UDP: the hub's STUN reported `10.92.0.x` for every node → hard/hard, never punched | Nodes: a private/CGNAT/link-local/loopback mapped address from a STUN server on a public address is not a candidate and not used for the NAT type (logged once per server); classification from public observations only; < 2 → unknown (punch). Hub: `relay_stun_advertise` / `LONGARM_HUB_RELAY_STUN_ADVERTISE` (`self`, `host:port`, `off`); detects that its own STUN is SNATed (a UDP session from a private address whose node reports a public mapping, no session from a public address) and then stops advertising its own ports and passing observed addresses (`"stun_snat": true` in `/sessions`, log "look source-NATed"). Template: `stunServers` (default `stun.cloudflare.com:3478,stun.l.google.com:19302`) → `self,<servers>`; `--stun-servers` in the scripts, kept on re-runs | `internal/relay/{addr,server,client,proto}.go`, `internal/transport/longnet/{nat,node}.go`, `internal/hub/config*.go`, `deploy/azure/main.bicep` |
| 4 | `Microsoft.ContainerInstance` NotRegistered on a new subscription | The scripts check and register ContainerInstance + Storage, OperationalInsights with `--log-analytics`, ContainerRegistry with `--build-in-acr`, and wait until Registered (10 min limit, clear message without rights) | `deploy/azure/deploy.{sh,ps1}` |
| 5 | Panel deploy hung in Creating, no events, 30+ min | Deployments run with `--no-wait` under a unique name and are polled; a group in Creating/Pending with waiting containers and **no events** for `--stuck-timeout` (600 s) stops with an actionable message (`--recreate`, `--no-panel --recreate`, `az container show`); a failed deployment prints its failed operations | `deploy/azure/deploy.{sh,ps1}` |
| 6 | Test gaps | Deploy e2e: the first run finds the fake `az` **on PATH** (no `LONGARM_DEPLOY_AZ`), every run has a hostile `AZURE_STORAGE_CONNECTION_STRING` / `AZURE_STORAGE_SAS_TOKEN` / `AZURE_STORAGE_ACCOUNT` (the fake fails like the real az), a new subscription, `--registry-user` with an ACR image → `registryServer=example.azurecr.io` (deploy.ps1 lacked this: fixed), `--build-in-acr`, a stuck group, a failed deployment, `--stun-servers`. `deploy/azure/test/acirules`: evaluates the compiled template (small ARM expression evaluator) for every parameter combination (bools, allowed values, int bounds and values next to fixed ports) and checks ACI's rules: one port number per container and group across protocols, group ports exposed by a container, memory in 0.1 GB steps, CPU in 0.01 steps, ≤ 4 vCPU / 16 GB, env names unique with value xor secureValue, volume mounts; runs in the CI `azure` job and as a Go test (it found that `relayUdpPort` 7442/7443 collides with 7443/tcp → `@minValue(7444)`) | `e2e/azure_deploy_test.go`, `e2e/helper/fakeaz.go`, `deploy/azure/test/acirules`, `.github/workflows/ci.yml` |
| 7 | E2E.md + isolation gaps | E2E.md: provider registration, 7445/7446 and the `?tcp=` listen URL, ACR build path, timings (deploy ~4 min, restart-restore ~36 s, Let's Encrypt ~2 min, recreate 208 s), the SNAT result and what to expect now, a per-user worker section with the isolation checks of the real run. **Loopback between users on Linux is now blocked**: longarmd owns an nftables table `inet longarm` (output hook on `lo`): the first packet of a connection records the sending account in the conntrack mark (bits 16-31 = low 16 bits of the uid), the listener's SYN-ACK / UDP reply from a different `la-*` account is dropped and the client's retried SYN gets a reset. `[isolation] loopback = "isolate"` (default on Linux; warns and runs on without nft) / `"require"` / `"allow"`; table replaced atomically on account changes, removed by `leave --purge`, uninstall, deb prerm, `service uninstall`. `/proc` command lines: still `hidepid=invisible` (documented, longarmd warns) | `deploy/azure/E2E.md`, `internal/loopguard`, `internal/userspace/userspace.go`, `docs/security.md` "Loopback between users (Linux)" |
| - | Also fixed on the way | the printed worker URL lacked `?tcp=<fqdn>:7443` (UDP 7445 ≠ TCP 7443: the TLS fallback would have dialed 7445/tcp); deploy README still described 7443/udp; CI's PSScriptAnalyzer install retries (PowerShell Gallery flakes); the deploy script runners run in parallel (e2e time) | |

## Build, run, test

```sh
go build ./...                                   # Go >= 1.27.3 (GOTOOLCHAIN=auto)
go vet ./...                                     # + GOOS/GOARCH for windows|linux|darwin x amd64|arm64
go test ./...                                    # unit + e2e (root/admin-only parts skip)
go test -run TestAzureDeployScripts -v ./e2e     # deploy.sh, deploy.ps1 (pwsh, Windows PowerShell) against the fake az
go run ./deploy/azure/test/acirules deploy/azure/azuredeploy.json      # ACI rules
bicep build deploy/azure/main.bicep --outfile deploy/azure/azuredeploy.json   # bicep 0.48.1 (CI checks it is current)
# Linux root (CI does this): loopguard + data dir tests as root, then the per-user e2e
go test -c -o bin/loopguard.test ./internal/loopguard && sudo bin/loopguard.test -test.v
go test -c -o bin/longarmd.test ./internal/longarmd && sudo bin/longarmd.test -test.run 'DataDir|Migrate|RootHome' -test.v
(see docs/TENANCY-REPORT.md for the per-user e2e as root: -test.run PerUser)
# the hub image, also the ACR stage
docker buildx build -f deploy/hub/Dockerfile .                 # default: BuildKit cross-compile
docker buildx build -f deploy/hub/Dockerfile --target acr .    # what az acr build --target acr builds
./deploy/azure/deploy.sh --name myhub --location eastasia --build-in-acr   # real Azure, no docker
```

## Tests

New or extended:

- `e2e/azure_deploy_test.go` + `e2e/helper/fakeaz.go` (above, finding 6); `e2e/dockerfile_test.go`.
- `deploy/azure/test/acirules` (+ negative cases: 7443 for TCP and UDP, 0.25 GB, 4 vCPU, a group port without container port).
- `internal/transport/longnet` `TestClassifyNAT` (private + public, one public, two equal/different public, only private from public servers, LAN, loopback, open), `internal/relay/snat_test.go` (PublicAddr, ExpandSTUN, the SNAT verdict and the hub's reply), `TestConfigEnv`.
- `e2e/longnet_snat_test.go` `TestLongNetSNATedRelaySTUN`: a relay whose STUN answers a private address (test hook `LONGARM_RELAY_TEST_FAKE_SNAT`) plus an external STUN server; the worker's NAT type is not hard, the relay detects SNAT, a later client never queries the hub's STUN, the bulk transfer goes **direct** (12 KB of 539 KB via the relay).
- `internal/longarmd/datadir*_test.go` (defaults, service dir resolution, migration incl. foreign owners / writable sources / partial copies, system dir trust on Unix and Windows), `internal/platform/windows/trust_test.go`, `e2e/peruser_unix_test.go` `TestPerUserRootDataDirDefault` (Linux root: join without `--data-dir` → system dir; per-user install accepts it, refuses a non-traversable home, adopts `~root/.longarm`), `e2e/peruser_windows_test.go` `TestPerUserServiceDataDirWindows` (administrator, temporary never-started service).
- `internal/loopguard` (golden ruleset, stable tags, collisions, `nft -c` as root), `internal/userspace/loopback_test.go`, `e2e/peruser_test.go` loopback checks (Linux root with nft: bob → alice's listener on 127.0.0.1 and ::1 refused, alice → alice works, root → alice works, the Longarm forward works).

Results:

| Where | Result |
|---|---|
| this machine (Windows 11, elevated) | `go test -count=1 ./...`: all 31 packages ok in 267 s (e2e 247 s); `TestAzureDeployScripts` deploy.sh / pwsh / powershell 5.1 pass; `TestPerUserServiceDataDirWindows` and the Windows data dir ACL tests pass (temporary service removed); vet + e2e test binary for 6 targets; gofmt; shellcheck 0.11; PSScriptAnalyzer 1.24 (0 findings); bicep 0.48.1 build (identical) + lint (clean); acirules ok; privacy-check ok |
| CI (GitHub-hosted) | lint, azure (bicep, ACI rules, PSScriptAnalyzer), image smoke (+ `acr` stage) and the test matrix on ubuntu (incl. per-user + loopguard as root), windows (incl. administrator) and macos: green on `5539dcf` (the report commit), incl. as root on ubuntu `TestRulesetNftCheck`, the data dir tests, `TestPerUserIsolation` with the loopback checks and `TestPerUserRootDataDirDefault`, and as administrator on windows the data dir ACL tests and `TestPerUserServiceDataDirWindows` (flaky tests seen earlier and re-run: `TestE2E/job_logs_wait_exit_code`, `TestUITraySmoke`, macOS `TestSelfUpdate` - also flaky on main) |

## Design decisions and deviations

- **System data dir is `/var/lib/longarmd`, not `/var/lib/longarm`** (the brief's example): the shipped deb's `longarmd-peruser.service` already uses `/var/lib/longarmd`; another default would disagree with it and break deb upgrades. Same on Windows: `C:\ProgramData\Longarm\longarmd` = the MSI's `DATADIR`. Users dir: `<data>/users`.
- **Legacy root installs**: while the system dir holds no enrollment and `~root/.longarm` does, root keeps using `~root/.longarm` (shared mode keeps working); the per-user service install migrates it. An elevated Windows administrator keeps `~\.longarm` unless only the ProgramData dir is enrolled.
- **ACR**: a separate `acr` stage instead of dropping `--platform` (which would make CI's arm64 image build under emulation). `az acr build` without `--target acr` still fails - documented. Admin credentials of the ACR (not a managed identity: that needs a role assignment = Owner/User Access Administrator).
- **STUN on ACI**: the template advertises `self,<public servers>`: the hub's own ports stay useful outside ACI, and the hub drops them once it detects SNAT. With `--stun-servers none` (`self` only) ACI deployments stay on the relay.
- **Loopback guard on by default** (`isolate`), uid-based (not cgroup-based: sessions, exec and the user helper are covered too); kept across longarmd restarts (jobs keep running across restarts); fails closed for answers without a full socket.
- **Stuck detection** only fires with zero events (no false positives on slow image pulls, which have events).

## Known gaps

- Nothing ran on real Azure here (no credentials): the ACR build, provider registration, stuck detection and the STUN advertisement are tested against the fake `az` / locally; the integrator re-runs deploy/azure/E2E.md.
- SNAT detection looks at UDP sessions only (whether ACI also SNATs TCP is unknown; TCP relay sessions don't affect STUN).
- Loopback guard: one-way UDP datagrams between accounts still arrive (only replies are dropped); unix sockets are not covered; needs conntrack on `lo`; one per-user longarmd per machine (fixed table name); uids whose low 16 bits collide are refused (warning, error with `require`); Windows keeps the loopback gap. Nothing of it was run on Linux here: CI runs it as root.
- `/proc` command lines of other users stay visible unless `/proc` is mounted `hidepid=invisible` (documented; longarmd warns).
- `deploy.sh` once failed on CI reading `bootstrap.env` right after fetching it (not reproduced; the scripts download it into a private 0700 temp dir, read it into memory and delete it at once; `deploy.sh` parses it through a pipe, not a here-string, which bash < 5.1 backs with a `$TMPDIR` file).

## Needs from a human

1. Review PR #4 (not merged, not tagged).
2. Re-run deploy/azure/E2E.md on Azure, ideally with `--build-in-acr` on a fresh subscription, and record: provider registration, direct vs relay paths and NAT types in step 4 (expected: direct between the laptop and the VM unless one side has a symmetric NAT), the per-user table of step 6 (loopback between `la-*` accounts now refused).
3. A production per-user Linux worker: `hidepid=invisible` on `/proc`; nftables installed (else longarmd warns that loopback isolation is off).
