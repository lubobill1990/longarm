//go:build windows

package signin

import (
	"golang.org/x/sys/windows/registry"

	"github.com/lubobill1990/longarm/internal/sched"
)

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

// InstalledLabels are the worker labels the MSI recorded (HKLM\Software\Longarm:
// Labels, "k=v,k2=v2" from LABELS); nil when none or invalid.
func InstalledLabels() map[string]string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Longarm`, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	s, _, _ := k.GetStringValue("Labels")
	if s == "" {
		return nil
	}
	m, err := sched.ParseLabels([]string{s}, false)
	if err != nil {
		return nil
	}
	return m
}

// InstalledRoles is the MSI's ROLES (HKLM\Software\Longarm: Roles):
// "client", "worker", "both", or "" when none was recorded.
func InstalledRoles() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Longarm`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	r, _, _ := k.GetStringValue("Roles")
	return r
}
