package telemetry

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Log attributes leave the machine only when structured by construction
// (review: a failed git fetch's error carried a credential URL).
func TestAttrValue(t *testing.T) {
	cases := []struct {
		key  string
		v    slog.Value
		want string
	}{
		{"reason", slog.StringValue(`git fetch https://git.example.net/opaqueCanaryCredential/repo.git: exit status 128`), "class:git"},
		{"err", slog.StringValue("dial tcp: connection refused"), "class:refused"},
		{"method", slog.StringValue("/longarm.v1.Jobs/Submit"), "/longarm.v1.Jobs/Submit"},
		{"method", slog.StringValue("has spaces in it"), "<omitted>"},
		{"argv", slog.StringValue("claude -p secret"), "<omitted>"},
		{"path", slog.StringValue(`C:\Users\bob\x`), "<omitted>"},
		{"n", slog.IntValue(3), "3"},
		{"took", slog.DurationValue(1500 * time.Millisecond), "1.5s"},
	}
	for _, c := range cases {
		if got := attrValue(c.key, c.v); got != c.want {
			t.Errorf("%s=%q: %q, want %q", c.key, c.v.String(), got, c.want)
		}
		if strings.Contains(attrValue(c.key, c.v), "Canary") {
			t.Fatal("credential leaked")
		}
	}
}

// `longarm telemetry off` reaches a process that is already running: it
// stops recording and drops what it buffered and queued.
func TestLiveOptOut(t *testing.T) {
	dir := t.TempDir()
	c := New(Config{Dir: dir, Identity: Identity{Node: "n1"}, Component: "longarm"})
	c.Event(t.Context(), "before", nil, nil)
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	c.Event(t.Context(), "buffered", nil, nil)
	if n, _ := c.Queue().Stat(); n == 0 {
		t.Fatal("nothing queued")
	}
	if err := SaveSettings(dir, Settings{Disabled: true}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.settingsAt = time.Time{} // don't wait for the next look
	c.mu.Unlock()
	if c.Enabled() {
		t.Fatal("still enabled after telemetry off")
	}
	c.Event(t.Context(), "after", nil, nil)
	c.Flush()
	if n, _ := c.Queue().Stat(); n != 0 {
		t.Fatalf("%d items queued after opting out", n)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) != 0 {
		t.Fatal("buffer kept")
	}
}
