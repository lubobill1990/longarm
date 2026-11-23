//go:build darwin

package darwin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"longarm/internal/platform/ptypes"
)

// TestPlist: the rendered LaunchDaemon is a valid property list (plutil
// reads it back) with the keys longarmd relies on - UserName, the escaped
// arguments, AbandonProcessGroup, and ProcessType Interactive (without it
// launchd throttles longarmd and every job it starts).
func TestPlist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "com.longarm.longarmd.plist")
	cfg := ptypes.ServiceConfig{Name: "longarmd", Exe: "/usr/local/bin/longarmd",
		Args: []string{"run", "--data-dir", "/Users/alice/a&b <dir>"}, User: "alice"}
	if err := os.WriteFile(p, []byte(Plist(cfg)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("plutil", "-lint", p).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v: %s", err, out)
	}
	for key, want := range map[string]string{
		"Label":               "com.longarm.longarmd",
		"UserName":            "alice",
		"ProgramArguments.3":  "/Users/alice/a&b <dir>",
		"AbandonProcessGroup": "true",
		"KeepAlive":           "true",
		"ProcessType":         "Interactive",
	} {
		out, err := exec.Command("plutil", "-extract", key, "raw", "-o", "-", p).Output()
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
