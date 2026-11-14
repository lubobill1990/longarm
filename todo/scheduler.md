# Scheduler

## A placement's CPU from `--cpus`

The hub reserves a placed job's cores by its type
(`sched.Params.CoresFor(req.Type, 0)` in `internal/hub/schedq.go`
`reserveLocked` / `pickLocked`): `PlaceRequest` has `mem_gb` but no
cores, so `longarm run --cpus 8` counts as its type (1 core) until the
worker reports the job, when `jobCores` reads `spec.cpu_limit`. Add
`double cpus` to `PlaceRequest` (proto/longarm/v1/hub.proto), send it from
`cmd/longarm` placeOpts (jobs.go has `cpus`) and the MCP / panel
submissions, and pass it as the explicit value. An older hub ignores the
field.

## Smoothed CPU

The gate and the score read the CPU % of the worker's last metrics
report, a short window. A build between two steps reads idle and attracts the next job.
An EWMA (or the max of the last few reports) per node in the hub's
`n.metrics` handling (`internal/hub/node.go`, `NodeReport_Metrics`) would
steady it; the client-side pick has a single probe and would stay as is.

## Linux Hyper-V Dynamic Memory guests

Only Windows guests report their Dynamic Memory ceiling (the
`Hyper-V Dynamic Memory Integration Service` counter); a Linux guest
under `hv_balloon` counts against what it has now, so the scheduler
underrates it. There is no reliable in-guest source for the ceiling (the
hot-add regions in the SRAT are a candidate); a `[metrics] mem_max_gb` in
longarmd.toml set by the operator would be the simple way.
