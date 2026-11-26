# Remote desktop (`longarm desktop`, docs/usage.md "Remote desktop")

## Native remote desktop: the Longarm Desktop service

docs/remote-desktop.md "Decisions": a LocalSystem service (installed with
the worker role) that runs the capturer as SYSTEM in the console session,
so UAC prompts, the lock screen and Windows service workers (session 0)
work; it sends Ctrl+Alt+Del (SendSAS works only from session 0). longarmd
reaches it through `Server.DesktopAgent` (already wired: the stream is
proxied). `internal/screen/session_windows.go` has the pieces the spike
proved.

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
