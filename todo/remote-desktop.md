# Remote desktop (`longarm desktop`, docs/usage.md "Remote desktop")

## Native remote desktop: the Longarm Desktop service in CI

TestDesktopService (e2e/desktop_service_windows_test.go) is opt-in like the
other service tests (LONGARM_SERVICE_TEST, elevated, in the console
session); it ran on a Windows 365 Cloud PC worker, not in CI. The MSI's
desktop-setup / desktop-remove steps are covered by the MSI tests only as
far as the custom action data goes.


## Native remote desktop: Linux binaries link libc (OpenH264)

H.264 on Linux loads Cisco's OpenH264 with purego
(internal/screen/openh264.go), and importing purego makes every Linux
binary that links internal/screen dynamically linked against glibc
(longarm, longarmd, and longarm-hub through internal/server), even with
CGO_ENABLED=0. The hub image is distroless `static-debian12`, which has
no libc: deploy/hub/image-build.sh must build longarm-hub with
`-tags noopenh264` (or internal/server must stop linking the screen
code) before this ships. Workers on musl (Alpine) need a
`-tags noopenh264` build too: JPEG tiles only. Check the release
builds' `file` output.

## Native remote desktop: Linux gaps

What internal/screen/*_linux.go leaves out:

- Wayland sessions are refused ("not supported yet"): the portal
  (ScreenCast + RemoteDesktop over D-Bus) and a PipeWire client are
  needed (docs/remote-desktop.md "macOS and Linux").
- No GPU encoder (VA-API/NVENC need cgo): OpenH264 encodes in software.
  It is downloaded from Cisco on first use (up to 30 s, then JPEG tiles;
  a failure is remembered for 10 minutes). The e2e test needs that
  download, an installed libopenh264.so.8/.7, or $LONGARM_OPENH264.
  Moving to a newer OpenH264 means new hashes in openh264_fetch.go.
- longarmd without $DISPLAY tries :0 with $XAUTHORITY, gdm's
  /run/user/<uid>/gdm/Xauthority or ~/.Xauthority; other display
  managers' cookies (LightDM, SDDM) are not found.
- Capture grabs the whole monitor on every DAMAGE report; the damaged
  rectangles could limit it (matters for remote X displays without
  MIT-SHM). A monitor layout change without a root size change is not
  noticed.
- The viewer: X key auto-repeat arrives as release/press pairs (xgb has
  no XKB for detectable auto-repeat); no clipboard; the worker's pointer
  is shown as the local cursor's shape, not drawn at its position, so a
  view-only window does not show where it is.
## Native remote desktop: macOS follow-ups

The backend (`internal/screen/*_darwin.go`) was written on Linux,
cross-compiled; its first run on a Mac is CI's e2e (which skips without
Screen Recording and Accessibility for the runner). Left out:

- **The pointer's shape.** The worker sends the position only; the viewer
  shows its own arrow. `NSCursor.currentSystemCursor` (macOS 10.15+) gives
  the shape, but it is AppKit, called off the main thread of a daemon:
  try it on a Mac first.
- **Viewer on the GPU.** Pictures are decoded into memory and drawn as a
  CGImage (a copy per frame). The design's path is VideoToolbox decoding
  into IOSurfaces given to the layer (or a CAMetalLayer) without copies.
- **System keys in the viewer.** Cmd+Tab, Cmd+Space, Mission Control stay
  on the viewing Mac: taking them needs a CGEventTap (Accessibility
  permission), as the Windows viewer's keyboard hook does.
- **Dirty rectangles.** SCStream reports them (SCStreamFrameInfoDirtyRects);
  the JPEG path diffs whole frames instead.
- **Onboarding.** Screen Recording and Accessibility are granted by hand
  (or an MDM PPPC profile); the tray should open the right System Settings
  pane when `NativeReason` says one is missing.
- **The login window and lock screen** cannot be captured from a
  LaunchAgent; longarmd as a LaunchDaemon says so (`Available`).

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
