package longarmd

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func TestAvailableUpdate(t *testing.T) {
	why := errors.New(`C:\Program Files\Longarm is not writable for this account (package-managed install?)`)
	for _, c := range []struct {
		running, target string
		want            string
	}{
		{"1.0.0", "1.1.0", "1.1.0"},
		{"1.1.0", "1.1.0", ""},
		{"1.2.0", "1.1.0", ""},   // newer than the hub's
		{"1.0.0", "", ""},        // the hub has no release
		{"dev", "1.1.0", ""},     // a development build
		{"1.0.0", "not a v", ""}, // garbage from the hub
		{"1.0.0", "1.0.1-rc1", "1.0.1-rc1"},
	} {
		got := availableUpdate(c.running, c.target, why, "msi")
		if (got == nil) != (c.want == "") || (got != nil && (got.Version != c.want || got.Reason != why.Error())) {
			t.Errorf("availableUpdate(%q, %q) = %v, want %q", c.running, c.target, got, c.want)
		}
	}
}

// TestSetUpdateAvailable: the notice is part of the worker's state (local
// status, the hub's node list) and announced once per change.
func TestSetUpdateAvailable(t *testing.T) {
	a := &hubAgent{log: slog.New(slog.NewTextHandler(io.Discard, nil)), link: &pb.HubLink{}, watchers: map[chan *pb.LocalEvent]struct{}{}}
	ch, stop := a.watch()
	defer stop()
	out := make(chan *pb.NodeReport, 4)
	a.out = out
	ua := &pb.UpdateAvailable{Version: "1.1.0", Reason: "MSI install"}
	a.setUpdateAvailable(ua)
	a.setUpdateAvailable(&pb.UpdateAvailable{Version: "1.1.0", Reason: "MSI install"}) // no change
	if got := a.State().GetUpdateAvailable(); got.GetVersion() != "1.1.0" {
		t.Fatalf("state: %v", a.State())
	}
	if len(out) != 1 || (<-out).GetState().GetUpdateAvailable().GetVersion() != "1.1.0" {
		t.Fatalf("hub reports: %d", len(out))
	}
	if ev := <-ch; ev.Kind != "update" {
		t.Fatalf("event %v", ev)
	}
	a.setUpdateAvailable(nil) // updated meanwhile
	if a.State().UpdateAvailable != nil || len(out) != 1 || (<-out).GetState().UpdateAvailable != nil {
		t.Fatal("cleared notice not reported")
	}
}
