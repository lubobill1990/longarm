package longarmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/supervisor"
	"github.com/lubobill1990/longarm/internal/telemetry"
)

func TestTelemetryOptOut(t *testing.T) {
	dir := t.TempDir()
	b := &pki.Bundle{Identity: pki.Identity{Name: "w-tel", Role: pki.RoleWorker}}
	cfg, err := LoadConfig(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if startTelemetry(cfg, b, false) != nil {
		t.Fatal("telemetry without a hub")
	}
	w := startTelemetry(cfg, b, true)
	if w == nil || !w.client().Enabled() {
		t.Fatal("telemetry off by default with a hub")
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte("[telemetry]\nenabled = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = LoadConfig(dir, nil); err != nil {
		t.Fatal(err)
	}
	if startTelemetry(cfg, b, true) != nil {
		t.Fatal("[telemetry] enabled = false ignored")
	}
	var off *workerTelemetry // every method of an off worker telemetry is a no-op
	off.event("x", nil)
	off.jobChanged(supervisorJob(), false)
	off.client().Event(context.Background(), "x", nil, nil)
	telemetry.SetDefault(nil)
}

func TestExitClass(t *testing.T) {
	i := func(n int) *int { return &n }
	for want, code := range map[string]*int{"unknown": nil, "ok": i(0), "error": i(2), "not_found": i(127), "signal": i(137), "large": i(-1073741510)} {
		if got := exitClass(code); got != want {
			t.Errorf("%v: %s, want %s", code, got, want)
		}
	}
}

func supervisorJob() supervisor.Job { return supervisor.Job{} }
