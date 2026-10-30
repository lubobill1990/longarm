# Ports: forwards (worker to you) and links (worker to worker)

## `longarm forward` (worker port on your localhost)

`longarm forward <w> <[host:]remotePort> [localPort]`

| flag / argument | meaning |
|---|---|
| `localPort` | defaults to the same port; `0` = any free port |
| `--bind ADDR` | local listen address (default 127.0.0.1) |
| `--bg` | hosted by the background `longarm daemon`, which is started if needed; the command returns |
| `--list` | lists running forwards |
| `--stop <localPort>\|all` | stops forwards |

- Without `--bg`, the forward runs in the foreground until interrupted. In agent tool calls, use `--bg` or MCP `fleet_forward`, which returns the local URL.
- `host:` is resolved on the worker. By default a worker allows only its own loopback (`[forward] targets = "loopback"` in longarmd.toml).
- With per-user isolation, the remote port must belong to a process of your own account.

## `longarm link` (port of worker B reachable on worker A)

```sh
longarm link create <wb>:8080 --on <wa> [--port 18080] [--ttl 8h] [--job <job-on-wa>] [--json]
longarm link ls [--json]          # state: listening | error | offline; connections; bytes
longarm link rm <link-id>         # cuts open connections
```

- Traffic goes from A to B directly, or through the hub's relay. It never passes through your machine. The hub signs a grant per link, and both workers apply their own policy to you.
- Processes on A connect to `127.0.0.1:<port>`. Jobs, sessions and `exec` started on A afterwards get `LONGARM_LINKS=wb:8080=127.0.0.1:18080,...` and `LONGARM_LINK_WB_8080=127.0.0.1:18080` (the worker name upper-cased, other characters `_`). Jobs keep the values they got at start, so create the link **before** starting the consumer.
- The link ends at its TTL (default 8h, at most the hub's `max_ttl`), when its `--job` ends, on `link rm`, or when a worker is revoked.
- Limits:
  - TCP only.
  - Listen ports 1024-65535, unique per worker. A port taken on A shows `error` and is left out of the variables.
  - 20 links per user by default.
  - The target port on B must be listened on by your account (per-user isolation).
- MCP: `fleet_link` with `action` create, list or remove.

## E2E recipes

Run the browser tests locally against a dev server on a worker:

```sh
longarm session start <w> -s web --repo . -- pnpm dev --host 127.0.0.1 --port 5173
longarm forward <w> 5173 --bg
pnpm playwright test            # baseURL http://127.0.0.1:5173
longarm forward --stop 5173 ; longarm session stop <w> -s web
```

Run the frontend and tests on worker A against a backend on worker B:

```sh
longarm session start <wb> -s api --repo . -- go run ./cmd/api --listen 127.0.0.1:8080
longarm link create <wb>:8080 --on <wa> --port 18080 --ttl 2h --json      # note the link id
longarm run <wa> --repo . --wait -e API_URL=http://127.0.0.1:18080 -- pnpm test:e2e
longarm link rm <link-id> ; longarm session stop <wb> -s api
```

To make a worker's port reachable from a teammate's worker, the teammate needs access to both workers. Links are personal: other users can't see or use yours.
