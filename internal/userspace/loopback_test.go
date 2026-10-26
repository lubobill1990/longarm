package userspace

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/loopguard"
	"github.com/lubobill1990/longarm/internal/platform"
)

// TestLoopbackLockedExtra: an account about to be used is in the rules
// before it is in a space; "require" turns a failure into an error,
// "isolate" into a warning.
func TestLoopbackLockedExtra(t *testing.T) {
	var got []loopguard.User
	fail := false
	applyLoopback = func(u []loopguard.User) error {
		got = u
		if fail {
			return errors.New("nft: not found")
		}
		return nil
	}
	t.Cleanup(func() { applyLoopback = loopguard.Apply })
	s := &Spaces{log: slog.New(slog.NewTextHandler(io.Discard, nil)), loopback: loopguard.Require, users: map[string]*Space{
		"alice": {Owner: "alice", Account: &platform.Account{Name: "la-alice", UID: 1001}},
		"gone":  {Owner: "gone"},
	}}
	if err := s.loopbackLocked(&platform.Account{Name: "la-bob", UID: 1002}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("rules for %v, want la-alice and la-bob", got)
	}
	fail = true
	if err := s.loopbackLocked(); err == nil || !strings.Contains(err.Error(), "require") {
		t.Fatalf("require: %v", err)
	}
	s.loopback = loopguard.Isolate
	if err := s.loopbackLocked(); err != nil {
		t.Fatalf("isolate: %v", err)
	}
	s.loopback = ""
	got = nil
	if err := s.loopbackLocked(); err != nil || got != nil {
		t.Fatalf("off: %v %v", got, err)
	}
}
