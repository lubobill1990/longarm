package transport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
)

func TestStateDirFor(t *testing.T) {
	for _, c := range []struct{ control, want string }{
		{"", "tsnet"},
		{"https://controlplane.tailscale.com", "tsnet-controlplane.tailscale.com"},
		{"https://HS.Example.com/", "tsnet-hs.example.com"},
		{"http://127.0.0.1:8080", "tsnet-127.0.0.1-8080"},
		{"https://example.com/hs/", "tsnet-example.com-hs"},
		{"https://[::1]:443", "tsnet-1--443"},
	} {
		if got := StateDirFor("tsnet", c.control); got != c.want {
			t.Errorf("StateDirFor(%q) = %q, want %q", c.control, got, c.want)
		}
	}
	if !SameControl("https://HS.example.com/", "https://hs.example.com") || SameControl("https://a", "https://b") ||
		!SameControl("", "") || SameControl("", "https://a") {
		t.Error("SameControl")
	}
}

func TestMigrateStateDir(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "tsnet")
	const hs = "https://hs.example.com"
	// P2 layout: hub Headscale state in "tsnet", key next to it.
	os.MkdirAll(base, 0o700)
	os.WriteFile(filepath.Join(base, JoinedMarker), []byte("2026-10-01T00:00:00Z"), 0o600)
	os.WriteFile(filepath.Join(base, "tailscaled.state"), []byte("{}"), 0o600)
	MigrateStateDir(base, hs)
	dst := StateDirFor(base, hs)
	if !Joined(dst) || Joined(base) {
		t.Fatal("state not moved")
	}
	if _, err := os.Stat(filepath.Join(dst, "tailscaled.state")); err != nil {
		t.Fatal(err)
	}
	if d := StateDirs(base); len(d) != 1 || d[0] != dst {
		t.Fatalf("StateDirs %v", d)
	}
	// A key waiting in the old place moves along; an existing dst wins.
	os.WriteFile(base+".authkey", []byte("k"), 0o600)
	MigrateStateDir(base, "https://other.example.com")
	if b, _ := os.ReadFile(StateDirFor(base, "https://other.example.com") + ".authkey"); string(b) != "k" {
		t.Fatalf("key not moved: %q", b)
	}
	// State of another control server (recorded in the marker) stays.
	os.MkdirAll(base, 0o700)
	os.WriteFile(filepath.Join(base, JoinedMarker), []byte("2026-10-01T00:00:00Z\nhttps://controlplane.tailscale.com\n"), 0o600)
	if JoinedControl(base) != "https://controlplane.tailscale.com" {
		t.Fatalf("JoinedControl %q", JoinedControl(base))
	}
	MigrateStateDir(base, "https://third.example.com")
	if !Joined(base) {
		t.Fatal("state of another control server moved")
	}
	if err := WipeState(dst, dst+".authkey"); err != nil || Joined(dst) {
		t.Fatalf("wipe: %v", err)
	}
	if fi, err := os.Stat(dst); err != nil || !fi.IsDir() {
		t.Fatal("wipe must leave an empty dir")
	}
}

type fakeStatus struct {
	mu sync.Mutex
	st ipnstate.Status
}

func (f *fakeStatus) set(state, authURL string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.BackendState, f.st.AuthURL = state, authURL
}

func (f *fakeStatus) StatusWithoutPeers(context.Context) (*ipnstate.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.st
	return &st, nil
}

func TestWatchLogin(t *testing.T) {
	old := needsLoginAfter
	needsLoginAfter = 700 * time.Millisecond
	defer func() { needsLoginAfter = old }()
	run := func(fromState bool, fs *fakeStatus, d time.Duration) error {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		go watchLogin(ctx, fs, fromState, cancel)
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(d):
			return nil
		}
	}
	// A node from state sent back to NeedsLogin: logged out.
	fs := &fakeStatus{}
	fs.set("NeedsLogin", "")
	if err := run(true, fs, 3*time.Second); !errors.Is(err, ErrNeedsLogin) {
		t.Fatalf("from state: %v", err)
	}
	// A fresh node with a key registers through NeedsLogin: not a failure.
	if err := run(false, fs, 1500*time.Millisecond); err != nil {
		t.Fatalf("fresh node: %v", err)
	}
	// An interactive login URL: at once, either way.
	fs.set("NeedsLogin", "https://login.example.com/a/123")
	start := time.Now()
	if err := run(false, fs, 3*time.Second); !errors.Is(err, ErrNeedsLogin) || time.Since(start) > time.Second {
		t.Fatalf("auth URL: %v after %s", err, time.Since(start))
	}
	// Starting / Running never trip it.
	fs.set("Starting", "")
	if err := run(true, fs, 1500*time.Millisecond); err != nil {
		t.Fatalf("starting: %v", err)
	}
}
