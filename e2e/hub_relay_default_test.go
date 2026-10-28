package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHubSignInWorkerRelay: a worker signed in with longarmd login (pairing)
// that listens on loopback only - what every sign-in install does (the MSI's
// session and admin workers, the tray) - also listens on the hub's longnet
// relay, which the hub announces in its welcome; nothing is configured. The
// hub reports the longnet endpoint, and a client on "another machine" (only
// the hub-reported endpoints that aren't loopback) runs exec through it,
// with longnet's hole punching taking the direct path (loopback here).
func TestHubSignInWorkerRelay(t *testing.T) {
	t.Parallel()
	a := newFakeAuthority(t)
	h := newHubEnv(t, a.hubConfig(""))
	erinWeb := entraPanel(t, h, a, "e7e70000-0000-4000-8000-000000000002", "erin@example.com")

	w := h.newWorkerDir(t, "rw", "") // listen = ["tcp://127.0.0.1:<port>"] only
	p := startProc(t, exec.Command(bins.longarmd, "login", h.addr(), "--data-dir", w.dataDir, "--ca-fingerprint", h.fp, "--name", "rw"))
	_, code := p.code(t)
	if out := erinWeb.decide(code, "approve"); !strings.Contains(out, "worker rw is yours") {
		t.Fatalf("approve worker: %s", out)
	}
	if c := p.wait(t); c != 0 {
		t.Fatalf("longarmd login: %d\n%s", c, p.out.String())
	}
	w.start(t, true)
	n := h.waitNode(t, "rw", "online with the hub relay leg", func(n map[string]any) bool {
		return isTrue(n["online"]) && strings.Contains(fmt.Sprint(n["endpoints"]), "longnet://")
	})

	// What another machine can use: the reported endpoints minus loopback tcp.
	eps, _ := n["endpoints"].([]any)
	var remote []string
	for _, e := range eps {
		s := fmt.Sprint(e)
		if strings.HasPrefix(s, "tcp://127.") || strings.HasPrefix(s, "tcp://[::1]") {
			continue
		}
		remote = append(remote, s)
	}
	want := fmt.Sprintf("longnet://%s?name=rw", h.addr()) // the test hub's relay shares its port
	if len(remote) != 1 || remote[0] != want {
		t.Fatalf("endpoints %v: want only %s beyond loopback", eps, want)
	}
	if log, _ := os.ReadFile(w.logPath); !strings.Contains(string(log), "listening on the hub's relay") {
		t.Fatalf("no relay leg in the worker's log:\n%s", tail(string(log), 4000))
	}

	// erin's client device; the worker as another machine sees it: longnet only
	erin := filepath.Join(h.root, "erin")
	cmd := exec.Command(bins.longarm, "login", h.addr(), "--ca-fingerprint", h.fp)
	cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+erin, "LONGARM_AZ="+filepath.Join(h.root, "no-az"))
	p = startProc(t, cmd)
	_, code = p.code(t)
	if out := erinWeb.decide(code, "approve"); !strings.Contains(out, "signed in as erin (client)") {
		t.Fatalf("approve client: %s", out)
	}
	if c := p.wait(t); c != 0 {
		t.Fatalf("longarm login: %d\n%s", c, p.out.String())
	}
	relayAddr := strings.TrimPrefix(strings.SplitN(remote[0], "?", 2)[0], "longnet://")
	f, err := os.OpenFile(filepath.Join(erin, "workers.toml"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "\n[[worker]]\nname = \"rw-remote\"\ncert_name = \"rw\"\ntransport = \"longnet\"\naddress = %q\n", relayAddr)
	f.Close()

	r := h.longarm(erin, "--transport", "longnet", "exec", "rw-remote", "--", bins.helper, "print", "3", "0", "0")
	if r.code != 0 || r.stdout != lines(1, 3) {
		t.Fatalf("exec through the hub relay leg: %+v", r)
	}
	if info := h.mustLongarm(erin, "--transport", "longnet", "info", "rw-remote"); !strings.Contains(info, "path: longnet/") {
		t.Fatalf("info:\n%s", info)
	}
	// Hole punching: on loopback the direct path comes up (the worker logs it).
	r = h.longarm(erin, "--transport", "longnet", "exec", "rw-remote", "--", bins.helper, "print", "2000", "0", "0", "1000")
	if r.code != 0 || r.stdout != lines(1, 2000) {
		t.Fatalf("exec: %d %d bytes %s", r.code, len(r.stdout), r.stderr)
	}
	waitFor(t, "a direct longnet path", 20*time.Second, func() bool {
		log, _ := os.ReadFile(w.logPath)
		return strings.Contains(string(log), "longnet: direct path up")
	})
}
