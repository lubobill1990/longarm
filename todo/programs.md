# Programs driving Longarm (docs/programs.md)

## More fixtures

`docs/programs/fixtures` covers the outputs TMIA reads. Not yet: `ls
--json`, `session stop --json`, `agent-sessions --json`, `worker update
--json`. Add a command to `programFixtures` in
`e2e/programs_fixtures_test.go` (set up what it needs there) and run it
with `-update`. The fixtures were generated on Windows; `doctor.json`'s
checks differ per platform, which is why `checks[].data` is opaque to the
check. Fields still left out at zero are listed in docs/programs.md
("Golden fixtures"); `worker-ls.json` lost `metrics.top_cpu` / `top_mem`
when its worker got an owner (a multi-user hub hides them) - a second
capture on a single-user hub would bring them back.

## `sessions --json` and steerable runs

A program wanted `job` on a `sessions --json` entry of a steerable run
(`run --steerable`), but `Session` (proto/longarm/v1/longarm.proto) has no
such field: only the tray's agent API (`cmd/longarm/daemon_agents.go`) says
which job a session belongs to. Add `job` to `Session` (set by the worker
for a steerable run's terminal), with a feature name and a steerable run
in the fixtures test - which needs a fake `claude` that speaks the hooks
(internal/steer).

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

## e2e tests depend on the machine's global git config

On a worker whose global git config sets `core.autocrlf` / `core.safecrlf`,
`TestSync`, `TestSyncShallowClone`, `TestGitAuthForward`,
`TestSessionsForPrograms` and others fail at their own `git add` with
"LF would be replaced by CRLF" (seen on cpc workers, 2026-11-03), so a full
`go test ./...` there reports failures unrelated to the change. The e2e
helpers that run git (`e2e/data_test.go` and friends) should run it with an
isolated config: `GIT_CONFIG_GLOBAL` pointing at an empty file in the
test's temp dir and `GIT_CONFIG_NOSYSTEM=1`.
