package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func TestUpdateNoticeText(t *testing.T) {
	msi := &pb.WhoamiResponse{LatestVersion: "1.2.0", LatestPath: "/releases/1.2.0/longarm-windows-amd64.msi"}
	if got, want := updateNoticeText(msi, "1.1.0", "hub.example.com:7443"),
		"Longarm 1.2.0 is available: https://hub.example.com/releases/1.2.0/longarm-windows-amd64.msi (or https://hub.example.com/install.ps1)"; !strings.HasPrefix(got, want) {
		t.Fatalf("got %q", got)
	}
	pub := &pb.WhoamiResponse{LatestVersion: "1.2.0", LatestPath: "/install.sh", PublicUrl: "https://panel.example.com/"}
	if got := updateNoticeText(pub, "1.1.0", "hub:7443"); !strings.HasPrefix(got, "Longarm 1.2.0 is available: https://panel.example.com/install.sh") {
		t.Fatalf("got %q", got)
	}
	for _, c := range []struct {
		who     *pb.WhoamiResponse
		running string
	}{
		{msi, "1.2.0"}, {msi, "1.3.0"}, {msi, "dev"}, {&pb.WhoamiResponse{}, "1.0.0"}, {nil, "1.0.0"},
	} {
		if got := updateNoticeText(c.who, c.running, "hub:7443"); got != "" {
			t.Errorf("%v running %s: %q", c.who, c.running, got)
		}
	}
}

// TestUpdateNoticeOnceADay: one notice per day, and again for a newer version.
func TestUpdateNoticeOnceADay(t *testing.T) {
	saved := versionForNotice
	versionForNotice = func() string { return "1.0.0" }
	defer func() { versionForNotice = saved }()
	dir := t.TempDir()
	who := &pb.WhoamiResponse{LatestVersion: "1.2.0", LatestPath: "/install.sh"}
	now := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	maybeUpdateNotice(&out, dir, who, "hub:7443", now)
	if !strings.Contains(out.String(), "Longarm 1.2.0 is available") {
		t.Fatalf("first run: %q", out.String())
	}
	out.Reset()
	maybeUpdateNotice(&out, dir, who, "hub:7443", now.Add(time.Hour))
	if out.Len() != 0 {
		t.Fatalf("second notice within a day: %q", out.String())
	}
	maybeUpdateNotice(&out, dir, &pb.WhoamiResponse{LatestVersion: "1.3.0", LatestPath: "/install.sh"}, "hub:7443", now.Add(2*time.Hour))
	if !strings.Contains(out.String(), "1.3.0") {
		t.Fatalf("newer version: %q", out.String())
	}
	out.Reset()
	maybeUpdateNotice(&out, dir, &pb.WhoamiResponse{LatestVersion: "1.3.0", LatestPath: "/install.sh"}, "hub:7443", now.Add(27*time.Hour))
	if out.Len() == 0 {
		t.Fatal("no notice the next day")
	}
}
