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

## The unelevated MSI path is tested by hand only

A double-clicked MSI and an uninstall from Settings > Apps run the early
stop (internal/msiupgrade stop-early) with the user's filtered token: only
their tray (and, uninstalling, their worker) goes before InstallValidate,
and the services and a `longarm` client are still running there. That
check is Restart Manager's detection (MSIRESTARTMANAGERCONTROL=
DisableShutdown, seconds) with no files-in-use dialog in the UI, instead of
Windows Installer's own (~40 s a process). CI installs elevated and silent
(msi-install-test.ps1), so it never takes this path: check by hand that a
double-clicked upgrade and a Settings uninstall with Longarm running pass
"Gathering required information" in seconds, show no dialog and need no
reboot. Should Restart Manager ask after all, the fallback is the Longarm
Update service doing the early stop when an install of its own product asks
(it runs as SYSTEM; the request must be bound to a running msiexec of the
Longarm product, or any user could stop the machine's Longarm).
