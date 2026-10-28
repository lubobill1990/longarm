//go:build !windows

package longarmd

var machineSettings = &memSettings{}

// MachineSettings: the MSI is Windows-only (in memory elsewhere).
func MachineSettings() SettingsStore { return machineSettings }
