# Native remote desktop (proposal)

Status: exploration. A Windows spike (capture, encode, browser viewer,
input) is in `internal/screen`; nothing is wired into longarmd yet.

## Why

`longarm desktop` today does not show a screen itself: it finds a Dev
Box's `ms-avd:` link, or forwards a worker's Remote Desktop port to mstsc
(docs/usage.md "Remote desktop"). That leaves out:

- **The screen someone is looking at.** RDP opens its own session (or
  takes the console over and locks it); helping a person on their screen,
  or watching what an agent's GUI test is doing, needs the console session.
- **Windows Home** (no Remote Desktop server), workers with Remote Desktop
  off, and per-user isolation workers (3389 is the system's port, so only
  admins may forward to it - todo/remote-desktop.md).
- **macOS and Linux workers** ("Windows workers only").
- **One path for everything.** RDP/VNC each need a client app, and their
  own sign-in. A native desktop rides the connection Longarm already has
  to every worker (direct or relayed, mTLS, the end-to-end keys of #173),
  is checked by the same access rules and lands in the same audit log.

The RDP and Dev Box paths stay: they are the better choice for a Dev
Box (Windows App, Shortpath, Entra). The native desktop is a new kind next
to them, and the only kind on macOS/Linux/Home.

## Spike results (Windows 11, 2560×1440, RTX 5090, pure Go, CGO_ENABLED=0)

`go run ./internal/screen/spike bench|serve` (see its doc comment).

| | |
|---|---|
| Capture | DXGI Desktop Duplication through raw COM vtables (slots checked against the 10.0.22621 SDK headers): works, gives dirty rectangles; idle screen costs nothing (AcquireNextFrame waits) |
| Full frame BGRA→RGBA | 3.2 ms |
| Full frame JPEG q70 (stdlib, 1 core) | 41 ms, 418 KB |
| Ordinary desktop use (terminal + editor) | ~5 changed frames/s, 19% of pixels per change, ~600 KB/s |
| Browser viewer | WebSocket + `createImageBitmap` + canvas: the live desktop renders correctly at full resolution |
| Input | `SendInput` (absolute mouse over the virtual desktop, wheel, scan-code keys) builds and is wired to the viewer; not exercised against the developer's live desktop |

So the whole loop needs no cgo and no third-party code. JPEG tiles are
good enough for an office desktop on a LAN or a good relay. They are not
good enough for video or scrolling over a slow link: that needs H.264
(below).

## Architecture

```
viewer (tray window /desktop/<worker>, WebView2/WKWebView; later the hub panel)
   │ WebSocket (loopback, tray session cookie)
longarm-tray server  ── gRPC Desktop.Stream over the node connection ──►  longarmd (worker)
   (client side, reuses the client's mTLS / direct / relay paths)            │
                                                                             ├─ session mode: captures in-process
                                                                             └─ service mode: desktop agent in the
                                                                                console session (named pipe)
```

### Worker: where capture runs

A process can only capture and inject into the session and desktop it is
attached to.

- **Session mode** (the MSI default: longarmd as the signed-in user,
  started at logon) is already in the console session: capture in
  longarmd. It cannot see the secure desktop (UAC prompts, lock screen,
  Ctrl+Alt+Del), and UIPI stops its input at elevated windows. The viewer
  shows "the worker is showing a secure screen" rather than a frozen
  image (duplication fails with ACCESS_LOST while it is up).
- **Service mode / admin worker** (LocalSystem): longarmd starts
  `longarmd desktop-agent` in the active console session
  (`WTSGetActiveConsoleSessionId`, its own token duplicated with
  `TokenSessionId` set, `CreateProcessAsUser` on `winsta0\default`), and
  re-attaches it to the input desktop on every switch
  (`OpenInputDesktop`/`SetThreadDesktop` - the spike does this). As
  SYSTEM in the session it can show and drive UAC prompts and the lock
  screen, and can send Ctrl+Alt+Del (`SendSAS`, with the
  SoftwareSASGeneration policy). This is how RustDesk/TeamViewer work.
  The agent talks to longarmd over a named pipe restricted to SYSTEM.
- **No one signed in** (service mode only): the agent runs on the
  Winlogon desktop, so you can sign in remotely.
- **RDP-only sessions / VMs without a display driver**: DXGI duplication
  can fail; fall back to GDI `BitBlt` (slower, no dirty rects - diff tiles).

### Encoding

1. **v0: JPEG dirty tiles** (the spike). Pure Go. Encode the dirty
   rectangles in parallel strips (1440p full frame: ~41 ms on one core,
   ~6 ms on 8); a lossless PNG refresh of a region after it has been still
   for 1 s, so text ends up sharp.
2. **v1: H.264** through Media Foundation: the hardware encoder MFTs
   (NVENC/Quick Sync/AMF) and Microsoft's software H.264 encoder are part
   of Windows, and they are COM like DXGI, so Go needs no cgo. Low-latency mode,
   CBR at the bandwidth the link measures, a keyframe on request. Decode
   in the viewer with WebCodecs `VideoDecoder` (WebView2 and Chromium:
   yes; WKWebView: Safari 16.4+; WebKitGTK: not reliably → JPEG there). No
   openh264: Cisco's patent cover only applies to its own downloaded binary.
   Media Foundation is missing on N editions without the Media Feature Pack
   → JPEG there.
3. **The cursor** is drawn by the viewer (shape from
   `GetFramePointerShape`, position in every frame header). Moving the
   pointer then costs no video and feels local.

### Protocol (gRPC, longarmd)

```proto
service Desktop {
  // The worker's displays and what it can do (codecs, control, secure desktop).
  rpc Info(DesktopInfoRequest) returns (DesktopCaps);
  rpc Stream(stream DesktopClientMsg) returns (stream DesktopServerMsg);
}
// client → worker: Start{display, codecs, max_fps, view_only}, Ack{frame},
//   Input{mouse|wheel|key(scan code)|text}, Clipboard{text}, KeyframeRequest, Sas
// worker → client: Started{w,h,codec}, Frame{seq, tiles[] | h264 access unit,
//   cursor}, CursorShape{id, png, hotspot}, Clipboard{text}, Secure{bool}, Ended{reason}
```

Flow control: at most N (≈3) unacked frames in flight. Capture waits
instead of queueing, and the dirty rectangles accumulate in DXGI meanwhile,
so a slow link shows fewer but current frames (the spike already behaves
this way). The H.264 bitrate follows the measured ack rate.

The node connection already does direct-or-relay and end-to-end keys. A
gRPC stream over the relay adds a TCP hop. That is acceptable for v0;
a QUIC datagram path for the direct case can come later.

### Viewer

A tray page `/desktop/<worker>` in its own window (the tray is Wails, so
it is WebView2 on Windows and WKWebView on macOS). The tray server bridges
the page's WebSocket to `Desktop.Stream`, the same way it proxies the
local APIs today. Then:

- `longarm desktop <w> --native` (and the tray's "desktop" button when
  the worker reports `kind: "native"`) opens it.
- Keys: `KeyboardEvent.code` → scan codes, so the worker's keyboard layout
  decides the characters, as on a local keyboard. While the canvas has
  focus, the Keyboard Lock API takes system keys (Win, Alt+Tab) in
  full screen. Text paste uses `Input{text}` (`KEYEVENTF_UNICODE`) for
  IMEs and Chinese.
- Clipboard text both ways (on focus / on change), scaling to fit or 1:1, a display picker,
  view-only, Ctrl+Alt+Del button, quality/bandwidth readout.
- The hub web panel can use the same page later, through the hub, the same
  way it reaches job terminals.

## Security

- **Who.** The worker's owner (`WorkerAccess.Owner`), and users it is
  shared with only if the owner turns on `desktop` for the share. An
  admin certificate does not get this automatically, because a desktop is
  more than a shell: it shows other people's screens.
- **Consent.** An `[desktop] consent = "never" | "when-someone-is-there"
  | "always"` setting, default `when-someone-is-there`: when the console
  session has had input in the last N minutes, the agent shows an
  Allow/Deny prompt on that screen (the UU Remote / Quick Assist model).
  Owners' unattended machines use `never`.
- **Visible.** While anyone is watching, there is a coloured border and a
  tray notice on the worker's screen ("alice is viewing - Stop"). Stop on
  the worker always wins.
- **Audit.** Start, end, who, view-only or control, and the consent answer
  (`longarm admin audit`). No recording in v1.
- **Per-user isolation.** The console belongs to whoever signed in at it,
  not to a Longarm space, so only the owner and admins get it there.

## macOS and Linux (later; same protocol, same viewer)

`internal/screen` keeps one interface (`Displays`, `Open → Next/Frame`,
input calls) with a file per OS.

- **macOS:** ScreenCaptureKit (`SCStream`, dirty rects in the frame
  info) + VideoToolbox H.264 + `CGEventPost` for input. Without cgo: through
  `ebitengine/purego`'s Objective-C runtime calls (already a dependency
  through Wails). longarmd needs Screen Recording and Accessibility
  permission, which macOS only grants interactively (or by MDM PPPC
  profile for fleets). The tray guides through it. Session mode
  (LaunchAgent) only: a LaunchDaemon cannot capture.
