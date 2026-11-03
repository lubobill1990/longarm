# todo: `longarm ui open` and the worker's fresh PATH

## `ui open` while an older tray runs

Only one tray runs per user (Wails' single instance). A tray from before
`ui open` has no control channel (`tray.addr`): `ui open` starts the new
longarm-tray, which exits as a second instance and makes the old one show
its popup at its default tab; `ui open` gives up after 30 s with "quit it
and try again". A newer tray could also take the page from a second
instance's arguments (`longarm-tray --open <page>`, read in
`OnSecondInstanceLaunch`, `cmd/longarm-tray/shell_wails.go`) - it helps
for the next upgrade, not this one; until then the message is the fix.

## Switch the popup's tab without a reload

`ShowPopup` loads the popup anew at `/tray?tab=..` with a fresh one-time
code (`SetURL`, `cmd/longarm-tray/shell_wails.go`): simple and the same on
every platform, but the page reloads (a moment of skeletons). The popup
could follow a hash or a message instead, as the Sessions window does
(`hashchange`).

## Fresh variables besides PATH

`platform.RefreshPath` (`internal/platform/freshpath_windows.go`) rebuilds
only PATH. Installers also add variables (`PNPM_HOME`, `JAVA_HOME`,
`NVM_HOME`, `GOPATH`): they are used to expand the registry's Path, but a
job doesn't get them until longarmd restarts. Adding registry variables
that longarmd's environment lacks (never overriding one it has, or the
job's own) would close that; Explorer's rule is to take the registry's.
