# Jobs

## Worker job priority

Add `[jobs] priority = "normal"|"low"` in `longarmd.toml`, applying nice
on Unix and BELOW_NORMAL priority on Windows to jobs, without lowering
longarmd's own priority. Start with worker config (`internal/longarmd`) and
job process creation (`internal/jobs`, `internal/platform`). This would let
workers yield CPU to local use explicitly; macOS LaunchDaemons now default
to `ProcessType = Interactive` rather than inheriting launchd throttling.
