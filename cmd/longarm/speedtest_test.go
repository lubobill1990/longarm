package main

import (
	"errors"
	"testing"
	"time"
)

// TestGuardTransfer: small transfers and direct paths always go; a fresh
// slow relayed measurement refuses what would take over 30 s (exit 75,
// --force overrides); unmeasured, only a large transfer over a path known
// to be relayed is refused; a measurement either way round counts.
func TestGuardTransfer(t *testing.T) {
	dir := t.TempDir()
	code := func(err error) int {
		var ce *codedError
		if errors.As(err, &ce) {
			return ce.code
		}
		if err != nil {
			return -1
		}
		return 0
	}
	for _, c := range []struct {
		name string
		n    int64
		path string
		want int
	}{
		{"small", 512 << 10, "relay", 0},
		{"unmeasured direct", 10 << 30, "direct", 0},
		{"unmeasured unknown path", 10 << 30, "", 0},
		{"unmeasured relayed, under 100 MiB", 50 << 20, "relay", 0},
		{"unmeasured relayed, large", 200 << 20, "derp", exitTempFail},
	} {
		if got := code(guardTransfer(dir, "put", "local", "w1", c.n, c.path, false)); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	// measured: 10 Mbit/s over the relay up, 100 down
	saveSpeed(dir, speedEntry{From: "local", To: "w1", UpMbps: 10, DownMbps: 100, Path: "relay", At: time.Now()})
	if err := guardTransfer(dir, "put", "local", "w1", 20<<20, "relay", false); err != nil { // ~17 s
		t.Errorf("20 MiB at 10 Mbit/s: %v", err)
	}
	if code(guardTransfer(dir, "put", "local", "w1", 100<<20, "relay", false)) != exitTempFail { // ~84 s
		t.Error("100 MiB at 10 Mbit/s went")
	}
	if err := guardTransfer(dir, "put", "local", "w1", 100<<20, "relay", true); err != nil {
		t.Errorf("--force: %v", err)
	}
	// get is the other way round: 100 Mbit/s, ~8 s
	if err := guardTransfer(dir, "get", "w1", "local", 100<<20, "relay", false); err != nil {
		t.Errorf("get 100 MiB at 100 Mbit/s: %v", err)
	}
	// a direct measurement never refuses
	saveSpeed(dir, speedEntry{From: "w1", To: "w2", UpMbps: 1, DownMbps: 1, Path: "direct", At: time.Now()})
	if err := guardTransfer(dir, "cp", "w1", "w2", 1<<30, "", false); err != nil {
		t.Errorf("direct: %v", err)
	}
	// a stale measurement is not used
	saveSpeed(dir, speedEntry{From: "w3", To: "w4", UpMbps: 1, Path: "relay", At: time.Now().Add(-time.Hour)})
	if err := guardTransfer(dir, "cp", "w3", "w4", 1<<30, "", false); err != nil {
		t.Errorf("stale: %v", err)
	}
	if e, ok := cachedSpeed(dir, "w2", "w1"); !ok || e.From != "w2" || e.To != "w1" {
		t.Errorf("reversed: %+v %v", e, ok)
	}
}
