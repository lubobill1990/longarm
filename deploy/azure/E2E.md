# Manual end-to-end test on Azure

The CI checks everything that runs without Azure: the template compiles
and lints clean, the deploy scripts work against a fake `az` (e2e/azure_deploy_test.go),
and the image passes deploy/azure/test/container-smoke.sh (Azurite as Blob
Storage: bootstrap, admin join, restore after replacement). This procedure
covers the rest on a real subscription. It takes about 45 minutes and costs
well under 1 USD (the resource group is deleted at the end).

Record the output of every numbered step that says **record**.

## 0. Prerequisites

- Azure CLI ≥ 2.60, logged in: `az login`, `az account show` shows the
  intended subscription (Contributor on it is enough).
- Resource providers: the scripts register what a new subscription lacks
  (`Microsoft.ContainerInstance`, `Microsoft.Storage`, and
  `Microsoft.OperationalInsights` / `Microsoft.ContainerRegistry` when
  needed) and wait for them. On the first real run `Microsoft.ContainerInstance`
  was `NotRegistered`; **record** whether the script printed
  `registering the resource provider ...` and how long it took.
- The image, one of:
  - `ghcr.io/lubobill1990/longarm-hub:edge` (published by `image.yml` after a
    merge to main); make the package public once (GitHub → Packages →
    longarm-hub → Package settings → Change visibility), or pass
    `--registry-user <user>` with `LONGARM_REGISTRY_PASSWORD=<PAT with read:packages>`.
  - **built in Azure from this checkout, no docker**: `--build-in-acr` in step 1
    instead of `--image` (an ACR in the resource group, `az acr build --target
    acr`; README "Building the image in Azure"). The first real run built the
    image with `az acr build` by hand: the original Dockerfile failed with
    "failed to scan dependencies" (ACR Tasks can't parse `FROM
    --platform=$BUILDPLATFORM`), hence the `acr` stage.
  - built and pushed yourself (WSL/Linux with docker):
    ```sh
    echo "$GHCR_TOKEN" | docker login ghcr.io -u <user> --password-stdin
    docker buildx build --platform linux/amd64,linux/arm64 -f deploy/hub/Dockerfile \
      --build-arg VERSION=0.0.0-azure-test -t ghcr.io/<owner>/longarm-hub:azure-test --push .
    ```
- `longarm` and `longarmd` for the laptop and a worker: from the releases page,
  or `go build -o bin/ ./cmd/longarm ./cmd/longarmd` from this branch.
- Two machines besides Azure: the **laptop** (admin/client) and a **worker**
  (any OS). Ideally on different networks (e.g. the worker behind a home NAT
  or a phone hotspot), so the relay path is real.

Variables used below:

```sh
NAME=lae2e$RANDOM            # DNS label, must be unused in the region
LOC=eastasia                 # any region with ACI
IMG=ghcr.io/<owner>/longarm-hub:edge
RG=$NAME-rg
```

## 1. Deploy to a fresh resource group

```sh
cd deploy/azure
time ./deploy.sh --name $NAME --location $LOC --image $IMG      # PowerShell: ./deploy.ps1 -Name ... -Location ... -Image ...
# or, without a published image:
time ./deploy.sh --name $NAME --location $LOC --build-in-acr
```

First real run (eastasia): about 4 minutes from start to "longarm-hub is
up", the panel's Let's Encrypt certificate within about 2 minutes after
that. Should the group sit in `Creating` with both containers `Waiting` and
no events (seen once, for 30+ minutes), the script stops after
`--stuck-timeout` (600 s) and says to re-run with `--recreate` (that worked
in 208 s on the real run) or `--no-panel --recreate`; **record** it if it
happens.

Expect (**record** the whole output and the duration):
- `creating resource group`, `deploying`, `waiting for the hub`, then the
  summary with `hub address $NAME.$LOC.azurecontainer.io:7443`, a 64-hex CA
  fingerprint, `panel https://$NAME.$LOC.azurecontainer.io`, a cost estimate,
  and the `longarm join ... <code> --ca-fingerprint <fp>` line.

Checks:

```sh
az resource list -g $RG -o table                        # storage account lahub..., container group $NAME
az container show -g $RG -n $NAME --query "{state:instanceView.state, ip:ipAddress.ip, fqdn:ipAddress.fqdn, ports:ipAddress.ports}" -o json
#   ports: 7443 TCP, 7445 UDP, 7446 UDP, 80 TCP, 443 TCP
#   (ACI refuses 7443 for TCP and UDP: DuplicateContainerPorts - CI checks this now, test/acirules)
az container logs -g $RG -n $NAME --container-name hub | head -40
#   "no matching backups found" (first start), "created a new CA", LONGARM_BOOTSTRAP line
#   (fingerprint, no join code), "longarm-hub started"
az container show -g $RG -n $NAME --query "containers[0].environmentVariables[?name=='LONGARM_HUB_RELAY_STUN_ADVERTISE'].value" -o tsv
#   self,stun.cloudflare.com:3478,stun.l.google.com:19302
curl -fsS https://$NAME.$LOC.azurecontainer.io/healthz            # valid Let's Encrypt certificate, JSON health
curl -fsS https://$NAME.$LOC.azurecontainer.io/ca.crt | openssl x509 -outform der | sha256sum
#   = the printed CA fingerprint
ACCT=$(az storage account list -g $RG --query "[0].name" -o tsv)
KEY=$(az storage account keys list -g $RG -n $ACCT --query "[0].value" -o tsv)
az storage file list -s state --account-name $ACCT --account-key "$KEY" -o table    # bootstrap.env, ca, hub.toml, tls
az container show -g $RG -n $NAME --query "containers[0].environmentVariables[].name" -o tsv
#   LITESTREAM_AZURE_SAS_TOKEN, no LITESTREAM_AZURE_ACCOUNT_KEY
az storage blob list -c litestream --account-name $ACCT --account-key "$KEY" -o table  # hub.db/... LTX files
```

## 2. Join the admin client (laptop)

Run the printed command:

```sh
longarm join $NAME.$LOC.azurecontainer.io:7443 <code> --ca-fingerprint <fp> --name laptop
longarm admin join-code --role worker            # record: works with the admin certificate
```

Negative check: the same code again from another config dir fails
(`LONGARM_CONFIG_DIR=$(mktemp -d) longarm join ...` → error).

Panel account (interactive; password + TOTP secret):

```sh
az container exec -g $RG -n $NAME --container-name hub --exec-command /usr/local/bin/longarm-hub-user-add
./deploy.sh --name $NAME --admin-code       # a second admin code (az container exec of longarm-hub-admin-code)
```

Log in at `https://$NAME.$LOC.azurecontainer.io/` and see the laptop node.

## 3. Join a worker

On the worker, with a code from `longarm admin join-code --role worker`:

```sh
longarmd join $NAME.$LOC.azurecontainer.io:7443 <worker-code> --ca-fingerprint <fp> --name e2e-w1
longarm admin approve e2e-w1                    # on the laptop (unless auto_approve)
# run it in the foreground for the test; listen on tcp and on the hub's longnet relay:
longarmd run --listen tcp://0.0.0.0:7701 --listen "longnet://$NAME.$LOC.azurecontainer.io:7445?tcp=$NAME.$LOC.azurecontainer.io:7443&name=e2e-w1"
```

(UDP 7445 for the relay and STUN, the relay's TLS fallback on TCP 7443:
the deploy script prints this line.)

On the laptop: `longarm ls` shows `e2e-w1` online with load, and
`longarm ls --direct` the path to it (**record**).

## 4. A job over each transport

```sh
longarm --transport longnet exec e2e-w1 -- hostname                     # longnet (direct after hole punching, or relay)
LONGARM_NET_DISABLE_DIRECT=1 longarm --transport longnet exec e2e-w1 -- hostname   # forced through the hub's relay, UDP 7445
LONGARM_NET_DISABLE_DIRECT=1 LONGARM_NET_RELAY_MODE=tcp longarm --transport longnet exec e2e-w1 -- hostname   # relay TLS fallback, TCP 7443
LONGARM_NET_LOG=1 longarm --transport longnet info e2e-w1               # NAT types from the advertised STUN servers
longarm --transport tcp exec e2e-w1 -- hostname                         # only if the laptop reaches the worker's 7701 directly
longarm run --repo . -f -- git log -1 --oneline                         # a scheduled job (the hub picks the worker), output followed
longarm ls --direct                                                      # record the paths, e.g. "longnet/relay 40ms (udp ...:7443, nat easy/hard)"
```

Expect every command to print the worker's hostname / output and exit 0;
the `nat x/y` column filled in.

**ACI source-NATs inbound UDP** (first real run): the hub's STUN ports
answered `10.92.0.x` for every node, both nodes looked "hard", and longnet
never punched (relay only, about 105 ms). Now nodes ignore private STUN
answers from a public server, classify from public answers only, and get
Cloudflare's and Google's STUN servers from the hub. Expect:
- `LONGARM_NET_LOG=1` on a node: one line about ignoring the hub's private
  STUN answer (per server), a NAT type `easy` / `open` / `unknown` - not
  `hard` unless the node really is behind a symmetric NAT;
- the hub log: "look source-NATed" once the first node with a public STUN
  answer connects (it then stops advertising its own STUN ports);
- `longarm ls --direct`: `longnet/direct` between the laptop and the worker
  when at most one side is behind a hard NAT (**record** the path and RTT;
  the real run had relay only, ~105 ms).
With `--stun-servers none` (a re-run) everything stays on the relay - that
is expected on ACI. Tailscale leg: only if the hub has a tailnet provider
(`--tailscale-secret-file`); then also `longarm --transport tailscale exec e2e-w1 -- hostname`.

## 5. Restart the container group: data survives

```sh
az container restart -g $RG -n $NAME          # in-place restart (SIGTERM -> Litestream flush)
az container logs -g $RG -n $NAME --container-name hub | head -30
#   no "created a new CA"; LONGARM_BOOTSTRAP ... admin_joined=true, a new boot_id, same ca_fingerprint
longarm ls                                     # e2e-w1 back online within ~30 s, no re-join
longarm admin join-code --role worker          # admin certificate still valid
```

Then the harder case, a **new container** (container disk gone, database must
come from the replica) and possibly a new IP:

```sh
az container show -g $RG -n $NAME --query ipAddress.ip -o tsv     # record the IP
./deploy.sh --name $NAME --image $IMG --recreate                  # deletes and recreates the group, same storage
az container show -g $RG -n $NAME --query ipAddress.ip -o tsv     # may differ: clients use the DNS name
az container logs -g $RG -n $NAME --container-name hub | head -30  # litestream restore lines, no "no matching backups found"
longarm ls                                                         # e2e-w1 online again, joined state intact
az container exec -g $RG -n $NAME --container-name hub --exec-command /usr/local/bin/longarm-hub-users   # the panel user still there
```

Also a plain re-run (`./deploy.sh --name $NAME`): it keeps image, size and
panel ("deploying ... small, panel true, <image>"), restarts the hub, prints
the same fingerprint and "An admin has joined already". And
`./deploy.sh --name $NAME --size medium` must refuse without `--recreate`.

Timings of the first real run: a restart until the hub answered again with
the database restored about 36 s; `--panel --recreate` 208 s.

## 6. A per-user (multi-tenant) Linux worker

On an Ubuntu VM (24.04 on the real run), with an admin on the laptop:

```sh
longarm admin user add alice --panel; longarm admin user add bob     # laptop: join codes for the users
longarm admin join-code --role worker
sudo longarmd join $NAME.$LOC.azurecontainer.io:7443 <worker-code> --ca-fingerprint <fp> --name e2e-pu
#   prints the data dir: /var/lib/longarmd (root's default; it was /root/.longarm before,
#   which the la-* accounts can't traverse - the per-user service then refused to start)
sudo longarmd service install --isolation per-user && sudo longarmd service start
longarm admin approve e2e-pu
sudo mount -o remount,hidepid=invisible /proc                          # see below
```

alice and bob join from their own machines (or config dirs:
`LONGARM_CONFIG_DIR=...`) with their codes. Checks (all passed on the real
run unless noted):

| check | expect |
|---|---|
| `longarm run e2e-pu -- id` as alice | `uid=... (la-alice)`, no supplementary groups |
| bob: `longarm jobs`, `logs`, `kill` on alice's job | not listed / permission denied |
| files: `ls -l /var/lib/longarmd/users` | `la-alice` / `la-bob` dirs, mode 0700/0710, owned by the account |
| bob's job: `cat /proc/<alice's job pid>/environ` | permission denied |
| bob: Files API on alice's home (`longarm sync` / `longarm mcp` file tools on her paths) | refused |
| bob: `longarm forward e2e-pu <alice's dev server port>` | refused (port owner check) |
| bob's job: `ps -ef` | without `hidepid=invisible`: alice's command lines are visible (documented gap, longarmd warns at start); with it: only bob's processes |
| bob's job: `curl http://127.0.0.1:<alice's port>` | real run (before this change): **connected** (documented gap). Now: connection refused - longarmd's nftables table `inet longarm` blocks loopback connections between `la-*` accounts (`[isolation] loopback = "isolate"`, docs/security.md "Loopback between users"); `sudo nft list table inet longarm` shows it. alice's own `curl` and `longarm forward` as alice still work |

## 7. Teardown

```sh
./deploy.sh --name $NAME --teardown            # asks for the name; deletes $RG
az group exists -n $RG                          # false
```

On the laptop and worker: `longarm leave` / `longarmd leave --force` (or
delete their config dirs) - the test CA is gone with the resource group.

## Report back

For each step: pass/fail, the recorded output, durations of step 1 and the
`--recreate`, the transport paths from step 4 (direct or relay, NAT types),
the per-user table of step 6, and the Azure region. Known
risks worth watching: Docker Hub rate limits on the Caddy image (step 1:
caddy container `Waiting`/`ErrImagePull`), Let's Encrypt issuance time
(panel `healthz` not answering for a few minutes), ACI probes restarting the
hub during a slow first start (`az container show ... instanceView.events`).
