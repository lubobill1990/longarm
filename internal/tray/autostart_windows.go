package tray

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// newAutostart: the Run key; the tray's own exe (resolved, as the MSI's
// value names it).
func newAutostart() autostarter {
	exe, _ := os.Executable()
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	reg := filepath.Join(os.Getenv("SystemRoot"), "System32", "reg.exe")
	return &runKeyAutostart{reg: winRunKeys{}, exe: exe, disableMachine: func() error {
		return RunElevatedWait(reg, machineDisableArgs()...)
	}}
}

// winRunKeys is the real registry (64-bit view).
type winRunKeys struct{}

func (winRunKeys) open(machine bool, key string, access uint32) (registry.Key, error) {
	root := registry.CURRENT_USER
	if machine {
		root = registry.LOCAL_MACHINE
	}
	return registry.OpenKey(root, key, access|registry.WOW64_64KEY)
}

func missing(err error) bool {
	return errors.Is(err, registry.ErrNotExist) || errors.Is(err, os.ErrNotExist)
}

func (r winRunKeys) String(machine bool, key, name string) (string, bool, error) {
	k, err := r.open(machine, key, registry.QUERY_VALUE)
	if missing(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer k.Close()
	v, _, err := k.GetStringValue(name)
	if missing(err) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (r winRunKeys) Binary(machine bool, key, name string) ([]byte, bool, error) {
	k, err := r.open(machine, key, registry.QUERY_VALUE)
	if missing(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer k.Close()
	v, _, err := k.GetBinaryValue(name)
	if missing(err) {
		return nil, false, nil
	}
	return v, err == nil, err
}

func (r winRunKeys) SetString(key, name, value string) error {
	// a fresh profile may have no Run key yet
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.SET_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, value)
}

func (r winRunKeys) Delete(key, name string) error {
	k, err := r.open(false, key, registry.SET_VALUE)
	if missing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue(name); err != nil && !missing(err) {
		return err
	}
	return nil
}
