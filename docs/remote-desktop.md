# Native remote desktop (design)

Status: Windows workers and viewers are in (`RemoteDesktop` in longarmd,
`internal/rdesk`, `internal/screen`, `longarm desktop`; e2e/desktop_test.go).
The Longarm Desktop service (UAC, lock screen, service workers:
`internal/desktopsvc`, `longarmd desktop-service`, the MSI) is in too. Not
yet: macOS and Linux. The spike measurements below are kept for the record (the
spike itself is gone; its code became `internal/screen`).

## Decisions (2026-11-26)

- **Owner only.** It is for controlling your own workers. Nobody else
  gets it: not shared users, not admins of a per-user isolation worker.
  There is no consent prompt. The "being viewed" indicator stays,
  because it is cheap and tells someone at the machine what is happening.
- **Video, not image tiles.** H.264 (Media Foundation, hardware
  encoder first) is the main path, for compression and smoothness. JPEG
  tiles remain only as the fallback where no encoder or decoder exists.
- **UAC prompts and the lock screen must work.** So a SYSTEM desktop
  agent in the console session is required from the first release, in
  every install mode (session-mode workers too). See "Worker: where
  capture runs".
- **A separate "Longarm Desktop" service** (`LongarmDesktop`,
  LocalSystem, installed with the worker role, removed with it). It is
  not part of the Longarm Update service. It keeps the capturer running
  as SYSTEM in the console session (`StartInSession`), following session
  changes, and it calls `SendSAS` itself, because Windows only takes
  Ctrl+Alt+Del from session 0 (see "Secure desktop" below). The install
  sets the `SoftwareSASGeneration` policy to 1 (services) and uninstall
  removes it.
- **Viewer: a native window, opened from the tray.** "Desktop" in the
  tray runs `longarm desktop <worker>`, which opens a native window (not
  a WebView page): on Windows, GPU H.264 decoding (Media Foundation +
  DXVA), drawn with the D3D11 video processor into a flip-model swap
  chain, and a low-level keyboard hook so Win, Alt+Tab and Ctrl+Esc reach
  the worker. It is resizable, and Ctrl+Alt+Enter toggles full screen
  (Ctrl+Alt+End sends Ctrl+Alt+Del). There is no hub web panel viewer,
  and no WebSocket bridge in the tray. The CLI already has the client's
  certificate and connection to workers, so it talks gRPC to longarmd
  directly. Why native rather than WebView: a page cannot take system
  keys (Keyboard Lock is not dependable in WebView2), and the spike
  measured it 10-20 ms slower ("Native viewer" below). The cost is one
  viewer per OS (macOS: VideoToolbox + Metal/CALayer through purego;
  Linux: later).

Where the sections below offer alternatives, these decisions take precedence.

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

### H.264 (Media Foundation, `internal/screen/h264_windows.go`)

`spike h264bench`, `spike scrollbench` (the captured screen scrolled
8 rows a frame: heavy motion without touching the desktop),
`spike serve -codec h264`.

