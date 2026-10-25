package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLeave: longarmd leave and longarm leave deregister from the hub (revoked there)
// and remove the local credentials; --kill-jobs/--purge need longarmd stopped
// (leaving through a running longarmd is covered by TestUIWorkerLocal).
func TestLeave(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	wa := h.newWorkerDir(t, "wa", "")
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	if r := longarmdCmd(t, "leave", "--purge", "--data-dir", wa.dataDir); r.code == 0 || !strings.Contains(r.stderr, "stop it first") {
		t.Fatalf("leave while running: %+v", r)
	}
	wa.kill()
	r := longarmdCmd(t, "leave", "--data-dir", wa.dataDir)
	if r.code != 0 || !strings.Contains(r.stdout, "left hub") {
		t.Fatalf("longarmd leave: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(wa.dataDir, "tls")); err == nil {
		t.Fatal("tls/ left behind")
	}
	h.waitNode(t, "wa", "revoked", func(n map[string]any) bool { return n["state"] == "revoked" })

	code := strings.TrimSpace(h.mustLongarm(h.admin, "admin", "join-code", "--role", "client"))
	c2 := filepath.Join(h.root, "c2")
	h.mustLongarm(c2, "join", h.addr(), code, "--ca-fingerprint", h.fp, "--name", "bob")
	if out := h.mustLongarm(c2, "leave"); !strings.Contains(out, "left hub") {
		t.Fatalf("longarm leave: %s", out)
	}
	h.waitNode(t, "bob", "revoked", func(n map[string]any) bool { return n["state"] == "revoked" })
	if _, err := os.Stat(filepath.Join(c2, "tls")); err == nil {
		t.Fatal("client tls/ left behind")
	}
}
