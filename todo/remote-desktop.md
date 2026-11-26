# Remote desktop (`longarm desktop`, docs/usage.md "Remote desktop")

## Native remote desktop: the Longarm Desktop service in CI

TestDesktopService (e2e/desktop_service_windows_test.go) is opt-in like the
other service tests (LONGARM_SERVICE_TEST, elevated, in the console
session); it ran on a Windows 365 Cloud PC worker, not in CI. The MSI's
desktop-setup / desktop-remove steps are covered by the MSI tests only as
far as the custom action data goes.

## Native remote desktop: macOS and Linux

`internal/screen/screen_{linux,darwin}.go` are placeholders (no capture,
no viewer); e2e/desktop_env_{linux,darwin}_test.go skip.

## Native remote desktop: clipboard, displays, tray

The proto carries clipboard text both ways, but neither side uses it yet.
The viewer has no display picker (`--display` only) and no "being viewed"
border on the worker. The tray's "Desktop" item still says "Remote
Desktop / Windows App" and is offered for Windows workers only
(web/src/components/WorkerCard.tsx): show it for any worker with
`desktop-native`.

## macOS and Linux workers

`internal/desktop/desktop_other.go` reports "Windows workers only". macOS
has Screen Sharing (VNC on 5900, the same screen: what UU Remote-style help
needs), GNOME has gnome-remote-desktop (RDP), others xrdp. Report
`DesktopInfo{kind: "vnc"|"rdp", rdp_port}` when the service listens on
loopback, and add a VNC launcher next to `launchRDP`
(`cmd/longarm/desktop_{windows,other}.go`: `open vnc://127.0.0.1:<port>` on
macOS, a VNC viewer elsewhere).

## Watching the same screen on Windows (VNC)

RDP opens its own session (or takes the console over); helping someone on
the screen they see needs a VNC server (TightVNC, TigerVNC, UltraVNC: GPL -
don't bundle them in the MSI). Detect one listening on 127.0.0.1:5900 and
offer `longarm desktop <w> --mode vnc`; say `winget install ...` when none is.

## Turning Remote Desktop on for a worker

A non-Dev Box Windows worker with Remote Desktop off reports
"Remote Desktop is off". Planned: an MSI property `ALLOW_REMOTE_DESKTOP=1`
and `longarmd desktop enable|disable` (elevated) that set
`fDenyTSConnections = 0` and leave the firewall rule off (the forward
reaches 127.0.0.1). Not done because the fleet's workers were Dev Boxes,
which have it on.

## Remote Desktop on per-user isolation workers

Forwards there reach only ports of the caller's own processes
(`portOwnedBy`, internal/server/tenancy.go), and 3389 is the system's, so
`longarm desktop` to a plain RDP worker needs an admin certificate. If
owners want it: allow the worker's owner (WorkerAccess.Owner) to forward to
`DesktopInfo.rdp_port` only, audited like any forward.
