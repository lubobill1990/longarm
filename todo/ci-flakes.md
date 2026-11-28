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
| `e2e TestUIPanelAPI` | macos-latest (main after #142) | `auto submit 429 … wa: free 1.8 GB - reserved 0.0 GB < 2.0 GB needed`: the runner's free memory under the job's 2 GB gate; the test could ask for less |
| `e2e TestUITraySmoke` | windows-latest (#142) | the daemon answered `0 workers, source none` before its first pool view; fixed in #142 (the smoke check waits for it) |
| `e2e TestPortLinks/hub_restart_and_down` | windows-latest, ubuntu-latest (#174) | `the 1m link closed before its TTL`: the test's deadline was taken before `link create`, the grant expires on a whole second from the request; fixed in #174 with a few seconds' slack (`job_bound` then failed only because the hub was left down) |
| `e2e TestLocalAPI` | windows-latest (#174) | `security info of \\.\pipe\longarm-longarmd-…: All pipe instances are busy` (localapi_test.go:27): the test reads the pipe's ACL while the daemon serves a client |
| `.github/scripts/msi-install-test.ps1` "still running after the upgrade" | windows-latest (#127) | a new process got the pid of one the upgrade ended; fixed in #128 (processes compared by id and start time) |
| `.github/scripts/msi-install-test.ps1` "service X ran before the upgrade and does not run again as a new process" | windows-latest, "service worker as a user" (#192: LongarmDesktop; #195: LongarmUpdate) | within 90 s no new pid for the service after the upgrade; both changes were elsewhere. Look at when the restart step starts the services and whether one is still stopping |
| `e2e TestHubRestartMeasured` | windows-latest (#193) | `workers back 12.8s after the hub answered (bound 12s)` |
| `e2e TestAgentAuth` | windows-latest (#197) | `a forwarded credential is on disk in …\agent-home\s-agent-…\token-cache.json` (agentauth_test.go:264, after `admin gc --keep 0s`): a session's agent home outlived gc - likely the session hadn't ended when gc ran |

## Seen on Windows workers under load (cpc, several jobs at once)

`e2e TestE2E/kill_pwsh_ping_tree` (timed out waiting for ping and pwsh to
die), `e2e TestUIWorkerLocal` ("another longarmd is already running" at
`longarmd leave`), `e2e TestHubRestartMeasured`, `internal/workspace`
(timeout), `web` vitest "Failed to start forks worker". The git-config
failures are in programs.md ("e2e tests depend on the machine's global git
config").
