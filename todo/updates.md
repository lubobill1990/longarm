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
(LongarmUpdate, longarmd-admin) and a `longarm` client are still running,
and on a busy desktop each costs InstallValidate ~40 s (on a CI runner the
same check takes seconds: it is the desktop, not the MSI).

The way left: the Longarm Update service doing the early stop when an
install of its own product asks (it runs as SYSTEM; the request must be
bound to a running msiexec of the Longarm product, or any user could stop
the machine's Longarm). Restart Manager is no way out (tried in #122, a
double-clicked upgrade on such a desktop): with
MSIRESTARTMANAGERCONTROL=DisableShutdown its detection took 8 minutes
there, and with no files-in-use dialog in the UI Windows Installer stops on
error 2803 ("dialog not found") - the dialog can't be left out, and showing
it is what #47 removed.

A program that starts Longarm's binaries itself while the install runs
(e.g. an app running `longarm-tray --serve` from the install dir) starts
after the stop and still holds its file at InstallFiles: that install
needs a reboot.

## Changing the worker service's configuration in an upgrade

An upgrade of a service worker without a password keeps the services as
they are (longarm.wxs KeepServiceLogon: InstallServices doesn't run), so a
later MSI that changes a ServiceInstall - longarmd's or LongarmUpdate's
arguments, name, start type - reaches only fresh installs, repairs and
upgrades given a password (TestMSIModeRun pins them). When such a change
comes, the install helper must apply it to the existing service without
touching its account (ChangeServiceConfig with SERVICE_NO_CHANGE and no
password, e.g. in install-helper msi setup), and the msi-install CI job
check it. (util:ServiceConfig, the failure actions, still applies.)
