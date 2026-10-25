package longarmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/transport"
)

// TestMigrateTailnetState: the pre-per-control "tsnet" dir goes to the
// listener that used it - a longarmd.toml tailscale listener when there is one
// (the hub's tailnet was not added then), else the hub's tailnet.
func TestMigrateTailnetState(t *testing.T) {
	const hub, own = "https://hs.example.com", "https://own.example.com"
	setup := func() Config {
		dir := t.TempDir()
		base := filepath.Join(dir, TSNetDir)
		os.MkdirAll(base, 0o700)
		os.WriteFile(filepath.Join(base, transport.JoinedMarker), []byte("2026-10-01T00:00:00Z"), 0o600)
		return Config{DataDir: dir}
	}
	ht := hubclient.TailnetConfig{ControlURL: hub, Hostname: "longarm-w"}

	c := setup() // hub-managed only
	migrateTailnetState(c, ht)
	if !transport.Joined(hubclient.TailnetStateDir(c.DataDir, hub)) {
		t.Fatal("hub tailnet state not migrated")
	}
	c = setup() // static leg on Tailscale's default: its state stays
	c.Listen = []string{"tcp://127.0.0.1:1", "tailscale://longarm-w"}
	migrateTailnetState(c, ht)
	if !transport.Joined(filepath.Join(c.DataDir, TSNetDir)) || transport.Joined(hubclient.TailnetStateDir(c.DataDir, hub)) {
		t.Fatal("static default-control state taken by the hub's tailnet")
	}
	c = setup() // static leg with its own control server
	c.Listen = []string{"tailscale://longarm-w?control=" + own}
	migrateTailnetState(c, ht)
	if !transport.Joined(transport.StateDirFor(filepath.Join(c.DataDir, TSNetDir), own)) {
		t.Fatal("static leg's state not moved to its control's dir")
	}
	c = setup() // static leg with its own dir: nothing moves
	c.Listen = []string{"tailscale://longarm-w?control=" + own + "&dir=/elsewhere"}
	migrateTailnetState(c, ht)
	if !transport.Joined(filepath.Join(c.DataDir, TSNetDir)) {
		t.Fatal("state moved for a listener with dir=")
	}
}
