# Security hardening

Findings of the November 2026 review left out of the fixes that change
nothing for legitimate use: each one changes behaviour someone sees, or
needs a design first.

## Handoff to local brings the source's plugins and MCP servers unasked

`handoff --from <worker> --to local` ships the worker's plugin directories
(hooks, scripts) and the MCP servers' `command` entries and starts
`claude --plugin-dir … --mcp-config …` here; only settings and hooks are
opt-in (`--carry-settings`). A compromised worker, or a co-tenant on a
shared-isolation worker, runs code on the laptop. Make remote plugins and
MCP opt-in or confirmed, at least when the source isn't the user's own
worker (cmd/longarm/handoff_env.go).

## Handoff skips agent-auth and connection pinning

Handoff sends MCP secrets (and with `--carry-settings` the settings env) to
a shared-isolation fleet worker with a warning only, where `run`/`exec`
refuse without `--agent-auth-shared-ok`; ownership comes from the hub
listing, not the end-to-end endorsement (`agentauth.EndorsedBy`), and the
RPCs aren't pinned (`PinPeer`). Route handoff through `agentauth.Check`
(cmd/longarm/handoff.go ownMachine, handoff_env.go). docs/security.md has
no handoff section yet.

## Workers take a downgrade pushed by the hub

`internal/longarmd/updater.go` check accepts any signed version that
differs from the running one; the downgrade policy is the hub's alone, so a
compromised hub (not trusted under end-to-end trust) can roll workers back
to a signed release with a known hole. Refuse non-newer versions unless a
rollback is authorised by something the hub can't forge (a signed
rollback marker, or local config). Admin rollbacks need that path first.

## Join codes: length, lifetime, a global budget

6 Crockford characters (~1e9), user and worker codes live 24 h by default;
guesses are limited per IPv4 address / IPv6 /64 only (hub joinLimit). Many
addresses still give a real chance per live code. Longer codes or shorter
default lifetimes change what users type and how long a code lasts; a
global failure budget needs a way not to lock out legitimate joins under
attack (internal/hub/enroll.go, events.go).

## Other users' shared workers are auto-placement candidates

A worker shared with you (or everyone) takes your jobs, code and forwarded
credentials without an opt-in; its placement inputs (free memory, warm
repos, isolation) are self-reported. Auto-place only on fleet workers and
your own unless you opt in (internal/hub/schedq.go candidatesLocked).

## Entra admins / allowed are checked at sign-in only

Removing someone from `[auth.entra] admins` or `allowed` (or disabling
them in Entra) doesn't touch certificates they hold: `renew` re-issues
with the stored role. Re-evaluate the policy on renewal for
identity-provider users (internal/hub/enroll.go renew, idp.go).

## A panel admin login named like a user acts as that user

`link.go` (`a.user = a.login`) and `panelViewer` treat an admin login with
the name of a Longarm user as that user; without an identity provider
`/link` then mints an admin certificate for them, past
`adminCredentialsRefusedLocked`. Refuse creating a user named like a panel
account, and drop the fallback.

## The tray's bootstrap code in a command line

On Linux and macOS the one-time `?token=` URL is in the argv of
`xdg-open`/`open`/the browser, readable by other local users who can then
redeem it first (internal/tray/server.go URL, os_other.go, os_darwin.go).
Pass it another way (a 0600 redirect file the browser opens), and rotate
the session cookie per bootstrap.

## Local channel names can be squatted

Windows pipe names (`\\.\pipe\longarm-…`, the shim's `longarm-s-…`) are
predictable: another local user can create one while the daemon is down
(a DoS), and a client may then talk to it. Check the server's owner SID /
process (GetNamedPipeServerProcessId) before use, and add a random part to
shim pipe names (internal/platform/windows/localipc.go, shim/session.go).
Linux: the root shim and local API use `/tmp/longarm-0`; use a root-only
runtime dir (`RuntimeDirectory=`).

## longnet accepts data packets from any UDP source

`longnet/node.go` readLoop hands a `typeData` packet to the inner QUIC
transport whatever address it came from; a spoofer holds half-open
handshakes and fills the shared path queue. Accept data only from a known
or verified candidate of `src`, or enable `VerifySourceAddress` on the
inner transport (check the hole-punching latency).

## The relay tells clients which workers are online

A client can address any worker by `WorkerID(name)` through the relay: an
online one answers the TLS handshake, an offline one gives PeerGone. Closing
it needs per-user visibility on the relay (hub-synced like the worker
pairs) or workers refusing unseen users silently (internal/relay).

## Any local user can install a dev or preview build

`longarm-update install --version V` (internal/updatesvc/service.go
OpInstall) is open to every local user and bypasses the channel only the
owner or an admin may set; the build is signed, but the no-downgrade rule
then pins the machine there. Limit explicit versions to the channel or to
owners/admins.
