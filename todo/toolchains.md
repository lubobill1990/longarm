# Toolchains

## Detect workers that ignore owner-only desired tools

Older workers ignore a desired toolchain without a profile, so owner-only tools
can remain unapplied without an explicit compatibility error. The CLI currently
documents that an updated worker is required. Add worker capability negotiation
or a minimum-version check before promising owner-only reconciliation; start at
`internal/hub/worker_toolchain.go` and the worker hello in
`internal/longarmd/hubagent.go`. Do not infer support from owner_revision = 0:
older workers can still apply merged tools when a profile is assigned.

