//go:build windows

package screen

import (
	"testing"
	"time"
)

// TestClipboard: Set puts text on the clipboard without reporting it as a
// change; somebody else's copy is reported.
func TestClipboard(t *testing.T) {
	c, err := OpenClipboard()
	if err != nil {
		t.Skipf("no clipboard here: %v", err)
	}
	defer c.Close()
	wc := c.(*winClipboard)
	if err := c.Set("longarm clipboard 中文"); err != nil {
		t.Skipf("the clipboard is busy here: %v", err)
	}
	select {
	case got := <-c.Changes():
		t.Fatalf("our own Set came back as a change: %q", got)
	case <-time.After(300 * time.Millisecond):
	}
	if got, ok := wc.read(); !ok || got != "longarm clipboard 中文" {
		t.Fatalf("read back %q, %v", got, ok)
	}
	// Somebody else copies: written behind the watcher's back.
	wc.last = ""
	if err := wc.write("copied elsewhere"); err != nil {
		t.Fatal(err)
	}
	wc.last = ""
	select {
	case got := <-c.Changes():
		if got != "copied elsewhere" {
			t.Fatalf("change %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no change reported")
	}
}
