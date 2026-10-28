//go:build !windows

package longarmd

// InstalledWorker: only the Windows MSI records a run mode and an admin
// worker.
func InstalledWorker() (runMode string, admin AdminInstall) { return "", AdminInstall{} }

// RecordAdminDisabled: Windows only.
func RecordAdminDisabled() error { return nil }
