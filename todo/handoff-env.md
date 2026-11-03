# The session's environment in longarm handoff

docs/usage.md "Hand off an agent session" ("The session's environment");
the code is cmd/longarm/handoff_env.go.

## Hooks and settings.json

A session whose behaviour depends on hooks or on `settings.json`
(permissions, `env`) resumes without them on a target that lacks them.
Claude Code's `--settings <file>` could load them per run like the rest,
but hooks run commands, so carrying them needs a consent step (list them,
carry only with a flag). Start at `planClaudeEnv` and `envArgs`.

## Copilot CLI user instructions

`~/.copilot/copilot-instructions.md` (and `$COPILOT_CUSTOM_INSTRUCTIONS_DIRS`)
shape a Copilot session like Claude's `CLAUDE.md`, which is carried. Copilot
has no per-run option to append instructions; `--add-dir` of a directory
with `.github/copilot-instructions.md` may work - check, then add it to
`planCopilotEnv`.

## Executable bits of plugin files

Files go to a worker with mode 0600 (`hoEndpoint.upload`), so a plugin's
hook or MCP script that is run directly (not through `node` / `bash`)
fails on a Unix target. Keep the source's executable bit (`hoFile` has no
mode yet; `Files.List` returns one).

## The per-session directories are never removed

`<agent dir>/longarm-handoff/<id>/` stays after the session moves on (the
transcript does too, as a backup). `longarm handoff --cleanup` could list
and remove those of sessions handed off from the machine (the marker says
so), and their `longarm-handoff-backup/` entries.

## Plugins from a marketplace the target names differently

A plugin counts as present on the target only under the same name (Claude
Code: `installed_plugins.json` key; Copilot: `installed-plugins/<marketplace>/<plugin>`),
so the same plugin from a renamed marketplace is carried again - harmless
(it loads twice under one name, the per-run copy wins) but wasteful.
