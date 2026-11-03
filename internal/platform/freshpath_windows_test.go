//go:build windows

package platform

import (
	"os"
	"strings"
	"testing"
)

// restartFresh forgets what NoteStartPath noted, as for a longarmd
// starting now, and the registry's Path read by registry (nil: the real
// one) until the test ends.
func restartFresh(t *testing.T, registry func() ([]string, error)) {
	t.Helper()
	freshMu.Lock()
	startNoted, startPrefix = false, nil
	if registry != nil {
		registryPath = registry
	}
	freshMu.Unlock()
	t.Cleanup(func() {
		freshMu.Lock()
		startNoted, startPrefix, registryPath = false, nil, RegistryPath
		freshMu.Unlock()
	})
}

// TestRefreshPath: the registry's Path entries end up on PATH,
// a prefix of our own stays first; LONGARM_FRESH_PATH=0 turns it off.
func TestRefreshPath(t *testing.T) {
	reg, err := RegistryPath()
	if err != nil || len(reg) == 0 {
		t.Skipf("no registry Path: %v", err)
	}
	mine := t.TempDir()
	t.Setenv("PATH", mine)
	restartFresh(t, nil)
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

// TestRefreshPathUpgrade: an installer replaced the registry's first Path
// entry since longarmd started; the next job gets the new one first, the
// old one (still on longarmd's PATH) after the registry's.
func TestRefreshPathUpgrade(t *testing.T) {
	reg := []string{`C:\old-go\bin`, `C:\Windows`}
	t.Setenv("LONGARM_FRESH_PATH", "")
	t.Setenv("PATH", `C:\fake\bin;C:\old-go\bin;C:\Windows`)
	restartFresh(t, func() ([]string, error) { return reg, nil })
	NoteStartPath() // longarmd starts
	reg = []string{`C:\new-go\bin`, `C:\Windows`}
	RefreshPath()
	if got, want := os.Getenv("PATH"), `C:\fake\bin;C:\new-go\bin;C:\Windows;C:\old-go\bin`; got != want {
		t.Fatalf("PATH\n got %s\nwant %s", got, want)
	}
}
