//go:build windows

package windows

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestLockedDACL(t *testing.T) {
	for sddl, want := range map[string]bool{
		"O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)": true,
		TraverseSDDL("BU"):                          true,
		"O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;S-1-5-21-1-2-3-1001)":   true, // a service account
		"O:BAD:PAI(A;OICI;FA;;;SY)(D;OICI;FA;;;WD)":                                    true,
		"O:BAD:AI(A;OICIID;FA;;;SY)(A;OICIID;FA;;;BA)":                                 false, // inherited
		"O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;WD)":                                    false,
		"O:BAD:PAI(A;OICI;FA;;;SY)(A;CI;0x100116;;;BU)":                                false, // ProgramData's "Users may create"
		"O:BAD:PAI(A;OICI;FA;;;SY)(A;OICIIO;FA;;;CO)":                                  false,
		"O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;AU)":                              false,
		"O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;;0x1000a0;;;S-1-5-21-1-2-3-1001)": true,
	} {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatalf("%s: %v", sddl, err)
		}
		if got, why := LockedDACL(sd); got != want {
			t.Errorf("%s: locked %v (%s), want %v", sddl, got, why, want)
		}
	}
}
