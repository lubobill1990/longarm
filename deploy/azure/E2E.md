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
- Providers registered (once per subscription):
  `az provider register -n Microsoft.ContainerInstance && az provider register -n Microsoft.Storage`
  (and `Microsoft.OperationalInsights` for `--log-analytics`).
- The image. After the PR is merged, `image.yml` publishes
  `ghcr.io/lubobill1990/longarm-hub:edge`; make the package public once
  (GitHub → Packages → longarm-hub → Package settings → Change visibility), or
  pass `--registry-user <user>` with `LONGARM_REGISTRY_PASSWORD=<PAT with read:packages>`.
  To test before the merge, build and push it yourself (WSL/Linux with docker):
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
```

Expect (**record** the whole output and the duration):
- `creating resource group`, `deploying`, `waiting for the hub`, then the
  summary with `hub address $NAME.$LOC.azurecontainer.io:7443`, a 64-hex CA
  fingerprint, `panel https://$NAME.$LOC.azurecontainer.io`, a cost estimate,
  and the `longarm join ... <code> --ca-fingerprint <fp>` line.

Checks:

```sh
az resource list -g $RG -o table                        # storage account lahub..., container group $NAME
az container show -g $RG -n $NAME --query "{state:instanceView.state, ip:ipAddress.ip, fqdn:ipAddress.fqdn, ports:ipAddress.ports}" -o json
#   ports: 7443 TCP, 7443 UDP, 7444 UDP, 80 TCP, 443 TCP
az container logs -g $RG -n $NAME --container-name hub | head -40
#   "no matching backups found" (first start), "created a new CA", LONGARM_BOOTSTRAP line, "longarm-hub started"
curl -fsS https://$NAME.$LOC.azurecontainer.io/healthz            # valid Let's Encrypt certificate, JSON health
curl -fsS https://$NAME.$LOC.azurecontainer.io/ca.crt | openssl x509 -outform der | sha256sum
#   = the printed CA fingerprint
ACCT=$(az storage account list -g $RG --query "[0].name" -o tsv)
KEY=$(az storage account keys list -g $RG -n $ACCT --query "[0].value" -o tsv)
az storage file list -s state --account-name $ACCT --account-key "$KEY" -o table    # bootstrap.env, ca, hub.toml, tls
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
az container exec -g $RG -n $NAME --container-name hub --exec-command "/usr/local/bin/longarm-hub user add admin"
```

Log in at `https://$NAME.$LOC.azurecontainer.io/` and see the laptop node.

## 3. Join a worker

On the worker, with a code from `longarm admin join-code --role worker`:

```sh
longarmd join $NAME.$LOC.azurecontainer.io:7443 <worker-code> --ca-fingerprint <fp> --name e2e-w1
longarm admin approve e2e-w1                    # on the laptop (unless auto_approve)
# run it in the foreground for the test; listen on tcp and on the hub's longnet relay:
longarmd run --listen tcp://0.0.0.0:7701 --listen "longnet://$NAME.$LOC.azurecontainer.io:7443?name=e2e-w1"
```

On the laptop: `longarm ls` shows `e2e-w1` online with load, and
`longarm ls --direct` the path to it (**record**).

## 4. A job over each transport

```sh
longarm --transport longnet exec e2e-w1 -- hostname                     # longnet (direct after hole punching, or relay)
LONGARM_NET_DISABLE_DIRECT=1 longarm --transport longnet exec e2e-w1 -- hostname   # forced through the hub's relay, UDP 7443
LONGARM_NET_DISABLE_DIRECT=1 LONGARM_NET_RELAY_MODE=tcp longarm --transport longnet exec e2e-w1 -- hostname   # relay TLS fallback, TCP 7443
LONGARM_NET_LOG=1 longarm --transport longnet info e2e-w1               # NAT types from both STUN ports (7443 + 7444 UDP)
longarm --transport tcp exec e2e-w1 -- hostname                         # only if the laptop reaches the worker's 7701 directly
longarm run --repo . -f -- git log -1 --oneline                         # a scheduled job (the hub picks the worker), output followed
longarm ls --direct                                                      # record the paths, e.g. "longnet/relay 40ms (udp ...:7443, nat easy/hard)"
```

Expect every command to print the worker's hostname / output and exit 0;
the `nat x/y` column filled in (needs UDP 7444; `?` means the second STUN
port is not reachable). Tailscale leg: only if the hub has a tailnet provider
(`--tailscale-secret-file`); then also `longarm --transport tailscale exec e2e-w1 -- hostname`.

## 5. Restart the container group: data survives

```sh
az container restart -g $RG -n $NAME          # in-place restart (SIGTERM -> Litestream flush)
az container logs -g $RG -n $NAME --container-name hub | head -30
#   no "created a new CA"; LONGARM_BOOTSTRAP ... admin_joined=true admin_join_code= ; same ca_fingerprint
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
az container exec -g $RG -n $NAME --container-name hub --exec-command "/usr/local/bin/longarm-hub user ls"   # admin still there
```

Also an upgrade by re-running with the same mutable tag:
`./deploy.sh --name $NAME --image $IMG` → "pulling ... again (az container restart)".

## 6. Teardown

```sh
./deploy.sh --name $NAME --teardown            # asks for the name; deletes $RG
az group exists -n $RG                          # false
```

On the laptop and worker: `longarm leave` / `longarmd leave --force` (or
delete their config dirs) - the test CA is gone with the resource group.

## Report back

For each step: pass/fail, the recorded output, durations of step 1 and the
`--recreate`, the transport paths from step 4, and the Azure region. Known
risks worth watching: Docker Hub rate limits on the Caddy image (step 1:
caddy container `Waiting`/`ErrImagePull`), Let's Encrypt issuance time
(panel `healthz` not answering for a few minutes), ACI probes restarting the
hub during a slow first start (`az container show ... instanceView.events`).
