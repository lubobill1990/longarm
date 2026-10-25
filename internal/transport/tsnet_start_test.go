package transport

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTSNetBackgroundStart: a dial that gives up doesn't abort the node's
// start (the next dial may find it up); Close cancels the start.
func TestTSNetBackgroundStart(t *testing.T) {
	ts := NewTSNet(TSNetConfig{Hostname: "longarm-bg-test", Dir: filepath.Join(t.TempDir(), "tsnet"),
		ControlURL: "http://127.0.0.1:1", AuthKey: "tskey-auth-unused"}) // nobody answers: the node stays starting
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := ts.Dial(ctx, "100.64.0.1:7701")
	if err == nil || !strings.Contains(err.Error(), "still starting") || time.Since(start) > 3*time.Second {
		t.Fatalf("dial: %v after %s", err, time.Since(start))
	}
	time.Sleep(200 * time.Millisecond)
	if !ts.Starting() || ts.Started() {
		t.Fatal("the start did not go on after the dial gave up")
	}
	ts.Close()
	deadline := time.Now().Add(30 * time.Second)
	for ts.Starting() {
		if time.Now().After(deadline) {
			t.Fatal("Close did not cancel the start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, _, err := ts.Dial(context.Background(), "100.64.0.1:7701"); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("dial after Close: %v", err)
	}
}
