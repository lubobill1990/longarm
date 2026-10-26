# Changelog

## Unreleased

### Added

- Port links (`longarm link`): a user's processes on one worker reach a
  loopback port of another worker as `127.0.0.1:<port>`
  ([docs/usage.md](docs/usage.md#port-links-worker-to-worker)).
- `[forward] targets` in longarmd.toml: what port forwards may reach.

### Breaking changes

- **Port forwards reach the worker's own loopback ports only by default.**
  `longarm forward`, ssh `-L`/`-D` through the built-in sshd (proxied and
  direct listeners) and port links now refuse other hosts, for every caller
  including admins. Before, a worker in shared isolation forwarded to any
  host:port it could reach (its LAN, other machines). See
  [Upgrading](#upgrading).

## Upgrading

### To the release with port links

- **Forwards to other hosts.** If you forward through a worker to
  machines on its network (e.g. `longarm forward w1 5432:db.lan:5432`), add
  this to the worker's `<data>/longarmd.toml` and restart longarmd:

  ```toml
  [forward]
  targets = "any"
  ```

  Without it such forwards fail with `PermissionDenied: this worker forwards
  to its own loopback ports only`, and longarmd logs a warning naming
  `[forward] targets = "any"` the first time it refuses one. Keep the
  default where users do not fully trust each other
  ([docs/security.md](docs/security.md)). With per-user isolation non-admins
  still only reach loopback ports of their own processes either way.
