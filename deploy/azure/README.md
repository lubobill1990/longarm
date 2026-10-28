# longarm-hub on Azure Container Instances

[![Deploy to Azure](https://aka.ms/deploytoazurebutton)](https://portal.azure.com/#create/Microsoft.Template/uri/https%3A%2F%2Fraw.githubusercontent.com%2Flubobill1990%2Flongarm%2Fmain%2Fdeploy%2Fazure%2Fazuredeploy.json)

One command (or the button) gives you a hub with a public name
`<name>.<region>.azurecontainer.io`, durable state and an HTTPS panel, for
roughly **44 USD/month** (small size, with the panel).

```sh
az login
git clone https://github.com/lubobill1990/longarm && cd longarm/deploy/azure
./deploy.sh --name myhub --location eastasia          # PowerShell: ./deploy.ps1 -Name myhub -Location eastasia
```

The script creates the resource group `myhub-rg`, registers the resource
providers a new subscription lacks (`Microsoft.ContainerInstance`,
`Microsoft.Storage`, plus `Microsoft.OperationalInsights` with
`--log-analytics` and `Microsoft.ContainerRegistry` with `--build-in-acr`;
this needs the right to register providers, e.g. Contributor on the
subscription), deploys [`main.bicep`](main.bicep) (compiled:
[`azuredeploy.json`](azuredeploy.json)), waits until the hub answers, and
prints everything you need next (a first deployment takes about 4 minutes):

```text
longarm-hub is up.
  hub address     myhub.eastasia.azurecontainer.io:7443
  CA fingerprint  3f9c...e1
  panel           https://myhub.eastasia.azurecontainer.io
  ...
1. Your laptop (admin): install longarm (https://github.com/lubobill1990/longarm/releases), then
     longarm join myhub.eastasia.azurecontainer.io:7443 K7P-Q2M --ca-fingerprint 3f9c...e1
2. Each worker: ...
     longarmd join myhub.eastasia.azurecontainer.io:7443 <worker-code> --ca-fingerprint 3f9c...e1 --name worker-1
3. Your agents:
     claude mcp add longarm -- longarm mcp
```

With the button: the admin join code is only in `bootstrap.env` on the
`state` file share (Portal: storage account `lahub...` → File shares → state →
bootstrap.env; or
`az storage file download --account-name <account> -s state -p bootstrap.env --dest bootstrap.env`).
It is never written to the container log, which more people can read (the
Reader role) than the share (the storage key). The log has the CA fingerprint
(line `LONGARM_BOOTSTRAP ...`). `./deploy.sh --name <name> --resource-group <rg>` (a re-run: it
restarts the hub) prints everything too.

## Options

| deploy.sh | deploy.ps1 | |
|---|---|---|
| `--name NAME` | `-Name` | DNS label and resource prefix (3-40: `a-z0-9-`) |
| `--location REGION` | `-Location` | needed on the first run (`az account list-locations -o table`) |
| `--size small\|medium` | `-Size` | small = 1 vCPU / 1.5 GB (default), medium = 2 vCPU / 3 GB |
| `--no-panel` | `-NoPanel` | no Caddy sidecar, no ports 80/443 (no panel, no install scripts) |
| `--resource-group RG` | `-ResourceGroup` | default `NAME-rg` |
| `--image IMAGE` | `-Image` | default `ghcr.io/lubobill1990/longarm-hub:latest` |
| `--registry-user USER` | `-RegistryUser` | private image; password in `LONGARM_REGISTRY_PASSWORD`; the login goes to the image's registry (ghcr.io, `<acr>.azurecr.io`, ...) |
| `--build-in-acr` | `-BuildInAcr` | build the image from this checkout in an Azure Container Registry ([below](#building-the-image-in-azure-no-docker)) |
| `--stun-servers LIST` | `-StunServers` | external STUN servers the hub advertises (default Cloudflare + Google, `none` = only the hub's own; [Direct connections](#direct-connections-longnet-on-aci)) |
| `--tailscale-secret-file F` | `-TailscaleSecretFile` | Tailscale OAuth secret ([tailnet] provider, [deploy/hub](../hub/README.md#tailnet-provider-tailscale-default-or-headscale)) |
| `--headscale` / `--no-headscale` | `-Headscale` / `-NoHeadscale` | a Headscale sidecar as the hub's tailnet provider ([below](#headscale-a-self-hosted-tailnet)); needs the panel; switching it needs `--recreate`; a re-run keeps it |
| `--headscale-public-derp on\|off` | `-HeadscalePublicDerp on\|off` | also hand nodes Tailscale's public DERP relays (default off: relayed traffic stays on the hub); a re-run keeps it |
| `--acme-email E` | `-AcmeEmail` | Let's Encrypt contact |
| `--entra-tenant ID --entra-client ID` | `-EntraTenant -EntraClient` | Sign in with Microsoft Entra ID (`longarm login`, panel); a re-run keeps them ([usage](../../docs/usage.md)) |
| `--entra-admins LIST` | `-EntraAdmins` | Entra admins: UPNs, `*@domain` or object IDs, comma-separated |
| `--entra-web spa\|confidential\|off` | `-EntraWeb` | panel sign-in mode (default spa) |
| `--gate-free-gb N` | `-GateFreeGb` | scheduler memory gate: free GB a worker needs for scheduled jobs (default 20); lower it for small workers (containers, laptops); a re-run keeps it |
| `--release-public-key KEY` | `-ReleasePublicKey` | the minisign public key line (`RW…`) releases are signed with, e.g. `"$(tail -1 longarm-release.pub)"` ([CI/CD 2.1](../../docs/ci-cd.md#21-release-key-and-update-public-key)): the hub refuses uploaded releases whose signature doesn't verify, so its install scripts only hand out signed files; a re-run keeps it |
| `--entra-apps LIST` | `-EntraApps` | app-only sign-in (CI): `<app id>=<user>`, comma-separated |
| `--log-analytics` | `-LogAnalytics` | container logs to a Log Analytics workspace |
| `--storage-auth key\|identity` | `-StorageAuth` | `identity`: no storage account keys, panel served by the hub ([below](#without-storage-account-keys)); a re-run keeps the mode (switching needs a new deployment) |
| `--acr NAME` | `-Acr` | with `identity`: pull the image from this Azure Container Registry (same resource group) |
| `--telemetry` | `-Telemetry` | Application Insights + Log Analytics for the fleet's scrubbed telemetry ([below](#telemetry-and-diagnostic-bundles)) |
| `--diagnostics` | `-Diagnostics` | with `identity`: a blob container for uploaded diagnostic bundles, deleted after 30 days |
| `--panel` | `-Panel` | turn the panel back on (with `--recreate`) |
| `--recreate` | `-Recreate` | delete the container group first (to change size or panel; data stays) |
| `--admin-code` | `-AdminCode` | print a new one-time admin join code |
| `--teardown [--yes]` | `-Teardown [-Yes]` | delete the resource group (only one the script created) |
| `--timeout S` | `-TimeoutSeconds` | how long to wait for the deployment and the hub (default 1200) |
| `--stuck-timeout S` | `-StuckTimeoutSeconds` | give up on a group stuck in `Creating` without any event (default 600) |

**A group stuck in Creating.** Once, a deployment sat in `Creating` with
both containers `Waiting` and no events at all for over 30 minutes (not
reproduced: the same deployment with `--recreate` was up in 3.5 minutes, and
Caddy alone runs fine). The scripts deploy with `--no-wait` and watch the
group: after `--stuck-timeout` seconds in that state they stop with what to
do - `./deploy.sh --name <name> --recreate` (storage and data are kept), or
`--no-panel --recreate` to rule out the sidecar. A failed deployment prints
the failed operations (e.g. `InaccessibleImage`).

Secrets (registry password, Tailscale secret) go to the deployment through a
temporary parameters file, never on a command line.

## Architecture

```text
                    myhub.<region>.azurecontainer.io (public IP, DNS label)
  ┌──────────────────────── ACI container group (restart: Always) ─────────────────────────┐
  │  hub  ghcr.io/.../longarm-hub (non-root)            caddy (optional)                   │
  │   PID 1 litestream replicate -exec "longarm-hub serve"   :443/:80 Let's Encrypt         │
  │     :7443/tcp gRPC API + relay TLS fallback (mTLS)       reverse_proxy 127.0.0.1:7480   │
  │     :7445/udp longnet relay (QUIC) + STUN                                                │
  │     :7446/udp second STUN port                                                           │
  │     127.0.0.1:7480 panel / API / install scripts / healthz                               │
  │   /var/lib/longarm-hub/hub.db  (container disk)    /data  (state share)  /data (caddy)  │
  └──────────────┬──────────────────────────────────────────┬──────────────────┬──────────┘
                 │ Litestream (WAL, every second)           │ SMB              │ SMB
        Blob container "litestream"              File share "state"     File share "caddy"
                         └──────────── storage account lahub<hash> ─────────────┘
```

- **No TLS-terminating proxy in front of 7443**: clients and workers do mTLS
  with the hub end to end; the CA fingerprint they pin is the hub's own CA.
  Azure Container Apps can't do this (no UDP ingress), ACI can.
- **The image** (`deploy/hub/Dockerfile`): static `longarm-hub` + Litestream
  0.5.17 (checksum-verified) on distroless, user `nonroot`. Its command
  `longarm-hub container` restores the database if the replica has one,
  runs `longarm-hub bootstrap`, then becomes `litestream replicate -exec
  "longarm-hub serve"`.
- **First start** (`longarm-hub bootstrap`, idempotent, every start): writes a
  `hub.toml` template to the share, creates the CA, and as long as no admin
  has joined keeps one valid one-time admin join code (24 h; a new one after
  it expired). Values go to `bootstrap.env` on the share; one
  `LONGARM_BOOTSTRAP` log line repeats them without the code. A CA is only
  created for a hub that never issued anything: a database with nodes next to
  an empty share stops the start (restore the CA instead).
- **Configuration** comes from the environment (set by the template), which
  wins over `hub.toml`: `LONGARM_HUB_ADVERTISE` (the FQDN: `public_addr` +
  certificate name), `LONGARM_HUB_PUBLIC_URL`, `LONGARM_HUB_HTTP`,
  `LONGARM_HUB_DB`, `LONGARM_HUB_DATA_DIR`, `LONGARM_HUB_CONFIG`. Your own
  settings (auto_approve, certificate TTLs, scheduler, tailnet) go into
  `hub.toml` on the share (the release public key: `--release-public-key`);
  restart the container group afterwards
  (`az container restart -g <rg> -n <name>`). Sign-in with
  Microsoft Entra ID: the template parameters `entraTenantId`,
  `entraClientId`, `entraAdmins` and `entraWeb` set the `LONGARM_HUB_ENTRA_*`
  variables (docs/usage.md "Sign in with Microsoft Entra ID"; `entraWeb =
  confidential` with `storageAuth = identity` redeems codes with the managed
  identity: `identityPrincipalId` is the federated credential's subject).
- **Health**: `longarm-hub healthcheck` (GET `/healthz` on 127.0.0.1:7480) is
  the liveness and readiness probe.
- **Stop** (restart, upgrade, ACI maintenance): SIGTERM reaches Litestream
  (PID 1), which stops the hub (it closes its listeners and the database),
  then ships the last WAL frames (up to 20 s) and exits.

## Where the data lives, and why

| what | where | why |
|---|---|---|
| `hub.db` (SQLite, WAL): nodes, join codes, users, jobs index, audit, releases index | container disk, streamed by **Litestream** to the blob container `litestream` | SQLite must not run on Azure Files: SMB byte-range locks and WAL's shared memory are not reliable there (corruption risk). Local disk + Litestream is the supported pattern. |
| CA (`ca/`), hub/relay certificates (`tls/`), `hub.toml`, release store (`releases/`), `bootstrap.env` | Azure Files share `state`, mounted at `/data` | small files written rarely (release files once per release); readable and editable with any SMB/Storage tool; no app code needed to sync them. Blob would need a sync loop in the image for the same result. |
| `--storage-auth identity`: release files | blob container `releases` (`<version>/<file>` + `.minisig`), written by the hub with its managed identity; `/data/releases` is a cache | there is no file share; see [below](#without-storage-account-keys). |
| Caddy's ACME account and certificates | Azure Files share `caddy` | a container restart must not request a new Let's Encrypt certificate (rate limits). |

Credentials: ACI mounts Azure Files only with the storage account key, so the
key is in the container group's volume definition (never readable back through
the ACI API). Litestream - and the hub, its child process - get only a **SAS
for the `litestream` blob container** (10 years; every `deploy.sh` run renews
it), not the account key that also opens the share with the CA. Litestream
0.5 could use a managed identity, but the share needs the key anyway, and a
role assignment would require Owner / User Access Administrator rights for a
one-click deployment. Anyone who can read the storage account keys
(Contributor on the resource group) can read the CA key: treat the resource
group like the hub's root of trust.

**Durability (RPO/RTO).** Litestream ships WAL changes every second; a
crash or host loss loses at most about a second of hub writes (a join, a
job record). A normal stop loses nothing. Restore on the next start takes
seconds. Litestream keeps a daily snapshot and 24 h of history; blob soft
delete keeps deleted blobs 7 days, share soft delete 7 days. Storage is
locally redundant by default (`storageSku` parameter: ZRS/GRS). The hub is
never on the data path: while it restarts, workers keep running jobs and
clients keep talking to workers they already know.

## Without storage account keys

Some subscriptions deny storage accounts with shared key access (Azure
Policy "Storage accounts should prevent shared key access"). There the
template can't work as described above: ACI mounts Azure Files only with the
account key, and the Litestream SAS is signed with it. `--storage-auth
identity` (template parameter `storageAuth=identity`) deploys without keys:

```sh
./deploy.sh --name myhub --location eastasia --storage-auth identity   # PowerShell: -StorageAuth identity
```

- The storage account has `allowSharedKeyAccess: false` and only blob
  containers: `litestream`, `releases` (and `diagnostics` with
  `--diagnostics`, see [below](#telemetry-and-diagnostic-bundles)); no file
  shares. With
  `storageAuth=key` the hub has no managed identity and doesn't collect
  diagnostic bundles (users attach the zip by hand).
- A user-assigned managed identity `<name>-id` gets **Storage Blob Data
  Contributor** on each of these containers (not the account); Litestream finds it through
  `AZURE_CLIENT_ID` (Azure SDK DefaultAzureCredential). The role
  assignment needs Owner or User Access Administrator on the resource group.
- The data dir `/data` is the container's own disk: it is gone after a
  restart. What matters in it is the CA, so the hub keeps a copy of
  `ca.crt` + `ca.key` in the database (`LONGARM_HUB_CA_IN_DB=1`, hub.toml
  `ca_in_db`) and bootstrap writes it back on every start. Everything else
  is recreated (hub/relay certificates from the CA, hub.toml from the
  environment, bootstrap.env). The database replica now holds the fleet's
  root key: whoever can read the `litestream` container can read the CA.
- Release files (`longarm admin release publish`) go to the blob container
  `releases` (`LONGARM_HUB_RELEASES_BLOB_URL`, hub.toml `[releases]
  blob_url`) as well as `/data/releases`; after a restart the hub downloads
  a file from there on its first request (`/releases/<v>/<file>`, worker
  self-update), checks it against the sha256 in the database and keeps it in
  `/data/releases`. At start it lists the container: files the database
  lists that are nowhere are logged (`RELEASE FILES MISSING`), and
  `/install.sh` / `/install.ps1` hand out the newest stable version that is
  complete (or "no release yet"). Publishing the same files again restores
  such a version - for a hub deployed before the container existed: re-run
  the release's publish job (or publish the next version). Right after the
  first deployment with the container the role assignment may take a few
  minutes to apply; a publish refused with 403 then works on a retry.
- No Caddy: the hub serves the panel on 443 itself (`LONGARM_HUB_HTTPS`,
  hub.toml `https`) with a Let's Encrypt certificate (TLS-ALPN-01), and
  redirects 80 to it. Certificate and ACME account are in the database
  too, so restarts and upgrades don't ask Let's Encrypt again (its limit is
  5 certificates per week for one name). The image's `longarm-hub` has the
  file capability `cap_net_bind_service` for that (it runs as nonroot).
- Not kept across restarts, by design: edits to hub.toml (it is written
  from the template; the settings are environment variables - redeploy),
  bootstrap.env (rewritten on every start from the database: while no
  admin has joined, a lost file means a new one-time admin code; the old one
  stays valid until it expires), the hub/relay server certificates (issued
  again from the CA; nodes pin the CA), staged uploads (temporary), and
  crash reports of the hub process waiting to be sent to telemetry (the
  panic is in the container log too).
- The admin join code is not in the log; get it with `--admin-code`.
- `--acr NAME` pulls the image from an Azure Container Registry in the
  resource group with the same identity (AcrPull), for registries without
  admin users.

## Backup and restore

- Database: the replica *is* the backup. Point-in-time copy:
  `litestream restore -o hub.db -timestamp 2026-10-25T10:00:00Z abs://<account>@litestream/hub.db`
  (with `LITESTREAM_AZURE_ACCOUNT_KEY` set).
- CA and state: `az storage file download-batch -s state -d ./hub-state --account-name <account>`
  (keep `ca/ca.key` offline and safe; it is the fleet's root of trust).
- Restore into a fresh deployment: deploy with the same name into a new
  resource group, stop it (`az container stop`), upload the `state` share
  content and copy the `litestream` container (`azcopy copy`), start it. The
  start restores the database from the replica.

## Upgrades

```sh
./deploy.sh --name myhub --image ghcr.io/lubobill1990/longarm-hub:1.4.0
```

Re-running the script is an upgrade: the template is applied again with the
running hub's settings (image, size, panel, Log Analytics, storage auth,
telemetry, diagnostics, Entra sign-in, STUN servers) unless an option
changes them - same storage, same data - and the hub restarts (the script
waits for the new start). With a moving tag (`:latest`, `:edge`) and no new
image it restarts the group so the newest image is pulled. Secrets can't be
read back from ACI: a hub deployed with `--registry-user` or
`--tailscale-secret-file` needs them again on every run (the script refuses
otherwise), and `--acme-email` too if you want to keep it. Changing `--size`
or the panel needs `--recreate` (ACI can't change a running group's resources
or ports); data is kept either way. The hub is down for about a minute;
workers and clients reconnect by themselves.

## Telemetry and diagnostic bundles

```sh
./deploy.sh --name myhub --storage-auth identity --telemetry --diagnostics   # PowerShell: -StorageAuth identity -Telemetry -Diagnostics
```

- `--telemetry` creates a Log Analytics workspace `<name>-telemetry` and a
  workspace-based Application Insights `<name>-ai` (retention 90 days,
  template parameter `telemetryRetentionDays`). The hub gets
  `LONGARM_HUB_APPINSIGHTS_CONNECTION_STRING`; workers and clients send their
  scrubbed telemetry to the hub, which forwards it
  ([docs/telemetry.md](../../docs/telemetry.md)). With `identity` the
  resource accepts only Entra ID ingestion (local auth off) and the hub's
  identity has **Monitoring Metrics Publisher** on it - the connection string
  alone lets nobody write.
- `--diagnostics` creates the blob container `diagnostics` (the hub's
  identity: Storage Blob Data Contributor on it only) and a lifecycle rule
  deleting bundles after 30 days (`diagnosticsRetentionDays`). Users upload
  with `longarm diag --upload` / `longarmd diag --upload`; maintainers read
  them with `longarm admin diag ls|get`
  ([docs/troubleshooting.md](../../docs/troubleshooting.md)).
- Cost: Application Insights / Log Analytics bill per GB ingested (about
  2-3 USD/GB; a small fleet sends a few MB a day); bundles are a few MB each.

## Costs

Linux ACI is billed per vCPU-second and GB-second (pay-as-you-go, about
0.0405 USD per vCPU-hour and 0.0045 USD per GB-hour in US regions; some
regions are up to ~20 % more; check the
[pricing page](https://azure.microsoft.com/pricing/details/container-instances/)):

| size | hub | + panel (0.25 vCPU / 0.3 GB) | monthly (approx.) |
|---|---|---|---|
| small | 1 vCPU / 1.5 GB | yes | ~44 USD |
| small, `--storage-auth identity` | 1 vCPU / 1.5 GB | yes (in the hub) | ~36 USD |
| small | 1 vCPU / 1.5 GB | no | ~36 USD |
| small, identity, `--headscale` (+ 0.25 vCPU / 0.3 GB) | 1 vCPU / 1.5 GB | yes (in the hub) | ~44 USD |
| medium | 2 vCPU / 3 GB | yes | ~78 USD |

Plus storage (a few GB: under 1 USD), Log Analytics if enabled (per GB
ingested), and **egress** beyond 100 GB/month (about 0.08-0.12 USD/GB):
traffic relayed by the longnet relay leaves Azure and counts. Direct
(peer-to-peer) connections between clients and workers don't touch the hub.

## Limits

- **Single instance.** One container group; no high availability. A hub
  outage only degrades the fleet (new joins, scheduling, the panel); running
  jobs and existing client-worker sessions are unaffected.
- **The public IP can change** on restarts and upgrades. Everything uses the
  DNS name (`<name>.<region>.azurecontainer.io`): the hub certificate carries
  it, the deploy script prints it, `longarm join` / `longarmd join` store it.
  Don't put the IP anywhere.
- **The DNS label is first come, first served** in the region: after a
  teardown someone else could take it. Nodes would refuse such a hub (its
  certificate is not from your CA), but they couldn't reach yours either.
- **Docker Hub pull limits** may apply to the Caddy image; set the
  `caddyImage` parameter to a mirror (e.g. your ACR) if pulls fail.
- **The image must be pullable**: make the `longarm-hub` package public on
  GitHub (package settings) or deploy with `--registry-user` and a token with
  `read:packages`.
- **Install scripts** (`/install.sh`, `/install.ps1`) need the panel and a
  release in the hub's release store (`longarm admin release publish`);
  until then install `longarm` / `longarmd` from the GitHub releases.
- **`az container exec` takes no arguments** (one program, interactive TTY).
  The image has argument-free names for what you need there:
  `/usr/local/bin/longarm-hub-admin-code` (a 24 h admin join code; what
  `deploy.sh --admin-code` runs), `/usr/local/bin/longarm-hub-user-add` (panel
  account: asks for name and password, prints the TOTP secret) and
  `/usr/local/bin/longarm-hub-users`:
  `az container exec -g <rg> -n <name> --container-name hub --exec-command /usr/local/bin/longarm-hub-user-add`
  (from Git Bash on Windows prefix `MSYS_NO_PATHCONV=1`).
- **Azure public cloud only**: the template builds the name
  `<name>.<region>.azurecontainer.io`, the public cloud's ACI DNS zone.
- **UDP 7445 and 7446, not 7443**: ACI refuses one port number for TCP and
  UDP (`DuplicateContainerPorts` / `DuplicateIpAddressPorts`), so the relay's UDP port is
  `relayUdpPort` (default 7445, at least 7444; STUN's second port is the next
  one) and its TLS fallback stays on 7443/tcp. Workers listen on
  `longnet://<fqdn>:7445?tcp=<fqdn>:7443&name=<worker>` (the scripts print
  it). CI checks the template against ACI's rules for every parameter
  combination ([test/acirules](test/acirules/main.go)): ports per container,
  memory in 0.1 GB steps, CPU, group limits.
- **ACI rewrites the source address of inbound UDP** (see below): the hub's
  own STUN answers are useless for NAT detection there.

## Building the image in Azure (no docker)

The default image is `ghcr.io/lubobill1990/longarm-hub` (public once the
package is made public). To run your own build - a branch, a fork, or while
the GHCR package is private - without docker:

```sh
./deploy.sh --name myhub --location eastasia --build-in-acr     # PowerShell: -BuildInAcr
```

The script creates an Azure Container Registry (Basic, about 5 USD/month)
in the hub's resource group on the first use, runs `az acr build --target
acr` on this checkout (ACR Tasks, a few minutes), and deploys
`<registry>.azurecr.io/longarm-hub:build-<UTC time>` with the registry's
admin login (`--storage-auth identity`: no admin login, the hub's managed
identity pulls it, as with `--acr`). Re-runs without `--build-in-acr` keep that image and find the
login by themselves; `--build-in-acr` again builds and deploys a new one.
By hand:

```sh
az acr build -r <registry> -t longarm-hub:mytag --target acr -f deploy/hub/Dockerfile .
```

`--target acr` matters: ACR Tasks scans every `FROM` line up to the target
for base images and only understands `FROM <image> [AS <name>]`; the
default (last) stage uses `FROM --platform=$BUILDPLATFORM` for BuildKit's
cross-compiling multi-arch build (CI), which ACR Tasks rejects with "failed
to scan dependencies". The `acr` stage builds natively (`--platform
linux/arm64` gives an arm64 image).

## Headscale: a self-hosted tailnet

```sh
./deploy.sh --name myhub --headscale --recreate     # an existing hub (data kept); new ones: no --recreate
```

The tailscale leg (DESIGN §4.2) on a control plane you own, with no VM and
no manual Headscale steps: a **Headscale v0.29.5 sidecar** (image
`headscale/headscale:v0.29.5-debug`, pinned by digest: the template
parameter `headscaleImage`) in the hub's container group becomes the hub's
`[tailnet] provider`.

```text
  ┌──────────────────────────── ACI container group ─────────────────────────────┐
  │  hub (longarm-hub)                                 headscale (sidecar)        │
  │   :443 https (Let's Encrypt, the hub's)  ──proxy──▶ 127.0.0.1:8080             │
  │     /ts2021 /key /derp /derp/probe /derp/latency-check                         │
  │     /bootstrap-dns /machine/ping-response    (nothing else of Headscale)      │
  │   :80 http: /ts2021 too (Noise is authenticated), else the https redirect      │
  │   Litestream: hub.db ─▶ "litestream"   /headscale/db.sqlite ─▶ "headscale"     │
  │                 /headscale (emptyDir: config, policy, DERP map, keys, socket) │
  └──────────────────────────────────────────────────────────────────────────────┘
```

- **Ports**: none new. Headscale listens on the group's localhost only; the
  hub serves exactly the endpoints nodes use on its own https port, with its
  certificate, at `https://<fqdn>` (the nodes' control URL). Its REST and
  gRPC API, metrics, swagger and the interactive `/register` / `/oidc`
  pages are not reachable from outside (keys are pre-auth keys the hub
  mints). The proxy passes the HTTP Upgrades of `/ts2021` (Noise) and
  `/derp` through, never DERP's "fast start" (the TLS is the hub's).
- **DERP and STUN**: Headscale's embedded DERP relay (region 900,
  `longarm-hub`) runs over that https; Tailscale's public DERP map is off
  (`--headscale-public-derp on` adds it), so relayed traffic stays on the
  hub. STUN does **not** run on the hub: ACI allows **5 ports per IP** (the hub
  with its panel uses all five: 7443/tcp, 7445-7446/udp, 80, 443) and
  source-NATs inbound UDP (STUN answers from inside the group would show
  every node a private address, see below). The region carries the external
  STUN servers the hub advertises for longnet (`--stun-servers`, default
  Cloudflare and Google) as STUN-only nodes instead; they only answer
  binding requests. With `--stun-servers none` nodes have no STUN and go
  through DERP unless they are on one network.
- **Persistence**: Headscale's SQLite database is on the shared `emptyDir`
  and Litestream (the hub's, the same storage account, the managed identity
  with Storage Blob Data Contributor on the new blob container `headscale`,
  or a SAS for it with storage keys) replicates it. The hub restores it
  before it lets Headscale start (`longarm-hub container`); a failed restore
  keeps Headscale stopped, not the hub. Headscale's **Noise and DERP
  private keys** live in the hub's database (replicated, like the CA in
  identity mode): every start writes them to the shared directory, so the
  nodes never have to register again.
- **API key**: once Headscale runs, the hub creates its API key over
  Headscale's unix socket in the shared directory (the directory belongs to
  the two containers only) and keeps it in its database; it is checked
  every 10 minutes, renewed 30 days before it expires (1 year) and replaced
  if Headscale no longer knows it (the old one is expired). It never
  appears in the environment, the portal or a log.
- **ACL policy** (written by the hub): `tag:longarm-client` → `tag:longarm-worker:7701`,
  and `tag:longarm-worker` → `tag:longarm-worker:7701` (port links); nothing else.
- **Existing nodes** need nothing: workers get the hub's new tailnet in their
  next hub session, clients on their next hub contact, and fetch a key
  (docs/usage.md "Switching providers"). While Headscale starts, the hub
  answers their key requests "unavailable" without charging their rate limit.
- **Health**: `/healthz` has `"headscale": "ok" | "starting" | "down"`,
  `longarm admin tailnet status` the detail; deploy.sh prints it at the end.
  Logs: `az container logs -g <rg> -n <name> --container-name headscale`.
  Headscale commands: `az container exec -g <rg> -n <name> --container-name
  headscale --exec-command "/ko-app/headscale --config /headscale/config.yaml nodes list"`
  (stale worker devices: `nodes delete`).
- **Cost**: the sidecar requests 0.25 vCPU / 0.3 GB, about 9 USD/month.
- Key mode (storage account keys, Caddy in front): the same; Caddy forwards
  everything to the hub, which proxies Headscale's endpoints.

## Direct connections (longnet) on ACI

ACI rewrites the **source address of inbound UDP**: the hub sees every node
as coming from an internal address (`10.92.0.x` on the real run), so the
reflexive addresses its STUN ports report are useless - every node looked
like a hard (symmetric) NAT and longnet never tried hole punching (relay
only, about 105 ms between a laptop and a worker that are 30 ms apart).

What happens now:

- Nodes ignore STUN answers with a private, CGNAT, link-local or loopback
  mapped address from a STUN server on a public address (logged once); NAT
  types are derived from public observations only, and fewer than two
  give "unknown" (punching is tried), not "hard".
- The template sets `LONGARM_HUB_RELAY_STUN_ADVERTISE` so the hub tells
  nodes to use **public STUN servers** (default
  `stun.cloudflare.com:3478,stun.l.google.com:19302`, the
  `stunServers` parameter / `--stun-servers`) next to its own ports.
- The hub notices that its own STUN answers are SNATed (nodes it sees from a
  private address report public addresses from other STUN servers; log line
  "look source-NATed", `"stun_snat": true` in the relay's `/sessions`) and
  then stops advertising its own STUN ports and stops passing the addresses
  it observed to peers. Sessions work through the relay either way.

So **direct connections on ACI need the external STUN servers**. Nodes
resolve the names and query them from their own UDP socket: the servers must
be reachable over UDP from the nodes, not from Azure. The template's value is
`self,<stunServers>` (`self` = the hub's own two ports, same syntax as
`relay_stun_advertise` in hub.toml; the environment wins). With
`--stun-servers none` only the hub's own ports are advertised and on ACI
everything goes through the relay (it works, just slower and billed as
egress). Nodes can also bring their own: `LONGARM_NET_STUN=host:port,...`
or `?stun=` on the longnet URL.

## Files

| file | |
|---|---|
| [main.bicep](main.bicep) | the template (`az deployment group create --template-file main.bicep -p name=myhub`) |
| [azuredeploy.json](azuredeploy.json) | `bicep build main.bicep --outfile azuredeploy.json` (CI checks it is current) |
| [deploy.sh](deploy.sh) / [deploy.ps1](deploy.ps1) | the one-command deploy / upgrade / teardown |
| [test/container-smoke.sh](test/container-smoke.sh) | the image test in CI (docker + Azurite) |
| [test/acirules](test/acirules/main.go) | ACI's rules (ports, memory/CPU steps, limits) for every parameter combination, in CI |
| [E2E.md](E2E.md) | the manual end-to-end test on a real subscription |
