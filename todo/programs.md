# Programs driving Longarm (docs/programs.md)

## Worker features as JSON

A program can't ask which features a worker has: `longarm info <worker>`
prints the `Node.Info` JSON followed by plain-text lines (`path:`,
`policy:`, `legs:`), so it isn't parseable as a whole. Today programs rely
on longarm refusing a flag the worker lacks (exit 2, nothing sent). Add
`longarm info <worker> --json` (or worker features in `worker ls --json`,
which the hub would have to keep from heartbeats) with a feature name, a
fixture and a row in docs/usage.md. Start at `infoCmd` in
`cmd/longarm/workers.go`.

## More fixtures

`docs/programs/fixtures` covers the six outputs TMIA reads first. Not yet:
`ls --json`, `session start --json`, `session send --json`, `session stop
--json`, `tail --json`, `agent-sessions --json`, `worker update --json`.
Add a command to `programFixtures` in `e2e/programs_fixtures_test.go` (set
up what it needs there) and run it with `-update`. The fixtures were
generated on Windows; `doctor.json`'s checks differ per platform, which is
why `checks[].data` is opaque to the check.

## Bracketed paste the program did not ask for

`session send --paste` always wraps the text in `ESC [200~` / `ESC
[201~`. A program that never turned bracketed paste on (`ESC [?2004h`)
sees the markers as input. The shim sees the program's output and could
track the mode (`internal/shim/session.go` `hub.output`) and send the
text unwrapped - or refuse - when it is off.

## Terminal replies the classifier does not know

`attach.IsReply` (`internal/attach/reply.go`) knows DA1/DA2, DSR OK, CPR /
DECXCPR, OSC 10/11/12 colours and focus in/out, as 7-bit sequences. Not
classified, so still counted as input: 8-bit C1 forms (`0x9b` CSI, `0x9d`
OSC), DECRPM (`CSI ? Ps ; Pm $ y`), the kitty keyboard flags (`CSI ?
flags u`), XTVERSION (`DCS > | ... ST`), window reports (`CSI 8 ; r ; c
t`), OSC 4 palette replies, and a reply split across two reads of the
client's terminal. Add them with recorded sequences in `reply_test.go` as
clients turn out to send them.

## The tray's send

The agent API of `longarm daemon` (`POST
/v1/workers/{worker}/sessions/{name}/send`, `cmd/longarm/daemon_agents.go`
`sendReq.writes` / `sendSession`) still sends the text and the Enter as two
sends. It could use `SendSessionRequest.after` (paste, then Enter,
atomically) and take an `if_input_seq` from the Sessions window like the
CLI.
