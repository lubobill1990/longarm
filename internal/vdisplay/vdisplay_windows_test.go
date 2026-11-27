//go:build windows

package vdisplay

import (
	"os"
	"testing"
)

// TestEnableDisable installs the driver, turns the display on and off
// again. It changes the machine (a driver, a display), so only with
// LONGARM_VDISPLAY_TEST=1, elevated; LONGARM_VDISPLAY_TEST=remove takes the
// driver away after.
func TestEnableDisable(t *testing.T) {
	v := os.Getenv("LONGARM_VDISPLAY_TEST")
	if v == "" {
		t.Skip("installs a display driver: LONGARM_VDISPLAY_TEST=1 (elevated)")
	}
	name, err := Enable(Mode{W: 1920, H: 1080})
	t.Logf("enable: %q %v", name, err)
	set, dev, ferr := findDevice()
	if ferr != nil || dev == nil {
		t.Fatalf("no device after enable: %v", ferr)
	}
	t.Logf("driver key %q, inf %q", driverKey(set, dev), oemInf(driverKey(set, dev)))
	set.Close()
	if err := Disable(); err != nil {
		t.Errorf("disable: %v", err)
	}
	if v == "remove" {
		if err := Remove(); err != nil {
			t.Errorf("remove: %v", err)
		}
		if set, dev, _ := findDevice(); dev != nil {
			set.Close()
			t.Error("the device is still there after remove")
		}
	}
}