| | GPU (NVENC, async MFT) | Software MFT |
|---|---|---|
| Opens from pure Go | yes, High profile `avc1.640033` | yes |
| BGRA→NV12 (BT.709, parallel strips, 24 threads) | 1.1 ms | 1.1 ms |
| Ordinary desktop use | ~8 frames/s, **0.98 Mbit/s** | 0.99 Mbit/s |
| Scroll at 60 fps, 1440p, 8 Mbit/s CBR target | **5.3 Mbit/s**, NV12 + encode p50 6 ms, p95 12 ms | 7.8 Mbit/s, p50 5 ms |
| Same scroll as full-frame JPEG q70 | 206 Mbit/s | |
| First keyframe | 152 KB | 317 KB |
| Viewer | WebCodecs `VideoDecoder` in Edge 157 (WebView2's engine), Annex B chunks, `optimizeForLatency`: decodes and draws the live desktop with correct colours | |

The whole loop needs no cgo and no third-party code. With motion, H.264
uses about 40× less bandwidth than JPEG frames, and about 5× less on an
ordinary desktop. A still screen costs nothing: frames only come
when it changes, plus two refresh frames so text sharpens once it stops.

### Native viewer (`internal/screen/{present,window}_windows.go`)

`spike serve -tcp 127.0.0.1:7789` + `spike view [-control] [-snapshot f.png]`.
A Win32 window, decoding with Media Foundation's H.264 decoder given a
DXGI device manager (DXVA: NV12 surfaces stay in video memory) and
drawing with `VideoProcessorBlt` (NV12→RGB, BT.709 studio range, scaled
and letterboxed) into a flip-discard swap chain. There are no shaders.
The keyboard hook and full screen are in `Window`. The window's own back
buffer, read back (`-snapshot`), shows the desktop correctly scaled.

Both viewers on one machine, so one clock; latency is from the capture
on the worker side to the viewer having the frame:

| Viewer | Capture → | p50 | p95 |
|---|---|---|---|
| Native window | `Present` returned | 6.5-8 ms | 12-13 ms |
| WebView (Edge 157 = WebView2's engine), WebSocket + WebCodecs | `drawImage` to the canvas (the compositor adds up to a frame after this) | 17.5 ms | 18 ms |

Both include the NVENC encode (~6 ms). The native window is 10-20 ms
quicker end to end, and it is the only one that can take system keys.

### Secure desktop (`internal/screen/session_windows.go`, a Cloud PC)

`spike agent` stood in for the Longarm Desktop service. It ran as SYSTEM
in session 0 (a one-off scheduled task) and started `spike serve` as
SYSTEM in the console session. The test machine was a Windows 365 Cloud PC
worker with no GPU (Hyper-V Video), signed in at the console as a
standard user. The viewer here was `spike view` through
`longarm forward`, and `spike poke` sent the test input.

| Test | Result |
|---|---|
| Ordinary desktop, 1024×768, software H.264 encoder | shown |
| Ctrl+Alt+Del (`SendSAS`) from the server in the console session | **nothing**: Windows ignores it there |
| `SendSAS` from SYSTEM in session 0, with `SoftwareSASGeneration` = 1 | the Ctrl+Alt+Del screen (Winlogon desktop) is shown |
| A click on its Cancel | back to the desktop: mouse input works on the Winlogon desktop |
| A UAC prompt (the credential prompt on the secure desktop) | shown |
| Esc | dismissed: keyboard input works on the secure desktop |

Capture and input follow the input desktop: the capturer reopens on
ACCESS_LOST, and the input thread calls `FollowInputDesktop` before every
event. The lock screen is the same Winlogon desktop as the Ctrl+Alt+Del
screen. It was not locked on purpose, because nobody could have unlocked
that machine's test user.

## Architecture

```
longarm desktop <worker>   (native window; the tray's "Desktop" runs it)
   │ gRPC Desktop.Stream over the client's node connection (mTLS, direct or relay)
   ▼
longarmd (worker) ─┬─ session mode: captures in-process (no secure desktop)
                   └─ the SYSTEM desktop agent in the console session
                      (named pipe): UAC, lock screen, Ctrl+Alt+Del
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

1. **H.264** through Media Foundation (the spike): the hardware encoder
   MFTs (NVENC/Quick Sync/AMF) or Microsoft's software encoder. Both are
   part of Windows and COM like DXGI, so Go needs no cgo. Low-latency mode,
   CBR at the bandwidth the link measures, a keyframe on request. The
   viewer decodes on its GPU (Media Foundation + DXVA). There is no
   openh264: Cisco's patent cover only applies to its own downloaded binary.
2. **JPEG dirty tiles** (the first spike), only where H.264 is
   unavailable: N editions without the Media Feature Pack.
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

A native window from `longarm desktop <worker>` (the tray's "Desktop"
button runs it, as its ssh buttons run `longarm ssh`). It has no tray
page and no WebSocket.

- **Picture:** `Presenter` (GPU decode, video processor, swap chain).
  The pointer is drawn by the viewer from the worker's cursor shape and
  position. The capture leaves it out, so moving it costs no video.
- **Keys:** scan codes from the low-level keyboard hook while the window
  is in front, so the worker's keyboard layout decides the characters, as
  on a local keyboard. Win, Alt+Tab and Ctrl+Esc go to the worker.
  Ctrl+Alt+Enter toggles full screen. Ctrl+Alt+End sends Ctrl+Alt+Del
  through the SYSTEM agent's `SendSAS`. When the window loses focus,
  every key held on the other side is released. Text paste uses
  `Input{text}` (`KEYEVENTF_UNICODE`) for IMEs and Chinese.
- **Mouse:** absolute positions mapped through the letterbox, buttons,
  wheel.
- Also: clipboard text both ways, a display picker, the bandwidth and
  latency in the title, and a reconnect that keeps the window.

## Security

- **Who.** Only the worker's owner (`WorkerAccess.Owner`), from their own
  devices. Not users it is shared with, and not an admin certificate. A
  worker that is not owned (an admin's pool machine) refuses the desktop.
- **No consent prompt** (it is your own machine). There is a visible
  indicator: while connected, the worker's screen has a thin coloured
  border and the tray says "Viewed from <device> - Disconnect". Disconnect
  on the worker always wins.
- **Audit.** Start, end, and from which device (`longarm admin audit
  --node <worker>`). No recording.
- **Per-user isolation workers:** the owner only, as everywhere.

## macOS and Linux (later; same protocol)

`internal/screen` keeps one interface (`Displays`, `Open → Next/Frame`,
input calls, `Presenter`, `Window`) with a file per OS.

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
- **Viewers there:** macOS gets an NSWindow with a CAMetalLayer, and
  VideoToolbox decoding into IOSurfaces (purego). Taking system keys
  needs a CGEventTap and Accessibility permission. Linux gets X11/Wayland
  windows later. Until then, Windows is the viewing side.

## Plan

| Step | What | Size |
|---|---|---|
| 1 | `Desktop` gRPC service in longarmd; H.264 (GPU MFT, software fallback) with flow control (≤3 unacked frames, bitrate from acks, keyframe on request); cursor shape and position; owner-only + audit; `longarm desktop <w>` opens the native window (decode, present, mouse, keys, hook, full screen); the tray's button | ~2 weeks |
| 2 | The Longarm Desktop service (LocalSystem, installed with the worker role, MSI + `longarmd desktop-service install` / `uninstall`): keeps the capturer running as SYSTEM in the console session and follows session changes; the input desktop (UAC prompt, lock screen, sign-in screen); `SendSAS` from the service for Ctrl+Alt+Del, with the `SoftwareSASGeneration` policy set at install; talks to longarmd over a named pipe only SYSTEM and longarmd's account may open | ~1-2 weeks |
| 3 | Clipboard, Unicode text, display picker, the "viewed" border, reconnect | ~1 week |
| 4 | macOS worker (ScreenCaptureKit, VideoToolbox, CGEvent; TCC onboarding in the tray) and macOS viewer (VideoToolbox + Metal/CALayer window) | ~3 weeks |
| 5 | Linux X11 worker; Wayland through the portal (or the RDP fallback); a Linux viewer | ~3 weeks |

