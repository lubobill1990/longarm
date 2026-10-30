# Status and diagnostics

| command | answers |
|---|---|
| `longarm ls [-o wide] [--json] [--direct]` | workers you may use: STATE, OS, VERSION (`↑` = update available), CPU%, MEM, JOBS, LABELS, ADMIN, OWNER. Wide adds agent sessions, free disk, uptime, top processes, and the transport path. Offline rows say when the worker was last seen |
| `longarm worker ls [--json]` | your workers, shared ones, fleet; owner, shares |
| `longarm info <w>` | worker version, features (`Node.Info`), the identity it sees for you |
| `longarm whoami [--json]` | this device: certificate name, role, user, teams, quota, usage, can-share |
| `longarm version --json` | client version + `features` |
| `longarm doctor [--json]` | sign-in, certificate validity, hub reachability/version, client daemon, Windows firewall rule, each own worker's online state and transport legs |
| `longarmd doctor [--json]` (on the worker, e.g. `longarm exec <w> -- longarmd doctor`) | enrollment, longarmd running, hub link, run mode, logs, recent ERROR lines |
| `longarm diag [--upload --yes] [--note ...]` | zip of logs, config (secrets removed), connectivity probe. `--upload` prints a report id for bug reports. Worker side: `longarm exec <w> -- longarmd diag --upload --yes` |

`doctor --json` is `{"schema":1,"ok":bool,"checks":[{"id","status":"ok|warn|fail|skip|info","detail","fix","data"}]}`. Check ids: `signed-in`, `certificate`, `hub`, `daemon`, `firewall`, `workers`, `worker:<name>`. `fix` is for humans: show it, don't run it.

## Common failures

| symptom | fix |
|---|---|
| `your sign-in expired` (77) | `longarm login` |
| `worker X is offline (the hub last saw it ...)` (255) | Windows user-session worker: its owner must log on, or `longarmd session-mode start` / `longarmd service start` on it. `--force-dial` if the hub is wrong |
| job stays `queued` | no worker passes the memory/CPU gate or `--require`. Check `longarm ls -o wide`; use `--no-queue` to fail fast |
| `does not support it; update longarmd` (2) | `longarm worker update <w> --wait` (owner only) |
| connection stays relayed / slow | `longarm doctor` firewall line; Windows: `longarm firewall allow` (elevated) |
| clone on worker fails 401/403 | `--git-auth forward`, or `--repo .` to ship from here |
| hub unreachable | most commands fall back to talking to workers directly. `--no-hub` forces that |

`--transport race|tcp|tailscale|longnet` (or `LONGARM_TRANSPORT`) pins a transport leg when debugging connectivity.
