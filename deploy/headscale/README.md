# Headscale: the self-hosted tailnet provider (DESIGN §4.2, §4.4)

Longarm reaches workers over several *legs* and races them (`longarm ls` shows
the winner): plain TCP, the self-made `longnet` (QUIC + hole punching + the
hub's relay) and **Tailscale through a self-hosted Headscale**. Headscale
gives the embedded `tsnet` nodes of `longarmd` and `longarm` a control plane we own,
with its **embedded DERP relay + STUN** (plus, by default, Tailscale's public
DERP/STUN servers; see "DERP map") and uses Tailscale's mature NAT traversal.

The default tailnet provider is the official Tailscale service ([deploy/hub](../hub/README.md),
"Tailnet provider"); Headscale is the self-hosted alternative, picked with
`[tailnet] provider = "headscale"` in the hub's hub.toml.

Nobody touches Headscale for day-to-day work: longarm-hub mints a **one-time,
tagged pre-auth key** for every node it enrolls (`tag:longarm-worker` for longarmd,
`tag:longarm-client` for longarm) and hands it over in the `longarmd join` / `longarm join`
response. The node stores it next to its tsnet state, joins the tailnet on
first use and deletes the key.

## Files

| File | |
|---|---|
| `docker-compose.yml` | `headscale/headscale:v0.29.5`; 8080 (API + DERP) on 127.0.0.1 for Caddy, 3478/udp STUN public |
| `config/config.yaml` | `server_url https://hs.example.com`, embedded DERP region 900 (`/derp`), STUN on 3478, **plus Tailscale's public DERP map** (see "DERP map" below), sqlite |
| `config/policy.hujson` | tags `tag:longarm-worker`, `tag:longarm-client`; only client -> worker:7701 is allowed |
| `Caddyfile.snippet` | `hs.example.com` -> 127.0.0.1:8080 (Upgrade passes through), plain-HTTP captive-portal probe |

## Runbook (on the hub VM, 203.0.113.10, next to longarm-hub)

1. **DNS:** `hs.example.com` A -> 203.0.113.10.
2. **Firewall:** allow inbound **3478/udp** (STUN). 443/tcp is Caddy's.
   (UDP 41641 is *not* needed: nodes' own WireGuard ports are punched.)
3. **Caddy:** append `Caddyfile.snippet` to the host's Caddyfile, `caddy reload`.
4. **Start Headscale:**
   ```bash
   cd deploy/headscale
   docker compose up -d
   docker compose logs -f headscale        # "listening and serving HTTP on: 0.0.0.0:8080"
   curl -fsS https://hs.example.com/health
   ```
5. **API key for the hub** (365 days; rotate before it expires):
   ```bash
   docker compose exec headscale headscale apikeys create --expiration 365d > ../hub/headscale-api.key
   chmod 600 ../hub/headscale-api.key
   ```
6. **longarm-hub:** add to `hub.toml` and restart the hub (the old `[headscale]`
   section is still accepted):
   ```toml
   [tailnet]
   provider = "headscale"
   [tailnet.headscale]
   url = "https://hs.example.com"          # what nodes use as tsnet ControlURL
   api_url = "http://headscale:8080"         # or http://127.0.0.1:8080; how the hub reaches the API
   api_key_file = "/etc/longarm-hub/headscale-api.key"
   ```
   (In docker compose mount the key file and put both containers on one
   network, or use `api_url = "http://host.docker.internal:8080"`.)
7. **Existing nodes** (enrolled before Headscale, or on another provider)
   follow by themselves: workers on their next hub session, clients on their
   next hub contact (`longarm ls`). They fetch a key from the hub and keep a state
   dir per control server (`tsnet-hs.example.com`).
8. **Check:**
   ```bash
   docker compose exec headscale headscale nodes list     # longarm-<worker> tag:longarm-worker, longarm-client-<you> tag:longarm-client
   longarm admin nodes --json | grep tailscale://              # workers advertise tailscale://100.64.x.y:7701
   longarm --transport tailscale info <worker>                 # path: tailscale/direct|derp ...
   longarm ls --direct                                         # the race: "[tcp ..., longnet ..., tailscale ... won]"
   ```

## Notes

- **Ephemeral or not:** keys are *not* ephemeral by default
  (`[headscale] ephemeral = false`), so a worker keeps its tailnet IP across
  restarts and the hub's advertised endpoint stays valid. Tagged nodes don't
  expire.
- **Tag-only keys** need Headscale >= 0.28. For older versions set
  `[headscale] user = "longarm"` (the hub creates the user and passes its id).
- **DERP over Caddy:** the DERP protocol is an HTTP Upgrade on `/derp`;
  Caddy's `reverse_proxy` forwards Upgrade requests as-is. `flush_interval -1`
  and the zero timeouts keep long polls and relayed streams open.
- **Same-host test caveat:** in this repo's opt-in live test
  (`LONGARM_HEADSCALE_BIN`) both tsnet nodes register
  and the worker advertises its tailnet IP, but with no reachable DERP two
  nodes on one Windows host did not complete a WireGuard handshake. Verify the
  data path in your deployment (step 8).
- **Firewalls:** Windows Defender Firewall must allow inbound UDP for
  `longarmd.exe` / `longarm.exe` (the MSI adds the rules), or Tailscale can only relay.

## DERP map: embedded region + Tailscale's public one (default)

`config.yaml` gives nodes **both** the embedded region 900 and Tailscale's
public DERP map (`derp.urls: [https://controlplane.tailscale.com/derpmap/default]`).

**Why.** If UDP to the hub's STUN port is filtered somewhere on a client's
network, the client never learns its public endpoint, can't hole-punch and
stays relayed through DERP. Tailscale's map gives it more STUN servers to try.
Nodes need just one reachable STUN server; more regions mean more networks
where one works, and a closer DERP fallback.

**What Tailscale sees.** Their DERP servers relay only WireGuard packets,
encrypted end to end between the two nodes, and they see node public keys and
IP addresses. Control (keys, ACLs, the netmap) stays on Headscale.

**Fully self-hosted.** Set `derp.urls: []` (and keep the embedded server)
when nothing may touch Tailscale Inc.'s infrastructure. Then make sure UDP
3478 to the hub VM is reachable from every network your nodes use: check
with `tailscale netcheck` from such a network (or `longarm --transport tailscale
info <worker>`: `tailscale/direct` vs `tailscale/derp`). Otherwise nodes stay
on the relay.
