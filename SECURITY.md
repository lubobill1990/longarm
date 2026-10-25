# Security policy

Longarm runs commands on other machines by design, so security reports are
taken seriously.

## Reporting a vulnerability

Please **do not open a public issue**. Report privately through GitHub:
[Security → Report a vulnerability](https://github.com/lubobill1990/longarm/security/advisories/new).

Include what you found, how to reproduce it, the affected version
(`longarm --version`, `longarmd --version`, `longarm-hub --version`) and the
impact you see. Redact hostnames, addresses, join codes, keys and
certificates from logs.

You can expect an acknowledgement within a week. Fixes are released as a new
patch version; the advisory is published once a fixed release is available.

## Supported versions

Only the latest release receives security fixes while Longarm is below 1.0.

## Scope

In scope, for example:

- a client, worker, relay or hub accepting a connection or call it should
  refuse (mTLS identity, role checks, revocation, join codes);
- a way for a `client` to act as `admin`, for the CI's `release` role to do
  more than publishing and rolling out, or for a node to impersonate another;
- secrets (`--secret-env`, MCP secrets, join codes, keys) leaking into logs,
  install logs, the hub or the relay;
- self-update accepting an unsigned or wrongly signed release, or a
  downgrade it should refuse;
- local control APIs (named pipes / unix sockets) reachable by other users.

Out of scope: anything a holder of a valid `client` or `admin` certificate can
do on the workers it is allowed to reach - running arbitrary code there is the
purpose of the tool (see "Security model" in the README).
