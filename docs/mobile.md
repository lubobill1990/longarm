# The Longarm phone app (design)

An iOS and Android app for the owner of Longarm workers: list your machines,
attach to their terminal sessions, see and use their desktops. TestFlight
(and a sideloaded APK) first; your own machines only.

## Decisions (2026-11-28)

- **Your own machines only.** The app lists the workers whose owner you are
  (`NodeInfo.owner` = `Whoami.user`); shared fleet workers and other
  people's are not shown. The worker side already allows only the owner a
  desktop (docs/remote-desktop.md).
- **Distribution: TestFlight first** (the maintainer's Apple Developer
  team; the app ID in the tree is a placeholder, `dev.longarm.app` - a
  release sets its own), an
  APK for Android. The App Store and Google Play later, if at all.
- **The Go core in the app, not a rewrite.** Everything hard - the hub
  sign-in and certificates, longnet (QUIC, NAT traversal, the hub relay),
  the gRPC protocol - is Go already; gomobile binds it (`mobile/core`). A
  phone runs no daemons (iOS allows no child processes), so the core runs
  in the app's process: `internal/mobile` does what `longarm daemon` and
  the tray do on a computer, as a library.
- **The UI: the web frontend in a Capacitor shell** for the machine list
  and terminals (React + xterm.js, as the tray), served to the WebView by
  the core on 127.0.0.1 with a per-start token.
- **The remote desktop: native**, not in the WebView: VideoToolbox into an
  `AVSampleBufferDisplayLayer` (iOS) and MediaCodec into a `SurfaceView`
  (Android) - hardware decoding, the lowest latency, native gestures, the
  hardware keyboard and the iPad pointer. The core hands the native view
  H.264 access units and the cursor through a gomobile callback and takes
  its input; it decodes nothing. Nothing changes on workers
  (`RemoteDesktop.Stream` as the desktop viewer uses it).

## Architecture

```
┌──────────────── the app (Capacitor: Swift / Kotlin shell) ────────────────┐
│ WebView: machines, sessions, terminal (xterm.js)                          │
│     │ http + ws://127.0.0.1:<port> (X-Longarm-Token / ?t=)                │
│ native desktop view ◄── frames, cursor ──┐                                 │
│                    ── touch, keys ──────►│                                 │
│ Go core (gomobile: longarm.aar / Longarm.xcframework):                     │
│   internal/mobile (API, sign-in, connections), client, hubclient, termws  │
└────────────────────────────┬───────────────────────────────────────────────┘
                             │ mTLS: QUIC (longnet) direct or via the hub relay, TCP
                  the hub (machine list) · workers' longarmd (sessions, desktop)
```

- **Sign-in**: `signin.SignIn` with the browser pairing (the tray's flow):
  the app shows the hub's link and a code, you sign in with your Microsoft
  account and approve; the phone gets a device certificate of its own,
  kept in the app's private storage (later: the key in the Keychain /
  Keystore).
- **API** (`internal/mobile`): `GET /api/status`, `POST|GET /api/signin`,
  `GET /api/machines`, `GET /api/machines/{w}/sessions`,
  `GET /api/machines/{w}/sessions/{name}/attach` (WebSocket,
  internal/termws - the daemon's terminal protocol).
- `go run ./internal/mobile/devserve -dir <config dir>` runs the core on a
  computer, for working on the UI.

## Phases

| Phase | What | State |
|---|---|---|
| 0. Spike | the Go core bound for Android and iOS, its size, longnet and the relay on a phone, sign-in | done (2026-11-28): Android emulator - machines from the hub, terminal attach to a Windows worker; iOS simulator - the core starts, the page loads (mobile.yml). Android core 12 MB compressed / 35 MB installed (arm64). Left for a device: the browser sign-in, longnet on mobile data |
| 1. MVP | the app shell, machines, terminal with a key bar (Esc, Tab, Ctrl, Alt, arrows); TestFlight, APK | |
| 2. Desktop | the native viewer: touch modes (trackpad, direct), zoom, keyboard, options; mobile data: lower fps and bitrate | |
| 3. Polish | Face ID / fingerprint lock, notifications (APNs/FCM through the hub), sound | |

## TestFlight

`mobile.yml`'s `testflight` job (pushes to main, manual runs) builds the
core for devices, archives the app signed for the App Store and uploads it
to App Store Connect; TestFlight then offers it to the team's testers. It
runs once the repository has:

- variables `LONGARM_IOS_APP_ID` (the real app id; the tree has the
  placeholder `dev.longarm.app`) and `APPLE_TEAM_ID`;
- secrets `APPLE_IOS_DIST_P12` / `APPLE_IOS_DIST_P12_PASSWORD` (an Apple
  Distribution certificate and key), `APPLE_IOS_PROFILE` (an App Store
  provisioning profile for the app id), and `APPLE_NOTARY_*` (an App Store
  Connect API key that may upload: the macOS notarization one).

The version is the last tag's x.y.z, the build number the run number.
Without them the job says so and does nothing. The app record in App Store
Connect is created by hand once (the API can't).

## Notes

- `mobile/` is its own Go module (`longarm/mobile`): gomobile needs
  `golang.org/x/mobile`, which the main module does not.
- gomobile builds with cgo (the NDK, Xcode's clang): purego (gopsutil on
  darwin, internal/screen on Linux) needs it there; the rest of Longarm
  stays `CGO_ENABLED=0`.
- gomobile's iOS start-up makes the app bundle the working directory;
  Capacitor's router resolves the empty path against it and served the
  `public` directory itself (a white screen, "The file "public" couldn't
  be opened"): the LongarmCore plugin sets it back to `/` in `load()`. The
  framework also needs `-lresolv` (Go's DNS resolver; the plugin's
  Package.swift).
- iOS suspends an app soon after it leaves the screen: connections end;
  the app reconnects when it comes back (no background connection).
