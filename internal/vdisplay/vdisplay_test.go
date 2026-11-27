package vdisplay

import (
	"strings"
	"testing"
)

// TestSettingsXML: the viewer's size comes first (Windows picks it), with
// smaller fallbacks; sizes are clamped and even.
func TestSettingsXML(t *testing.T) {
	x := settingsXML(clamp(Mode{2561, 1601}))
	first := strings.Index(x, "<width>2560</width><height>1600</height>")
	if first < 0 || first > strings.Index(x, "<width>1920</width>") {
		t.Fatalf("2560x1600 not first:\n%s", x)
	}
	if !strings.Contains(x, "<count>1</count>") || !strings.Contains(x, "<HardwareCursor>true</HardwareCursor>") {
		t.Fatalf("settings:\n%s", x)
	}
	if got := clamp(Mode{100, 100000}); got != (Mode{640, 4320}) {
		t.Errorf("clamp: %v", got)
	}
	if x := settingsXML(Mode{1280, 720}); strings.Contains(x, "<width>1920</width>") {
		t.Errorf("a larger fallback offered:\n%s", x)
	}
}
