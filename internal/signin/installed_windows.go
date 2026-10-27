//go:build windows

package signin

import "golang.org/x/sys/windows/registry"

// InstalledHub is the hub the MSI recorded (HKLM\Software\Longarm: Hub,
// CaFingerprint; packaging/windows/longarm.wxs), "" when none.
func InstalledHub() (hub, fingerprint string) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Longarm`, registry.QUERY_VALUE)
	if err != nil {
		return "", ""
	}
	defer k.Close()
	hub, _, _ = k.GetStringValue("Hub")
	fingerprint, _, _ = k.GetStringValue("CaFingerprint")
	return hub, fingerprint
}
