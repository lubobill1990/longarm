// Package vdisplay adds a virtual display to a Windows worker for Longarm's
// remote desktop (docs/usage.md "Virtual display"): a machine without a
// monitor (a VM, a Cloud PC) gets a screen of the viewer's size, or a
// second one. It uses Virtual-Display-Driver (github.com/VirtualDrivers,
// MIT; driver/), an Indirect Display (IddCx) driver: user mode, so its
// publisher's signature is enough - unlike a kernel driver it needs no
// Microsoft signature, and loads with Secure Boot on.
//
// The driver is installed the first time a viewer asks (SYSTEM: the
// Longarm Desktop service's agent) and stays installed, disabled while no
// viewer uses it; uninstalling Longarm removes it.
package vdisplay

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsupported: not Windows.
var ErrUnsupported = errors.New("virtual displays need Windows")

// Mode is the virtual display's size.
type Mode struct{ W, H int }

// Enable installs the driver if needed and turns the virtual display on at
// m; it returns the display's device name (\\.\DISPLAYn), which
// screen.Displays lists.
func Enable(m Mode) (string, error) { return enable(m) }

// Disable turns the virtual display off (the driver stays installed).
func Disable() error { return disable() }

// Remove uninstalls the driver and its device (Longarm's uninstall).
func Remove() error { return remove() }

// Supported: this machine can have one (Windows 10 2004+ with IddCx).
func Supported() bool { return supported() }

// settingsXML is the driver's vdd_settings.xml for one monitor of mode m:
// m first (Windows picks it), then smaller common sizes to fall back on;
// 60 Hz; the pointer as a hardware cursor (the capture sends its shape).
func settingsXML(m Mode) string {
	modes := []Mode{m}
	for _, f := range []Mode{{1920, 1080}, {1280, 720}} {
		if f.W < m.W || f.H < m.H {
			modes = append(modes, f)
		}
	}
	var b strings.Builder
	b.WriteString("<?xml version='1.0' encoding='utf-8'?>\n<vdd_settings>\n")
	b.WriteString("  <monitors><count>1</count></monitors>\n  <gpu><friendlyname>default</friendlyname></gpu>\n")
	b.WriteString("  <global><g_refresh_rate>60</g_refresh_rate></global>\n  <resolutions>\n")
	for _, r := range modes {
		fmt.Fprintf(&b, "    <resolution><width>%d</width><height>%d</height><refresh_rate>60</refresh_rate></resolution>\n", r.W, r.H)
	}
	b.WriteString("  </resolutions>\n  <options>\n    <CustomEdid>false</CustomEdid>\n    <PreventSpoof>false</PreventSpoof>\n")
	b.WriteString("    <EdidCeaOverride>false</EdidCeaOverride>\n    <HardwareCursor>true</HardwareCursor>\n    <SDR10bit>false</SDR10bit>\n")
	b.WriteString("    <HDRPlus>false</HDRPlus>\n    <logging>false</logging>\n    <debuglogging>false</debuglogging>\n  </options>\n</vdd_settings>\n")
	return b.String()
}

// clamp keeps a viewer's size to what the driver offers sensibly: even,
// 640x480 to 7680x4320.
func clamp(m Mode) Mode {
	m.W = min(max(m.W, 640), 7680) &^ 1
	m.H = min(max(m.H, 480), 4320) &^ 1
	return m
}
