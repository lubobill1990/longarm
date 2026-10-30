# Longarm - notes for coding agents

Start with README.md (what Longarm is), CONTRIBUTING.md (setup, tests,
pull requests) and docs/design.md; docs/usage.md is the user reference.
Write code like the surrounding code: its comment density, naming and
idioms.

## Follow-ups: todo/

`todo/` holds known follow-ups that were left out of a change on purpose -
one file per area, one section per item (todo/README.md). Look there before
working on an area; delete an item in the pull request that finishes it;
add one when you leave something undone deliberately (not only in the
pull request description).

## Releasing

Every merge to `main` publishes a dev build; release candidates and
releases come from tags. The `longarm-release` skill
(.claude/skills/longarm-release) and docs/ci-cd.md "Release channels" have
the details.

## Generated files

- `internal/webui/dist` is the built web UI: after changing `web/`, run
  `node web/build.mjs` and commit the result (on a merge conflict there,
  rebuild instead of merging by hand).
- `proto/longarm/v1/*.pb.go`: `buf generate` after changing a `.proto`.

## Parallel work on Longarm workers

Independent parts of a change can run as Claude Code jobs on workers
(`longarm run <worker> --repo . -- claude -p ...` with an `--allowedTools`
allowlist): write the spec and any shared interfaces first and commit
them, give each job its own files, and have it finish with
`git bundle create <its worktree>/<name>.bundle <base>..HEAD` - a job
can't write outside its worktree. Fetch the bundle with `longarm get`,
merge, then build and test everything together.
