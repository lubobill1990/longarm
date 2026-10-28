//go:build windows

package longarmd

import (
	"golang.org/x/sys/windows/registry"

	"github.com/lubobill1990/longarm/internal/platform"
)

// ProvenanceLocation is where the provenance values live.
const ProvenanceLocation = `HKLM\Software\Longarm\Provenance`

const provenanceKey = `Software\Longarm\Provenance`

// regProvenance keeps the values in HKLM (administrators and SYSTEM only can
// write it; readable by everybody, holds no secret).
type regProvenance struct{}

func newProvenance() ProvenanceStore { return regProvenance{} }

func (regProvenance) Get(n string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, provenanceKey, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, _ := k.GetStringValue(n)
	return v
}

func (regProvenance) Set(n, v string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, provenanceKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(n, v)
}

func (regProvenance) Delete(n string) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, provenanceKey, registry.SET_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	if err := k.DeleteValue(n); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

func trustedSystemPath(dir string, also ...string) error {
	return platform.TrustedSystemPath(dir, also...)
}
