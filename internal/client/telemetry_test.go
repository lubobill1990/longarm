package client

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lubobill1990/longarm/internal/telemetry"
)

func TestTelemetryConfigOptOut(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(telemetry.EnvOptOut, "")
	if _, off := TelemetryConfig(dir, "longarm"); off != "no hub" {
		t.Fatalf("no hub: %q", off)
	}
	os.WriteFile(filepath.Join(dir, "hub.toml"), []byte("address = \"hub.example:7443\"\n"), 0o600)
	cfg, off := TelemetryConfig(dir, "longarm")
	if off != "" || cfg.Disabled || cfg.Dir != filepath.Join(dir, TelemetryDir) {
		t.Fatalf("with a hub: %q %+v", off, cfg)
	}
	t.Setenv(telemetry.EnvOptOut, "0")
	if cfg, off := TelemetryConfig(dir, "longarm"); off == "" || !cfg.Disabled {
		t.Fatal("LONGARM_TELEMETRY=0 ignored")
	}
	t.Setenv(telemetry.EnvOptOut, "")
	telemetry.SaveSettings(filepath.Join(dir, TelemetryDir), telemetry.Settings{Disabled: true})
	if cfg, off := TelemetryConfig(dir, "longarm"); off == "" || !cfg.Disabled {
		t.Fatal("longarm telemetry off ignored")
	}
}

func TestErrClass(t *testing.T) {
	for in, want := range map[string]string{
		"dial tcp 10.0.0.1:7701: connectex: No connection could be made because the target machine actively refused it": "refused",
		"context deadline exceeded": "timeout", "tls: bad certificate": "tls", "something": "other",
	} {
		if got := ErrClass(in); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
}
