# macOS .pkg

`build-pkg.sh` (on a Mac) makes a universal (arm64 + x86_64) installer from
GoReleaser's darwin builds: `longarm` and `longarmd` in `/usr/local/bin`, codesigned
with the hardened runtime, the package signed with a *Developer ID
Installer* certificate, notarized with `notarytool` and stapled.

Unattended setup: write `/Library/Preferences/com.longarm` (Roles, Hub,
JoinCode, User, CAFingerprint) before `installer -pkg`; see
`scripts/postinstall`. Without it the pkg only installs the binaries.

Firewall: the postinstall adds longarm/longarmd to the application firewall
(`socketfilterfw --add/--unblockapp`) when it is enabled. That is the macOS
equivalent of the Windows inbound rules: without it, unsolicited inbound
UDP (hole punching, tailscale direct) is dropped.

Uninstall: `sudo longarmd service uninstall; longarmd leave --kill-jobs; sudo rm /usr/local/bin/longarm /usr/local/bin/longarmd; sudo pkgutil --forget com.longarm.pkg`.

Not built or tested here (needs a Mac and the Apple developer account).
