# Driving Longarm from a program

This page is for programs that run commands and agent sessions on Longarm
workers - an IDE, a desktop app, an orchestrator such as TMIA. It says how
to talk to Longarm and what Longarm promises to keep stable. The details of
every command are in the [usage reference](usage.md); this page links there
instead of repeating it.

The interface for programs is the `longarm` CLI: its exit codes, its
`--json` outputs and the features `longarm version --json` lists. Anything
else - the tables and text a human reads, messages on stderr, the gRPC API
between longarm and longarmd, files in the data directories - can change in
any release.

Longarm owns machines and sessions: adding machines, sign-in, labels,
updates, port links, toolchains, the machine views and the UI that runs and
steers agent sessions (the tray's Workers tab and Sessions window). A
program owns its work: which workers it uses for what and its own views of
that work. It links into Longarm's UI rather than rebuilding it.

## 1. Detect features, not versions

```sh
longarm version --json
```

prints `{"version": "...", "features": [...]}` ([Capabilities](usage.md#capabilities-longarm-version---json)).
Check `features` before using a flag or an output field; never compare
version strings. Feature names are lowercase-hyphenated and keep their
meaning forever; a new behaviour gets a new name. A `longarm` without the
`version` command (exit 255, "unknown command") has none of them.

Some features also need the worker (the table says which: "the worker must
also support it"). You don't have to ask the worker yourself: longarm does,
and a worker that is too old fails the command with **exit 2** before
anything is sent ("... does not support it; update longarmd").

## 2. Exit codes

The [exit codes](usage.md#exit-codes) (feature `exit-codes`) are the
contract; the message on stderr is for people. In short:

| code | | the program |
|---|---|---|
| 0 | done | |
| 2 | the request can't succeed as given; nothing was done (except: `session send` to a session that ended while being written to may have delivered some bytes) | fix the request; don't retry it unchanged |
| 3 | not found (job, session, worker, file) | |
| 69 | `jobs --hub-only`: no hub, or it did not answer | retry later, or ask the workers |
| 75 | refused for now: capacity, quota, rate; `session send --if-idle-ms` / `--if-input-seq` | retry later |
| 77 | not allowed, or the sign-in expired | tell the user (`longarm login`) |
| 137 / 254 | the job was killed / lost (`wait`, `run --wait`) | |
| 255 | anything else; the outcome may be unknown | look it up (`jobs --id`) or retry with the same `--id` |

Commands that mirror a job (`exec`, `run --wait`, `wait`) exit with the
job's own code once it ran; to tell Longarm's codes from the job's, submit
without `--wait` and read the outcome from `jobs --json`.

## 3. Submitting work: `run --id`, `--stdin-file`, `--agent-auth`

- **Choose the id.** `longarm run --id <ULID>` is idempotent: the same id
  and spec return the existing job (exit 0, its id on stdout), another spec
  is `AlreadyExists` (exit 2). After a 255 - did the submit reach the
  worker? - run it again with the same `--id`. With a local `--repo` every
  run uploads the checkout anew; the same content (commit, uncommitted and
  untracked files) is the same job, other content another job. `run`
  without `--wait` prints the id on stdout exactly when the job was
  submitted. ([Exit codes](usage.md#exit-codes))
- **Prompts and other payloads: `--stdin-file`** (feature `stdin-file`).
  The file becomes the job's stdin and is never written anywhere by
  Longarm - not the job record, the hub, the audit log. At most 1 MiB. If
  the payload was lost before the job started, the job ends `failed` with
  reason `stdin payload lost before start; resubmit`: resubmit with the
  same `--id` (and the same `--repo` content) and it starts again.
  Workers announcing `job-stdin-binding` also require identical payload
  content, not merely the same byte count. Older records without a binding
  are refused explicitly; inspect their outcome before choosing a new id.
  ([Job stdin payload](usage.md#job-stdin-payload---stdin-file))
- **Agent credentials: `--agent-auth`** (feature `agent-auth`). When the
  command is `claude` or `copilot` longarm forwards that agent's
  credentials from this machine, memory-only on the worker; name the agent
  (`--agent-auth claude`) when it runs inside a wrapper.
  ([Agent credentials](usage.md#agent-credentials---agent-auth))

## 4. Finding out what happened: `jobs --json`, `--hub-only`

`longarm jobs --json --id <job>` (features `jobs-source`, `jobs-hub-only`)
prints the job with `state`, `exitCode`, `reason`; `source` says whether
the hub's index (`"hub"`) or the workers (`"workers"`) answered. Only the
hub's answer makes a missing job authoritative. `--hub-only` asks only the
hub: exit 3 is "no such job", exit 69 "the hub didn't answer" - never
confused. A job submitted a moment ago can be missing from the index for
that moment. ([`jobs --json`](usage.md#jobs---json))

To cancel the observed job rather than a replacement, pass its exact
`createdAt` to `longarm kill <worker> <id> --if-created-at <createdAt>`
(client and worker feature `job-kill-if-created-at`). A stale timestamp
fails with exit 2 and leaves the replacement untouched. Keep the worker
identity, job id and generation together; an uncertain result needs
readback, not a new job id.

## 5. Workers: `worker ls --json`, `whoami --json`, `doctor --json`

- `longarm worker ls --json` lists the workers you may use - your own,
  shared with you, the fleet - with `name`, `id`, `online`, `state`,
  `owner`, `labels`, `os`, `arch`, `version` and the latest `metrics`.
  Which of them your program uses for what is your program's decision; to
  add a machine, label it, update it, send the user to Longarm (the tray,
  `longarm worker add`, `longarm worker label`, `longarm worker update`).
- `longarm whoami --json`: the certificate's `name`, `role`, `user` and
  the hub.
- `longarm info <worker> --json` (client feature `info-json`) returns the
  worker's info and `features` as one JSON value, without the normal
  connection-detail text. Check worker features when retry guarantees
  matter, for example `job-stdin-binding`. Like `jobs --json`, field names
  are camelCase and 64-bit integers are strings.
- `longarm doctor --json` (feature `doctor-json`): a checklist with stable
  `id`s, a `status` per check and a `fix` for people to run. Show it; don't
  run the fixes. ([`doctor --json`](usage.md#doctor---json))

## 6. Sessions for programs

A session is an interactive program (Claude Code, Copilot CLI, a shell) on
a worker's terminal that your program drives and a human can attach to at
any time. ([Sessions for programs](usage.md#sessions-for-programs))

```sh
longarm session start w1 -s agent1 --repo . --json -- claude                 # idempotent by name
longarm sessions w1 -s agent1 --json                                         # state, inputSeq, humanInputSeq, ...
longarm session send w1 -s agent1 --text "$PROMPT" --paste --key enter \
    --if-input-seq 57 --json                                                 # only if nothing came since 57
longarm logs w1 agent1 --session -f                                          # the transcript
longarm session stop w1 -s agent1 --grace 10s
```

- **Did a human type?** `inputSeq` counts every write to the terminal
  (keystrokes, sends, stop's Ctrl-C) and only grows; `humanInputSeq`
  counts an attached human's keystrokes only (feature `input-seq-human`).
  The answers a terminal sends by itself (device attributes, cursor
  position, colours, focus) are not input: an attached xterm.js no longer
  moves them.
- **Send only if nothing changed:** remember `inputSeq` (from `sessions
  --json` or the last `send --json`) and send with `--if-input-seq N`
  (feature `session-send-if-input-seq`): written only if it is still N,
  checked and written atomically, else exit 75 and nothing is written.
  `--if-idle-ms` is the time-based variant.
- **Submit text to a TUI:** `--paste --key enter` (feature
  `session-send-paste-enter`) writes a bracketed paste, waits a moment,
  then Enter, with no other input in between - so the program takes the
  text as a paste and the Enter as a submit.
- `--json` outputs: `session start --json`, `session send --json`,
  `sessions --json` (every field present, also at its zero value). Note
  that `sessions --json` has `inputSeq` as a string (protojson, 64-bit)
  but `session send --json` as a number: `{"worker": "w1", "session":
  "agent1", "inputSeq": 58}`; read both as integers.

## 7. What is stable

Stable - changed only by adding, never by removing or changing meaning:

- the exit codes and what each means;
- feature names (`longarm version --json`, the workers' `Node.Info`
  features);
- the `--json` outputs: field names, JSON types and meaning. Fields are
  added; a program ignores fields it doesn't know.

Not stable: human-readable output (tables, `ls` columns, stderr messages,
hints), the order of fields and of list entries, the gRPC API, file layouts
on the machines, and anything not described in [usage.md](usage.md).

Conventions of the `--json` outputs:

- Outputs built from the API use protojson: 64-bit integers are strings
  (`"logSize": "148"`), enums are names (`"JOB_STATE_RUNNING"`), times
  RFC 3339. `jobs --json` and `sessions --json` use camelCase names;
  `worker ls --json` and `whoami --json` the proto names (snake_case).
- Fields at their zero value are left out (read a missing field as zero /
  empty / false), except `sessions --json`, which has every field.
  `null` is a time or an object that is not set.
- Outputs not built from the API are plain JSON: numbers are numbers
  (`session send --json`'s `inputSeq`, `doctor --json`'s `schema`).

### Golden fixtures

[`docs/programs/fixtures/`](programs/fixtures) has a real output of each of

| file | command |
|---|---|
| `version.json` | `longarm version --json` |
| `info.json` | `longarm info <worker> --json` |
| `worker-ls.json` | `longarm worker ls --json` |
| `whoami.json` | `longarm whoami --json` |
| `jobs.json` | `longarm jobs --json` |
| `jobs-id.json` | `longarm jobs --json --id <job>` |
| `session-start.json` | `longarm session start <worker> -s <name> --repo <dir> --json -- <command>` |
| `session-send.json` | `longarm session send <worker> -s <name> --text <text> --json` |
| `sessions.json` | `longarm sessions --json` |
| `tail.json` | `longarm tail <worker> <path> --json` |
| `doctor.json` | `longarm doctor --json` |

produced by an end-to-end test from a hub and two workers and anonymised
(example names, ids, addresses and times). Copy them into your tests.
`tail --json` prints JSON lines, one object per line of the file;
`tail.json` is the array of them. CI checks that every field in them still
exists with the same JSON type, in every element of a list, and is null
only where it may be: a field removed, retyped or turned null fails
Longarm's build, a field added does not (`go test ./e2e -run
TestProgramsFixtures`; `-update` rewrites them). Values are examples;
`doctor.json`'s checks and each check's `data`, and a worker's `metrics`,
differ by machine. `info.json`'s `desktop` is optional and its fields
depend on the worker platform; the fixture checks its object type only.

The test sets things up so that fields left out at their zero value are
in the fixtures at least once: `worker-ls.json` has a worker of alice's
(`owner`) running a job (`jobs_active`, `reserved_gb`) and a fleet worker
with a toolchain profile (`profile`, `toolchain.profile`); `jobs.json` a
running, a killed (`reason`) and a finished job in a worktree (`workdir`,
`spec.workspace`); `sessions.json` a session with a workspace. Not in
them, as they need a bigger setup - read them as left out when zero:

- `jobs --json`: `unreachable` (only with `"source": "workers"`, when
  the hub did not answer and some workers did not either);
- `tail --json`: `truncated` (with `-f`, when the file shrank or was
  replaced);
- `session start --json`: `reason` (a session that failed to start);
- `worker ls --json`: `metrics.top_cpu` / `metrics.top_mem` (the process
  lists: only a hub with a single user shows them, and a worker with an
  owner makes the hub multi-user), `draining` /
  `drain_reason`, `shares`, `update`, and the other fields of
  `NodeInfo` (proto/longarm/v1/hub.proto) not shown.
