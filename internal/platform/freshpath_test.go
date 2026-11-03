package platform

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestMergePath(t *testing.T) {
	for _, c := range []struct {
		name, current string
		registry      []string
		want          string
	}{
		{"registry first, then what only the daemon has",
			`C:\Windows;C:\old\tool;C:\Windows\System32`, []string{`C:\Windows\System32`, `C:\Windows`, `C:\Users\a\AppData\Local\Microsoft\WinGet\Links`},
			`C:\Windows\System32;C:\Windows;C:\Users\a\AppData\Local\Microsoft\WinGet\Links;C:\old\tool`},
		{"case and a trailing backslash: the same directory",
			`c:\windows\;C:\Go\bin`, []string{`C:\Windows`, `C:\GO\BIN\`},
			`C:\Windows;C:\GO\BIN\`},
		{"empty entries dropped, duplicates in the registry once",
			`;C:\a;;`, []string{`C:\b`, ``, `C:\b`, `C:\a`},
			`C:\b;C:\a`},
		{"a prefix the registry does not have stays in front",
			`C:\fake\bin;C:\Windows;C:\x`, []string{`C:\Windows`, `C:\new`},
			`C:\fake\bin;C:\Windows;C:\new;C:\x`},
		{"no registry entries: the daemon's PATH, cleaned",
			`C:\a;C:\a;C:\b`, nil, `C:\a;C:\b`},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := MergePath(c.current, c.registry, ";")
			if got != c.want {
				t.Fatalf("MergePath\n got %s\nwant %s", got, c.want)
			}
			if again := MergePath(got, c.registry, ";"); again != got {
				t.Fatalf("not idempotent:\n%s\n%s", got, again)
			}
		})
	}
}

func TestExpandWindowsEnv(t *testing.T) {
	vars := map[string]string{"USERPROFILE": `C:\Users\a`, "PNPM_HOME": `C:\Users\a\AppData\Local\pnpm`}
	lookup := func(n string) (string, bool) { v, ok := vars[strings.ToUpper(n)]; return v, ok }
	for in, want := range map[string]string{
		`%UserProfile%\go\bin`:      `C:\Users\a\go\bin`,
		`%PNPM_HOME%;%USERPROFILE%`: `C:\Users\a\AppData\Local\pnpm;C:\Users\a`,
		`%NOPE%\x`:                  `%NOPE%\x`,
		`100% sure %USERPROFILE%`:   `100% sure C:\Users\a`,
		`a%%b`:                      `a%%b`,
		`trailing %`:                `trailing %`,
		`plain`:                     `plain`,
		`%NOPE%USERPROFILE%`:        `%NOPEC:\Users\a`,
	} {
		if got := ExpandWindowsEnv(in, lookup); got != want {
			t.Errorf("ExpandWindowsEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRefreshPath: on Windows the registry's Path entries end up on PATH,
// a prefix of our own stays first; LONGARM_FRESH_PATH=0 turns it off.
func TestRefreshPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		RefreshPath() // a no-op
		return
	}
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
