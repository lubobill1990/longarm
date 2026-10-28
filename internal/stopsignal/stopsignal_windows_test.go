//go:build windows

package stopsignal

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestSignal: the process's own event cancels its context; a pid without
// one reports false (the stopper then ends it by pid).
func TestSignal(t *testing.T) {
	ctx, cancel := NotifyContext(context.Background())
	defer cancel()
	ok, err := Signal(os.Getpid())
	if err != nil || !ok {
		t.Fatalf("Signal(self) = %v, %v", ok, err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the stop event did not cancel the context")
	}
	if ok, err := Signal(0x7ffffff0); ok || err != nil {
		t.Errorf("Signal(no such pid) = %v, %v", ok, err)
	}
}

// TestNameTaken: an event made by someone else under this process's name
// is not used (it could be set by them).
func TestNameTaken(t *testing.T) {
	h, err := create(os.Getpid() + 1000000)
	if err != nil {
		t.Fatal(err)
	}
	defer closeHandle(h)
	if _, err := create(os.Getpid() + 1000000); err == nil {
		t.Error("an existing event was taken over")
	}
}
