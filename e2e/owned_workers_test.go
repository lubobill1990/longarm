package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/tenancy"
)

// TestOwnedWorkers: alice registers a worker of her own (longarm worker
// add -> longarmd join): it joins approved and serves alice only - bob does
// not see it, cannot use it directly or by name, and placement never puts
// his jobs there - until an admin lets alice share and she shares it with
// bob; unsharing locks him out again.
func TestOwnedWorkers(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	alice, root := h.client, h.admin
	h.mustLongarm(root, "admin", "user", "add", "alice", "--no-code")
	h.mustLongarm(root, "admin", "user", "add", "bob", "--no-code")
	bob := filepath.Join(h.root, "cfg-bob")
	h.mustLongarm(alice, "admin", "issue", "--role", "client", "--name", "bob-laptop", "--user", "bob",
		"--out", filepath.Join(bob, "tls"), "--ca-dir", filepath.Join(alice, "ca"))
	os.WriteFile(filepath.Join(bob, "workers.toml"), nil, 0o644)
	os.WriteFile(filepath.Join(bob, "hub.toml"), []byte(fmt.Sprintf("address = %q\n", h.addr())), 0o600)

	// a fleet worker (registered by the admin) for everybody
	wf := h.newWorkerDir(t, "wf", "")
	h.joinWorker(t, wf, true)
	wf.start(t, true)

	// alice's own worker
	r := h.longarm(alice, "worker", "add", "aw", "--label", "kind=mine")
	code := strings.TrimSpace(r.stdout)
	if r.code != 0 || code == "" || !strings.Contains(r.stderr, "longarmd join "+h.addr()+" "+code+" --ca-fingerprint "+h.fp+" --name aw") {
		t.Fatalf("worker add: %+v", r)
	}
	aw := h.newWorkerDir(t, "aw", "")
	j := longarmdCmd(t, "join", h.addr(), code, "--data-dir", aw.dataDir, "--ca-fingerprint", h.fp)
	if j.code != 0 || !strings.Contains(j.stdout, "joined as worker aw") || !strings.Contains(j.stdout, "owned by alice") {
		t.Fatalf("longarmd join: %+v", j)
	}
	aw.start(t, true)
	for _, w := range []string{"wf", "aw"} {
		h.waitNode(t, w, "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	}
	if n := h.waitNode(t, "aw", "owned", func(n map[string]any) bool { return n["owner"] == "alice" }); n["state"] != "approved" {
		t.Fatalf("aw: %v", n)
	}
	accessFile := filepath.Join(aw.dataDir, tenancy.WorkerAccessFile)
	waitAccess := func(what string, ok func(s string) bool) {
		t.Helper()
		waitFor(t, "worker access: "+what, 30*time.Second, func() bool {
			b, err := os.ReadFile(accessFile)
			return err == nil && ok(string(b))
		})
	}
	waitAccess("owner alice", func(s string) bool { return strings.Contains(s, `"alice"`) })

	direct := fmt.Sprintf("aw@127.0.0.1:%d", aw.port)
	print1 := []string{"--", bins.helper, "print", "1", "0", "0"}
	exec := func(cfg, worker string) result {
		return h.longarm(cfg, append([]string{"exec", worker}, print1...)...)
	}
	if r := exec(alice, "aw"); r.code != 0 || !strings.Contains(r.stdout, "line 1") {
		t.Fatalf("alice exec on her worker: %+v", r)
	}
	id := strings.TrimSpace(h.mustLongarm(alice, append([]string{"run", "aw"}, print1...)...))
	h.mustLongarm(alice, "wait", "aw", id)
	if out := h.mustLongarm(alice, "ls"); !strings.Contains(out, "OWNER") || !lineStarts(out, "aw ") {
		t.Fatalf("alice's ls:\n%s", out)
	}
	if out := h.mustLongarm(alice, "worker", "ls"); !strings.Contains(out, "mine") || !strings.Contains(out, "fleet") {
		t.Fatalf("alice's worker ls:\n%s", out)
	}

	bobLocked := func(when string) {
		t.Helper()
		if out := h.longarm(bob, "ls").stdout; lineStarts(out, "aw ") {
			t.Fatalf("%s: bob sees aw:\n%s", when, out)
		}
		if r := exec(bob, "aw"); r.code == 0 {
			t.Fatalf("%s: bob exec aw by name: %+v", when, r)
		}
		if r := exec(bob, direct); r.code == 0 || !strings.Contains(r.stderr, "worker owner/shares") {
			t.Fatalf("%s: bob exec aw directly: %+v", when, r)
		}
		for i := 0; i < 3; i++ { // placement: always the fleet worker
			r := h.longarm(bob, append([]string{"run"}, print1...)...)
			if r.code != 0 || !strings.Contains(r.stderr, "placed on wf") {
				t.Fatalf("%s: bob's run: %+v", when, r)
			}
		}
	}
	bobLocked("not shared")
	// an admin is not exempt either (owner-only means owner-only)
	if r := exec(root, direct); r.code == 0 || !strings.Contains(r.stderr, "worker owner/shares") {
		t.Fatalf("admin exec on alice's worker: %+v", r)
	}

	// sharing needs an admin's permission
	if r := h.longarm(alice, "worker", "share", "aw", "--user", "bob"); r.code == 0 || !strings.Contains(r.stderr, "can-share") {
		t.Fatalf("share without permission: %+v", r)
	}
	h.mustLongarm(root, "admin", "user", "set", "alice", "--can-share")
	if out := h.mustLongarm(alice, "whoami"); !strings.Contains(out, "can share own workers: true") {
		t.Fatalf("whoami:\n%s", out)
	}
	h.mustLongarm(alice, "worker", "share", "aw", "--user", "bob")
	waitAccess("shared with bob", func(s string) bool { return strings.Contains(s, `"bob"`) })
	if r := exec(bob, "aw"); r.code != 0 || !strings.Contains(r.stdout, "line 1") {
		t.Fatalf("bob exec on the shared worker: %+v", r)
	}
	if out := h.mustLongarm(bob, "worker", "ls"); !strings.Contains(out, "shared") {
		t.Fatalf("bob's worker ls:\n%s", out)
	}

	h.mustLongarm(alice, "worker", "unshare", "aw", "--user", "bob")
	waitAccess("unshared", func(s string) bool { return !strings.Contains(s, `"bob"`) })
	bobLocked("unshared")

	waitFor(t, "hub audit of the sharing", 20*time.Second, func() bool {
		out := h.longarm(root, "admin", "audit", "--node", "hub", "--limit", "100", "--json").stdout
		return strings.Contains(out, "workers/share") && strings.Contains(out, "workers/code")
	})
	h.mustLongarm(alice, "worker", "remove", "aw")
	h.waitNode(t, "aw", "revoked", func(n map[string]any) bool { return n["state"] == "revoked" })
}

// lineStarts reports whether a line of out starts with prefix.
func lineStarts(out, prefix string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}
