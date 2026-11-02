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
