//go:build windows

package longarmd

import (
	"os"

	"golang.org/x/sys/windows"

	"github.com/lubobill1990/longarm/internal/platform"
)

func currentDataDirEnv() dataDirEnv {
	home, _ := os.UserHomeDir()
	pd, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	if err != nil {
		pd = os.Getenv("ProgramData")
	}
	system := isLocalSystem()
	return dataDirEnv{
		getenv:   os.Getenv,
		home:     home,
		system:   system,
		elevated: !system && platform.Elevated(),
		sysDir:   systemDataDirFor("windows", os.Getenv, pd),
		inUse:    DataDirInUse,
	}
}

// isLocalSystem reports whether this process runs as LocalSystem (the
// per-user service, the MSI's custom actions).
func isLocalSystem() bool {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	return u.User.Sid.IsWellKnown(windows.WinLocalSystemSid)
}
