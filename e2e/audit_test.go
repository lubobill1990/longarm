package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAuditTrail: a worker's audit.log is hash-chained (longarmd audit
// verify), copied to the owner's sinks (a file, a webhook) and still
// forwarded to the hub; a broken [audit] section keeps longarmd from
// starting.
func TestAuditTrail(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var hooked strings.Builder
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer tok" {
			hooked.Write(b)
		}
	}))
	defer hook.Close()

	h := newHubEnv(t, "")
	root := t.TempDir()
	copyPath := filepath.Join(root, "audit-copy.log")
	os.WriteFile(filepath.Join(root, "token"), []byte("tok\n"), 0o600)
	wa := h.newWorkerDir(t, "wa", fmt.Sprintf("[audit]\nfile = '%s'\nwebhook = '%s'\nwebhook_token_file = '%s'\n",
		copyPath, hook.URL, filepath.Join(root, "token")))
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	for range 2 {
		if r := h.longarm(h.client, "exec", "wa", "--", bins.helper, "print", "1", "0", "0"); r.code != 0 {
			t.Fatalf("exec: %+v", r)
		}
	}
	waitFor(t, "the webhook copy", 20*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(hooked.String(), "/longarm.v1.Exec/Run")
	})
	waitFor(t, "forwarded chained records", 20*time.Second, func() bool {
		r := h.longarm(h.admin, "admin", "audit", "--node", "wa", "--limit", "100")
		return strings.Contains(r.stdout, "Exec/Run")
	})
	if r := longarmdCmd(t, "audit", "verify", "--data-dir", wa.dataDir); r.code != 0 || !strings.HasPrefix(r.stdout, "ok: ") {
		t.Fatalf("verify: %+v", r)
	}
	if r := longarmdCmd(t, "audit", "verify", "--file", copyPath); r.code != 0 || !strings.HasPrefix(r.stdout, "ok: ") {
		t.Fatalf("verify the copy: %+v", r)
	}
	if b, _ := os.ReadFile(copyPath); !strings.Contains(string(b), `"seq":1,"prev":""`) {
		t.Fatalf("copy:\n%s", b)
	}

	// An edited line is found.
	wa.kill()
	path := filepath.Join(wa.dataDir, "audit.log")
	b, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(b), "\n")
	if len(lines) < 3 {
		t.Fatalf("audit.log:\n%s", b)
	}
	lines[0] = strings.Replace(lines[0], `"who":"`, `"who":"x`, 1)
	os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600)
	if r := longarmdCmd(t, "audit", "verify", "--data-dir", wa.dataDir); r.code != 1 || !strings.Contains(r.stderr, "chain broken at line") {
		t.Fatalf("verify after an edit: %+v", r)
	}

	// A broken [audit] section is fatal at start.
	os.WriteFile(filepath.Join(wa.dataDir, "longarmd.toml"), []byte(fmt.Sprintf("listen = [\"tcp://127.0.0.1:%d\"]\n[audit]\nwebhook = \"http://audit.example.com/\"\n", wa.port)), 0o600)
	wa.start(t, false)
	select {
	case <-wa.exited:
	case <-time.After(60 * time.Second):
		t.Fatal("longarmd started with a plain-http webhook")
	}
	if log, _ := os.ReadFile(wa.logPath); !strings.Contains(string(log), "https:// required") {
		t.Fatalf("log:\n%s", log)
	}
}
