# Flaky tests

Tests that failed once and passed on a rerun without a change, while
merging #112-#114 (2026-11-03). Each is a race worth a look; start from the
failing assertion.

## Seen on GitHub Actions

| test | runner | failure |
|---|---|---|
| `e2e TestInstallPs1AccountOnlyAndUninstall` | windows-latest | (main after #112) |
| `internal/hub TestRestartAnnounced` | windows-latest | (main after #112) |
| `internal/signin TestEnterPresses` | windows-latest | (main, rerun of the above) |
| `internal/telemetry/appinsights TestRetry` | macos-latest | (#113) |
| `e2e TestUITraySmoke` | macos-latest | (#113) |
| `e2e TestNodeIDs/rename_and_dispatch` | ubuntu-latest | the hub recorded the rename before the worker's listeners served the new certificate; fixed in #113 by waiting for the new name to answer |
| `e2e TestProgramsFixtures` | ubuntu-latest | `worker-ls.json: nodes[].toolchain.updated_at is missing`: captured before the worker's first inventory; fixed by waiting for it |
| `e2e TestExternalRelay` | ubuntu-latest (#118), windows-latest (#127) | `timed out waiting for wa online on both relays` (ext_relay_test.go:158) |
| `e2e TestOldWorkerPortLinks/direct` | macos-latest (#115), ubuntu-latest (#127) | `timed out waiting for ow -> nw (direct): the path in link ls` (compat_old_test.go:775); the relayed links of the same test were up |
| `.github/scripts/msi-install-test.ps1` "still running after the upgrade" | windows-latest (#127) | a new process got the pid of one the upgrade ended; fixed in #128 (processes compared by id and start time) |

## Seen on Windows workers under load (cpc, several jobs at once)

`e2e TestE2E/kill_pwsh_ping_tree` (timed out waiting for ping and pwsh to
die), `e2e TestUIWorkerLocal` ("another longarmd is already running" at
`longarmd leave`), `e2e TestHubRestartMeasured`, `internal/workspace`
(timeout), `web` vitest "Failed to start forks worker". The git-config
failures are in programs.md ("e2e tests depend on the machine's global git
config").
