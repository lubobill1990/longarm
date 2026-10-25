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
