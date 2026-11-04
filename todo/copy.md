# todo: `longarm cp` (worker to worker)

## No relay through this machine

When the target can't reach the source - no direct path, and no hub relay
for the pair (no hub, an older hub, a standalone relay) - `cp` fails after a
few tries with the target's "connect to <w1>: ...". `longarm handoff` then
relays through the client (`coder.direct` falls back, `cmd/longarm/handoff_code.go`);
`cp` could do the same: read from the source (`Files.Read`) and write to
the target (`Files.Write`) per file, with the same skip and resume logic,
saying so (as handoff: "relaying through this machine").

## One user helper per read with per-user isolation

On a source worker with per-user isolation every `Copy.List` / `Stat` /
`Read` starts the owner's user helper (`Server.filesAs`,
`internal/server/copy.go`): three helper starts per file. An offer could
keep one helper while it is read (closed with the offer, started again
when its stdio connection is gone).

## Many small files

Files go one by one, each with a hash `Stat` on both sides before its
`Read` (`copier.fileOnce`): a tree of many small files is round-trip bound.
A few files in parallel, or a `List` with hashes for the same-size check,
would help.

## Each new file is read twice on the source

`fileOnce` asks the source for the whole file's sha256 before reading it
(the target verifies against it before the file appears). The source could
send the hash after the data instead (a trailer on `Copy.Read`) when the
target writes to a fresh partial file.

## An MCP tool

`longarm mcp` has `fleet_read` / `fleet_write` but nothing for a copy
between workers; a `fleet_cp` (source, target, the summary) would let an
agent hand one job's output to another worker without the CLI.
