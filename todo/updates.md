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
