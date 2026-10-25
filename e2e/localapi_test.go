package e2e

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/localapi"
)

// TestLocalAPI: longarmd's and longarm daemon's local control API: CLI (gRPC) and the
// JSON gateway over the same user-only channel, drain persisted across a
// restart, and the channel's access control.
func TestLocalAPI(t *testing.T) {
	e := newEnv(t)
	e.startLongarmd()
	addrFile := filepath.Join(e.dataDir, "longarmd.local")
	addr, err := localapi.ReadAddr(addrFile)
	if err != nil {
		t.Fatal(err)
	}
	checkLocalACL(t, addr)

	st := longarmdCmd(t, "status", "--data-dir", e.dataDir)
	if st.code != 0 || !strings.Contains(st.stdout, "worker      w1") || !strings.Contains(st.stdout, "hub         not configured") {
		t.Fatalf("longarmd status: %+v", st)
	}
	hc := localapi.HTTPClient(addr)
	resp, err := hc.Get("http://local/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(compactJSON(string(b)), `"name":"w1"`) {
		t.Fatalf("gateway status: %s", b)
	}
	resp, err = hc.Post("http://local/v1/drain", "application/json", strings.NewReader(`{"draining":true,"reason":"via gateway"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(compactJSON(string(b)), `"draining":true`) {
		t.Fatalf("gateway drain: %s", b)
	}
	// Persisted: still draining after a restart.
	e.killLongarmd()
	e.startLongarmd()
	if st := longarmdCmd(t, "status", "--data-dir", e.dataDir); !strings.Contains(st.stdout, "draining: via gateway") {
		t.Fatalf("drain not persisted: %+v", st)
	}
	if st := longarmdCmd(t, "undrain", "--data-dir", e.dataDir); st.code != 0 || !strings.Contains(st.stdout, "accepting work") {
		t.Fatalf("undrain: %+v", st)
	}
	// Events stream (SSE through the gateway): a job shows up.
	ev, err := hc.Get("http://local/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer ev.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(ev.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	id := e.run(bins.helper, "print", "1", "0", "0")
	deadline := time.After(20 * time.Second)
	for seen := false; !seen; {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("event stream ended")
			}
			seen = strings.Contains(l, id)
		case <-deadline:
			t.Fatal("no job event on /v1/events")
		}
	}

	// longarm daemon.
	if r := e.longarm(e.client, "daemon", "start"); r.code != 0 {
		t.Fatalf("daemon start: %+v", r)
	}
	t.Cleanup(func() { e.longarm(e.client, "daemon", "stop") })
	daddr, err := localapi.ReadAddr(filepath.Join(e.client, "daemon.addr"))
	if err != nil {
		t.Fatal(err)
	}
	checkLocalACL(t, daddr)
	if r := e.longarm(e.client, "daemon", "status"); r.code != 0 || !strings.Contains(r.stdout, "client/alice") {
		t.Fatalf("daemon status: %+v", r)
	}
	resp, err = localapi.HTTPClient(daddr).Get("http://local/v1/forwards")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("daemon gateway: %d", resp.StatusCode)
	}
	port := startEchoServer(t, e.root)
	r := e.longarm(e.client, "forward", "w1", strconv.Itoa(port), "0", "--bg")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "http://127.0.0.1:") {
		t.Fatalf("forward --bg: %+v", r)
	}
	if r := e.longarm(e.client, "forward", "--stop", "all"); r.code != 0 || !strings.Contains(r.stdout, "(daemon)") {
		t.Fatalf("forward --stop: %+v", r)
	}
	if r := e.longarm(e.client, "daemon", "stop"); r.code != 0 {
		t.Fatalf("daemon stop: %+v", r)
	}
	waitFor(t, "daemon gone", 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(e.client, "daemon.addr"))
		return err != nil
	})
}
