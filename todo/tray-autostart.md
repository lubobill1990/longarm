# The tray's "Start Longarm at login"

## The MSI's uninstall leaves a user's own Run value behind

A user who turned the tray's autostart off on an MSI install (HKLM's
StartupApproved: disabled for everybody) and on again gets an HKCU Run
value `longarm-tray` of their own (internal/tray/autostart.go
runKeyAutostart.Set). The MSI's uninstall removes HKLM's value only, so
that user's value stays and names a missing longarm-tray.exe at the next
logon (Windows skips it silently; Task Manager lists it). install.ps1's
uninstall removes the installing user's value; the MSI's could do the
same for every profile in the uninstall step of longarm-msihelper
(packaging/windows/longarm.wxs, LongarmUninstall*), or the tray could
re-enable HKLM's StartupApproved value (elevated) instead of writing its
own when the MSI's value exists.

## macOS: a LaunchAgent switched off in System Settings reads as on

macOS 13+ lets the user switch a LaunchAgent off in System Settings >
General > Login Items (Background Task Management); the plist stays, so
fileAutostart.State says on. Read `launchctl print-disabled gui/<uid>`
(or SMAppService.statusForLegacyURL) in autostart_darwin.go.
