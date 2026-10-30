# Sessions window (docs/agent-sessions-ui.md)

## Focusing an open Sessions window on macOS and Linux

`POST /trayapi/sessions/open` on an open window sets `location.hash`
through Wails' `ExecJS` (`cmd/longarm-tray/shell_wails.go`). It is
verified on Windows only, where Wails injects its runtime into every page;
if the hash doesn't change on macOS / Linux, switch to
`SetURL(srv.SessionsURL(hash, false))` (only the hash changes, the page
doesn't reload).

## Remember the Sessions window's size and position

The window always opens at 1200×760. The popup already remembers its
position (`tray-window.json` in the client config dir: `windowPrefsFile`,
`internal/tray/roles.go`); store the Sessions window's bounds the same way
and restore them in `shell_wails.go`.

## A test for the Sessions window's options

`shell_wails_test.go` has `TestPopupIsAnApp` for the popup; add the same
for `sessionsOptions` (normal frame, not always-on-top, not hidden on
focus loss, resizable).
