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

## Seen on Windows workers under load (cpc, several jobs at once)

`e2e TestE2E/kill_pwsh_ping_tree` (timed out waiting for ping and pwsh to
die), `e2e TestUIWorkerLocal` ("another longarmd is already running" at
`longarmd leave`), `e2e TestHubRestartMeasured`, `internal/workspace`
(timeout), `web` vitest "Failed to start forks worker". The git-config
failures are in programs.md ("e2e tests depend on the machine's global git
config").
