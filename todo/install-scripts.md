# Install scripts (the hub's install.ps1 / install.sh / uninstall.ps1)

## uninstall.sh for Linux and macOS

`uninstall.ps1` is install.ps1's counterpart on Windows; install.sh has
none. Linux packages come off with `apt remove` / `dnf remove`, but an
install.sh without a package (a binary install in `--prefix`, the systemd
unit or launchd daemon, `~/.longarm`, `~/.config/longarm`) is removed by
hand (docs/usage.md "Uninstall", Linux / macOS). Start from `shTmpl` and
`uninstallPs1Tmpl` in internal/hub/install.go; serve it as
`GET /uninstall.sh` next to `/uninstall.ps1` (internal/hub/http.go) and
extend `TestInstallScriptsLogin` (`sh -n`).

## The elevation is tested by hand only

install.ps1 / uninstall.ps1 from a PowerShell that isn't elevated relaunch
themselves elevated (`Invoke-Elevated`, UAC). A UAC prompt can't be answered
unattended, so CI runs the account-only install and the uninstall
(`TestInstallPs1AccountOnlyAndUninstall`, CI runners are elevated) but not
the relaunch: try it by hand on a machine that may lose its Longarm
(`irm https://<hub>/install.ps1 | iex` from a normal PowerShell, approve,
then `irm https://<hub>/uninstall.ps1 | iex`).

## install.sh on a machine signed in already

install.ps1 asks whether to replace the sign-in when `longarmd login`
exits 80 (`signin.ExitSignedIn`), then signs in again with `--force`.
install.sh still stops there: run as `curl ... | sh` its stdin is the
script, so a prompt would have to read `/dev/tty` (and `longarmd login`
opens the browser at once for the same reason - its stdin isn't a
terminal). Ask through `/dev/tty` when there is one; keep the sign-in
without.

## An unattended install without a code in its parameters

The MSI no longer rolls back when its `JOIN_CODE` fails (an expired code:
the worker waits for a sign-in). An unattended install still has to carry
the code in its parameters, though - a Dev Box customization's parameters
can be read by everyone who can read the customization group, and the task
may run days later, after a short-lived code expired. A way without a code
there: a `JOIN_URL` / one-time token the task exchanges for a code when it
runs, or a longer-lived, single-use "pre-approved node name" enrollment the
hub accepts once (`longarm worker add <name> --preapprove --ttl 7d`).
Start from `longarmd.JoinFailed` (internal/longarmd/leave.go) and the
hub's join codes (internal/hub/enroll.go).
