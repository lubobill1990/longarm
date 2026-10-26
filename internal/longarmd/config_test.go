package longarmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Invalid [hub] labels of an older longarmd.toml are dropped with a warning, not
// fatal (longarmd, longarmd leave and the uninstaller must still start).
func TestLoadConfigDropsInvalidLabels(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ConfigFile), []byte("[hub]\nlabels = { region = \"eu,os=windows\", gpu = \"RTX 4090\", pool = \"big\" }\n"), 0o600)
	cfg, err := LoadConfig(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Hub.Labels) != 1 || cfg.Hub.Labels["pool"] != "big" {
		t.Fatalf("labels %v", cfg.Hub.Labels)
	}
	if len(cfg.Warnings) != 2 || !strings.Contains(strings.Join(cfg.Warnings, "\n"), "ignored") {
		t.Fatalf("warnings %q", cfg.Warnings)
	}
}

// [links] max_conns: 64 by default, -1 = no cap (0 inside), other
// negatives are an error.
func TestLoadConfigLinkMaxConns(t *testing.T) {
	for in, want := range map[string]int{"": DefaultLinkMaxConns, "[links]\nmax_conns = 8\n": 8, "[links]\nmax_conns = -1\n": 0, "[links]\nmax_conns = -2\n": -2} {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, ConfigFile), []byte(in), 0o600)
		cfg, err := LoadConfig(dir, nil)
		if want < 0 {
			if err == nil {
				t.Errorf("%q: no error", in)
			}
			continue
		}
		if err != nil || cfg.Links.MaxConns != want {
			t.Errorf("%q: %d %v, want %d", in, cfg.Links.MaxConns, err, want)
		}
	}
}
