//go:build windows

package longarmd

import "golang.org/x/sys/windows/registry"

// regSettings is HKLM\Software\Longarm (administrators and SYSTEM only can
// write it).
type regSettings struct{}

// MachineSettings is the store of the MSI's recorded settings.
func MachineSettings() SettingsStore { return regSettings{} }

func (regSettings) Get(n string) (string, bool) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Longarm`, registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(n)
	return v, err == nil
}

func (regSettings) Set(n, v string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `Software\Longarm`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(n, v)
}

func (regSettings) Delete(n string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software\Longarm`, registry.SET_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	if err := k.DeleteValue(n); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}
