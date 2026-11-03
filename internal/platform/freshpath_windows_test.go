//go:build windows

package platform

import (
	"os"
	"strings"
	"testing"
)

// TestRefreshPath: the registry's Path entries end up on PATH,
// a prefix of our own stays first; LONGARM_FRESH_PATH=0 turns it off.
func TestRefreshPath(t *testing.T) {
	reg, err := RegistryPath()
	if err != nil || len(reg) == 0 {
		t.Skipf("no registry Path: %v", err)
	}
	mine := t.TempDir()
	t.Setenv("PATH", mine)
	t.Setenv("LONGARM_FRESH_PATH", "0")
	RefreshPath()
	if os.Getenv("PATH") != mine {
		t.Fatalf("LONGARM_FRESH_PATH=0 changed PATH: %s", os.Getenv("PATH"))
	}
	t.Setenv("LONGARM_FRESH_PATH", "")
	RefreshPath()
	got := strings.Split(os.Getenv("PATH"), ";")
	if got[0] != mine || !strings.EqualFold(got[1], strings.TrimSpace(reg[0])) {
		t.Fatalf("PATH = %v (registry %v)", got, reg)
	}
}