- **Linux X11:** MIT-SHM capture and XTest input over the X protocol in
  pure Go (`jezek/xgb`), XDamage for dirty rects. **Wayland:** only
  through xdg-desktop-portal ScreenCast + RemoteDesktop (D-Bus: `godbus`
  is already a dependency) and a PipeWire stream. PipeWire's protocol
  needs libpipewire or a pure-Go client, which does not exist yet: the
  hardest part. The fallback is todo/remote-desktop.md's
  gnome-remote-desktop/xrdp (RDP) plan. The portal asks the user once,
  and the restore token keeps it.
- Codecs there: VideoToolbox (macOS), VA-API/NVENC through
  GStreamer/ffmpeg only with cgo → JPEG tiles first on Linux.

## Plan

| Step | What | Size |
|---|---|---|
| 1 | `Desktop` gRPC service + JPEG tiles (parallel encode, PNG refresh) in session-mode longarmd; flow control; cursor overlay; tray viewer page + bridge; `longarm desktop --native`; owner-only; audit | ~2 weeks |
| 2 | Input polish: Unicode text, clipboard, multi-monitor, Keyboard Lock, view-only; consent prompt + "being viewed" border | ~1 week |
| 3 | Service mode: `desktop-agent` in the console session, secure desktop (UAC / lock screen / Ctrl+Alt+Del), sign-in with no one logged on | ~1 week |
| 4 | H.264 through Media Foundation + WebCodecs; bitrate from acks; JPEG fallback | ~1-2 weeks |
| 5 | macOS (ScreenCaptureKit, CGEvent, TCC onboarding in the tray) | ~2 weeks |
| 6 | Linux X11; Wayland through the portal (or RDP fallback) | ~2 weeks |

## Open questions

1. Is unattended control of one's own workers (no prompt) the main use, or
   helping someone at the screen (consent prompt first)? This decides
   step 2's default.
2. Does v1 need the secure desktop (UAC, lock screen), i.e. step 3 before
   H.264?
3. Should the viewer live in the tray only, or also in the hub web panel
   (browser access from a machine without Longarm installed)?
