# Worker updates

## Other viewers of an older worker's owner-started update

A worker before 0.1.30 reports nothing about an MSI update its owner
starts with `longarm worker update` or the tray's button (MachineUpdate
goes client -> worker; the hub may not take a client's word for a node's
state). Only the clicking tray shows "updating" (from its own job); other
viewers see it offline until it comes back. Goes away as workers update to
0.1.30+, whose longarmd reports the Longarm Update service's progress
(internal/longarmd/machineupdate.go watchService).

## The RTT gap marked from the hub's timeline

The card colours failed RTT samples amber while the machine updates, and
labels the chart "gap: updating". Samples from before the update started
that failed for other reasons are coloured the same while it runs. Marking
exactly the update's window needs the update's started_at against each
sample's time (web/src/components/WorkerCard.tsx LinkRows; the pool's
history points have no timestamps yet).

## The files-in-use wait without an administrator's token

The MSI's early stop (internal/msiupgrade stop-early) needs an
administrator's token to stop the services and move binaries aside before
InstallValidate. A double-clicked MSI and an uninstall from Settings > Apps
run their immediate steps with the user's filtered token, so only the
user's own tray (and, uninstalling, their worker) goes early: the services
(LongarmUpdate, longarmd-admin) and a `longarm` client still cost
InstallValidate ~40 s each. Options: the Longarm Update service doing the
early stop when an install of its own product asks (it runs as SYSTEM;
the request must be bound to a running msiexec of the Longarm product, or
any user could stop the machine's Longarm), or Restart Manager for
detection only (MSIRESTARTMANAGERCONTROL=DisableShutdown) if it lists
windowless processes without the dialog #47 removed - to be tried on CI.
