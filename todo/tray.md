# todo: the tray's popup (longarm-tray)

## The popup keeps its size from another display scale

The popup is created once, hidden and shown again (`popupOptions`, 460 x
780; `desktop.show`, `cmd/longarm-tray/shell_wails.go`): `show` positions
it near the tray icon but never sizes it. A popup created under another
display scale keeps that scale's pixel size - opened during a remote
desktop session at 150 % and later shown on the local 100 % screen it is
690 px wide, everything stretched across it (seen with v0.1.32-dev).
Quitting and starting the tray fixes it until the next scale change.
`show` (when the user hasn't moved the window: `placed`) could set the
size again in the current monitor's scale before `PositionWindow`, or the
window could follow `WM_DPICHANGED` / Wails' DPI events; check it with a
remote desktop session at another scale, then the console.
