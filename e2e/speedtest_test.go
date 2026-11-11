package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSpeedtest: longarm speedtest measures both ways between this machine
// and a worker, and between two workers (the path longarm cp takes), and
// keeps the result; put and cp then refuse a large transfer a slow relayed
// measurement says would take long (exit 75) unless --force.
func TestSpeedtest(t *testing.T) {
	h := newHandoffEnv(t)
	type entry struct {
		From, To           string
		UpMbps, DownMbps   float64
		UpBytes, DownBytes int64
		Path               string
	}
	run := func(args ...string) entry {
		t.Helper()
		r := h.client(h.root, append([]string{"speedtest", "--json", "--duration", "500ms"}, args...)...)
		if r.code != 0 {
			t.Fatalf("speedtest %v: exit %d\n%s\n%s", args, r.code, r.stdout, r.stderr)
		}
		var e entry
		if err := json.Unmarshal([]byte(r.stdout), &e); err != nil {
			t.Fatalf("speedtest %v: %v\n%s", args, err, r.stdout)
		}
		if e.UpBytes <= 0 || e.DownBytes <= 0 || e.UpMbps <= 0 || e.DownMbps <= 0 || e.Path == "" {
			t.Errorf("speedtest %v: %+v", args, e)
		}
		return e
	}
	if e := run("w1"); e.From != "local" || e.To != "w1" {
		t.Errorf("to w1: %+v", e)
	}
	if e := run("w1", "w2"); e.From != "w1" || e.To != "w2" {
		t.Errorf("w1 to w2: %+v", e)
	}
	if r := h.client(h.root, "speedtest", "w1", "--duration", "300ms"); r.code != 0 || !strings.Contains(r.stdout, "this machine -> w1:") {
		t.Errorf("text: exit %d\n%s%s", r.code, r.stdout, r.stderr)
	}
	cache := filepath.Join(h.e.client, "speed-cache.json")
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("no cache: %v", err)
	}

	// a slow relayed measurement: 4 MiB would take ~34 s at 1 Mbit/s
	slow := func(from, to string) map[string]any {
		return map[string]any{"from": from, "to": to, "upMbps": 1.0, "downMbps": 1.0, "upBytes": 1, "downBytes": 1, "path": "relay", "at": time.Now()}
	}
	b, _ := json.Marshal(map[string]any{"local|w1": slow("local", "w1"), "w1|w2": slow("w1", "w2")})
	if err := os.WriteFile(cache, b, 0o600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(h.root, "big.bin")
	os.WriteFile(local, bytes.Repeat([]byte("x"), 4<<20), 0o644)
	dst := "w1:" + filepath.Join(h.w1home, "big.bin")
	if r := h.client(h.root, "put", local, dst); r.code != 75 || !strings.Contains(r.stderr, "--force") {
		t.Errorf("put over a slow path: exit %d\n%s", r.code, r.stderr)
	}
	if r := h.client(h.root, "put", "--force", local, dst); r.code != 0 {
		t.Fatalf("put --force: exit %d\n%s", r.code, r.stderr)
	}
	if r := h.client(h.root, "cp", "w1:"+filepath.Join(h.w1home, "big.bin"), "w2:"+filepath.Join(h.w2home, "big.bin")); r.code != 75 {
		t.Errorf("cp over a slow path: exit %d\n%s", r.code, r.stderr)
	}
	// small files always go
	small := filepath.Join(h.root, "small.txt")
	os.WriteFile(small, []byte("small"), 0o644)
	if r := h.client(h.root, "put", small, "w1:"+filepath.Join(h.w1home, "small.txt")); r.code != 0 {
		t.Errorf("a small put: exit %d\n%s", r.code, r.stderr)
	}
}
