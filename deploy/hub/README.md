# Deploying longarm-hub

Example target: a small Linux VM (e.g. Ubuntu 24.04, 1-2 vCPU / 2 GB) with a public IP and
Caddy on ports 80/443. longarm-hub needs **7443/tcp + 7443/udp + 7444/udp** public (gRPC API, longnet
relay QUIC and its TLS fallback on the same TCP port, told apart by ALPN; the
relay's second STUN port) and
serves its panel/API + install scripts on **127.0.0.1:7480** for Caddy
(`hub.example.com`).

## docker compose

```sh
cd deploy/hub
cp hub.toml.example hub.toml && $EDITOR hub.toml      # public_url, public_addr, hosts
mkdir import && cp ~/.config/longarm/ca/ca.crt ~/.config/longarm/ca/ca.key import/   # existing CA
docker compose build
docker compose run --rm longarm-hub ca import /import      # or: ca init (new CA)
rm -r import                                          # and drop the ./import mount
docker compose run --rm longarm-hub ca show                # fingerprint for --ca-fingerprint
docker compose run --rm -T longarm-hub user add ops --password-stdin <<< 'a long passphrase'
#   -> prints the TOTP secret / otpauth:// URI for an authenticator app
docker compose up -d
curl -s 127.0.0.1:7480/healthz
```

Caddy: add [Caddyfile.snippet](Caddyfile.snippet) (`reverse_proxy
127.0.0.1:7480` with `flush_interval -1` for server-sent events) and reload.
`web.trusted_proxies` in hub.toml must contain the address Caddy connects from
(the docker bridge, `172.16.0.0/12`), so login rate limiting and install
script URLs use the real client.

Firewall (cloud security group, ufw, ...): allow 7443/tcp, 7443/udp and 7444/udp inbound. 7444/udp
is the relay's second STUN port (`relay_stun2`): with only one STUN point,
nodes can't tell easy from hard NAT and `longarm ls` shows no `nat x/y`.

## systemd (no docker)

```sh
useradd --system --home /var/lib/longarm-hub --shell /usr/sbin/nologin longarm-hub
install -m 0755 longarm-hub /usr/local/bin/
install -D -m 0640 -g longarm-hub hub.toml /etc/longarm-hub/hub.toml   # data_dir = "/var/lib/longarm-hub", http = "127.0.0.1:7480"
install -m 0644 longarm-hub.service /etc/systemd/system/
sudo -u longarm-hub longarm-hub --config /etc/longarm-hub/hub.toml ca import /path/to/ca
systemctl enable --now longarm-hub
```

Updates from CI: the `deploy` user with the SSH forced command
[`longarm-hub-deploy`](longarm-hub-deploy) (verifies the release signature with the installed
longarm-hub, keeps `longarm-hub.prev`, health-checks `/healthz`, rolls back) - setup in
[docs/ci-cd.md](../../docs/ci-cd.md#23-hub-deploy-user-ssh-forced-command-no-shell).

## First steps

```sh
# on the hub: a join code for your own longarm (admin role)
docker compose run --rm longarm-hub join-code --role admin
# on your machine
longarm join 203.0.113.10:7443 <code> --ca-fingerprint <fp>
longarm admin join-code                      # worker codes from now on
longarm-hub release add ./dist --version 0.2.0   # binaries for install.ps1 / install.sh (longarm-*, longarmd-*)
```

Existing workers keep their hand-issued certificates (same CA): add
`[hub] address = "203.0.113.10:7443"` to their longarmd.toml and restart longarmd; they
register themselves as approved.

## Tailnet provider: Tailscale (default) or Headscale

The hub hands every node it enrolls a one-time, pre-authorized, tagged key
for the embedded tsnet node of `longarmd` / `longarm`, so nobody installs `tailscaled`
or logs in on a new machine. `[tailnet] provider` in hub.toml picks the
control plane: `tailscale` (the official service), `headscale`
([deploy/headscale](../headscale/README.md)), or `none`.

**Tailscale (official service, free plan):**

1. Tailnet policy (admin console → Access controls). The hub's credential gets
   a tag of its own, `tag:longarm-hub`, which **owns** the two node tags.
   Add a grant/ACL that lets `tag:longarm-client` reach `tag:longarm-worker:7701`
   (the same rule is in `deploy/headscale/config/policy.hujson`):
   ```jsonc
   "tagOwners": {
     "tag:longarm-hub": ["autogroup:admin"],
     "tag:longarm-worker":    ["autogroup:admin", "tag:longarm-hub"],
     "tag:longarm-client":    ["autogroup:admin", "tag:longarm-hub"],
   },
   "grants": [
     {"src": ["tag:longarm-client"], "dst": ["tag:longarm-worker"], "ip": ["tcp:7701"]},
   ],
   ```
2. Trust credential / OAuth client (Settings → Trust credentials, formerly
   OAuth clients): scope **Auth Keys: write** (`auth_keys`) only, and **only
   the tag `tag:longarm-hub`**. Store the secret on the hub, readable by the
   hub only:
   ```sh
   install -m 0640 -o root -g longarm-hub /dev/stdin /etc/longarm-hub/tailscale-oauth.secret <<< 'tskey-client-...'
   ```
   (docker: mount it read-only into the container and point
   `oauth_secret_file` at the mounted path, or pass
   `LONGARM_HUB_TAILSCALE_OAUTH_SECRET`).
3. hub.toml:
   ```toml
   [tailnet]
   provider = "tailscale"
   ephemeral_clients = true      # longarm / longarm daemon / longarm mcp; workers never (stable tailnet IP)
   key_ttl = "1h"
   [tailnet.tailscale]
   oauth_secret_file = "/etc/longarm-hub/tailscale-oauth.secret"
   ```
   then `systemctl restart longarm-hub`. The hub log shows `tailnet key minted`
   (provider, node, tags, reason; never the key) for each key.
4. Check: `longarm admin tailnet status` lists the provider, the control URL and
   every node's tailnet state, tailnet name and IPs.
   Live API check from a dev machine (mints one ephemeral 5-minute key and
   deletes it again):
   `LONGARM_TAILSCALE_OAUTH_SECRET_FILE=/path/to/secret go test -run TestTailscaleLive -v ./internal/hub/`.

**Why `tag:longarm-hub`.** Tailscale lets a trust credential mint only keys
that carry **exactly the credential's tags, or tags owned by them**.
A credential tagged `tag:longarm-worker` +
`tag:longarm-client` can mint a key with both tags, but not the key a worker needs
(`tag:longarm-worker` alone). The API then answers `400 requested tags are invalid
or not permitted`. With the credential on `tag:longarm-hub` and that tag
listed as an owner of both node tags, the hub can mint either one.

If the credential is set up the old way, every key fails:
- the hub log shows `tailnet key … requested tags … invalid or not permitted`
  together with a pointer to this section;
- `longarm admin tailnet status` prints the last key error and a `hint` with the
  working policy;
- workers show `needs-key`.

After fixing the credential (or its tags), `longarm admin tailnet rekey <worker>`
makes a waiting worker ask again at once.

The OAuth client can't delete devices (no `devices` scope). `longarmd leave`
logs a worker's node out, and ephemeral client nodes disappear by
themselves. Stale worker devices (left, re-keyed with `longarm admin tailnet rekey`,
reinstalled) stay in the Machines list until an admin removes them there.

**Headscale:** follow [deploy/headscale](../headscale/README.md), then set
`provider = "headscale"` with a `[tailnet.headscale]` section. The old
`[headscale]` section still works.

**Switching** is the `provider` line plus a hub restart. Workers move on their
next hub session and clients on their next hub contact. Each control server
has its own state dir on the node (`tsnet-<host>`), so switching back reuses
the old identity while it is still valid. Endpoints advertised to clients
update through the hub.

## Backup

The database is `<data>/hub.db` (SQLite, WAL mode). With Litestream
([litestream.yml](litestream.yml)), uncomment the `litestream` service in the
compose file. The CA key lives in `<data>/ca/ca.key`: back it up separately and
keep it offline. Losing hub.db loses join codes, users, profiles and the node
registry (workers re-register with their certificates; jobs are rebuilt from
worker reports).
