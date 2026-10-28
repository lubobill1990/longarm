//go:build windows

package longarmd

import (
	"golang.org/x/sys/windows/registry"
)

// InstalledWorker is what the MSI recorded about this machine's worker
// (HKLM\Software\Longarm): RunMode "session" (the user-session worker),
// "service" or "" (no worker / no MSI), and the admin worker.
func InstalledWorker() (runMode string, admin AdminInstall) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Longarm`, registry.QUERY_VALUE)
	if err != nil {
		return "", AdminInstall{}
	}
	defer k.Close()
	get := func(n string) string { s, _, _ := k.GetStringValue(n); return s }
	runMode = get("RunMode")
	if runMode == "" && get("ServiceAccount") != "" {
		runMode = "service" // an MSI from before user-session mode
	}
	admin = AdminInstall{Enabled: get("AdminWorker") == "1", Service: get("AdminServiceName"),
		DataDir: get("AdminDataDir"), Handoff: get("AdminHandoff")}
	if admin.Service == "" {
		admin.Service = AdminServiceName
	}
	return runMode, admin
}

// RecordAdminDisabled notes in the registry that the admin worker was
// disabled (elevated; the MSI keeps it disabled on upgrades).
func RecordAdminDisabled() error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Longarm`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("AdminWorker", "0")
}
