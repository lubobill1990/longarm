package hub

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestInstallScriptChannel: GET /install.ps1?channel=dev (and preview, rc;
// install.sh alike) hands out that channel's newest release, and install.ps1
// has the MSI record the channel (UPDATE_CHANNEL) - also through its
// elevated relaunch; without a channel: the stable release, the channel
// recorded only with automatic updates; an unknown one: 400. Automatic
// updates (AUTO_UPDATE=1) only with ?autoupdate=1 or LONGARM_AUTO_UPDATE=1.
func TestInstallScriptChannel(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := InitCA(dir); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: dir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "off"}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	h, err := Open(cfg, slog.New(slog.NewTextHandler(&syncWriter{w: &bytes.Buffer{}}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	files := map[string]string{"longarm-windows-amd64.exe": "c", "longarmd-windows-amd64.exe": "d", "longarm-windows-amd64.msi": "msi"}
	for _, r := range [][2]string{{"0.1.20", ""}, {"0.1.21-rc.1", "beta"}, {"0.1.22-dev.9", "dev"}} {
		if _, err := addRelease(ctx, h.rel, h.store, releaseDir(t, files), r[0], r[1], "", false); err != nil {
			t.Fatal(r, err)
		}
	}
	get := func(kind, q string) (int, string) {
		w := httptest.NewRecorder()
		h.installScript(kind)(w, httptest.NewRequest(http.MethodGet, "/install."+kind+q, nil))
		return w.Code, w.Body.String()
	}
	for q, want := range map[string]string{"": "0.1.20", "?channel=stable": "0.1.20", "?channel=rc": "0.1.21-rc.1", "?channel=preview": "0.1.21-rc.1", "?channel=dev": "0.1.22-dev.9"} {
		code, body := get("ps1", q)
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("install.ps1%s: %d, want version %s", q, code, want)
			continue
		}
		follows := !strings.Contains(body, "if ($AutoUpdate -or '') { $props += 'UPDATE_CHANNEL=stable' }")
		if stable := want == "0.1.20"; follows == stable {
			t.Errorf("install.ps1%s: always records the machine's channel %v, want %v", q, follows, !stable)
		}
		if !strings.Contains(body, "[switch]$AutoUpdate = ($env:LONGARM_AUTO_UPDATE -eq '1')") {
			t.Errorf("install.ps1%s: automatic updates not off by default", q)
		}
		if _, sh := get("sh", q); !strings.Contains(sh, want) {
			t.Errorf("install.sh%s: want version %s", q, want)
		}
	}
	_, dev := get("ps1", "?channel=dev")
	if !strings.Contains(dev, "$props += 'UPDATE_CHANNEL=dev'") || !strings.Contains(dev, "Invoke-Elevated 'install.ps1?channel=dev'") {
		t.Errorf("install.ps1?channel=dev: the channel or the elevated relaunch's channel missing")
	}
	if code, _ := get("ps1", "?channel=nightly"); code != 400 {
		t.Errorf("an unknown channel: %d, want 400", code)
	}
	_, auto := get("ps1", "?channel=preview&autoupdate=1")
	if !strings.Contains(auto, "[switch]$AutoUpdate = ($true)") || !strings.Contains(auto, "$props += 'AUTO_UPDATE=1'") ||
		!strings.Contains(auto, "LONGARM_AUTO_UPDATE = $(if ($AutoUpdate)") || !strings.Contains(auto, "UPDATE_CHANNEL=preview") {
		t.Errorf("install.ps1?autoupdate=1: automatic updates not asked for (or not through the elevated relaunch)")
	}
	if code, _ := get("ps1", "?autoupdate=yes"); code != 400 {
		t.Errorf("autoupdate=yes: %d, want 400", code)
	}
}
