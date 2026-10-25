package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
)

// serverCertNotAfter does a fresh TLS handshake with longarmd and returns the
// expiry of the certificate it presents.
func (e *env) serverCertNotAfter(t *testing.T) time.Time {
	t.Helper()
	b, err := pki.LoadBundle(filepath.Join(e.client, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: b.ClientConfig("w1")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", e.port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.(*tls.Conn).ConnectionState().PeerCertificates[0].NotAfter
}

// TestOps covers the P0b ops items: out.log size cap with rotation, GC
// (longarm admin gc, longarmd gc), and TLS certificate hot-reload.
func TestOps(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(e.dataDir, 0o700)
	os.WriteFile(filepath.Join(e.dataDir, "longarmd.toml"), []byte("max_log_bytes = 4096\n[gc]\ndisabled = true\n"), 0o600)
	e.startLongarmd()

	t.Run("log_rotation", func(t *testing.T) {
		id := e.run("--", bins.helper, "print", "1000", "0", "0")
		e.mustLongarm(e.client, "wait", "w1", id)
		dir := filepath.Join(e.dataDir, "jobs", id)
		for _, f := range []string{"out.log", "out.log.1"} {
			fi, err := os.Stat(filepath.Join(dir, f))
			if err != nil || fi.Size() > 4096 {
				t.Fatalf("%s: %v (size %d)", f, err, fi.Size())
			}
		}
		r := e.longarm(e.client, "logs", "w1", id)
		if r.code != 0 || !strings.Contains(r.stderr, "rotated away") || len(r.stdout) > 2*4096 {
			t.Fatalf("logs: code %d, %d bytes, stderr %q", r.code, len(r.stdout), r.stderr)
		}
		if !strings.HasSuffix(r.stdout, "line 1000\nerr 1000\n") {
			t.Fatalf("log tail %q", r.stdout[len(r.stdout)-40:])
		}
		tail := e.mustLongarm(e.client, "logs", "w1", id, "--from", "-10")
		if tail != r.stdout[len(r.stdout)-10:] {
			t.Fatalf("--from -10 = %q", tail)
		}
		jl := e.mustLongarm(e.client, "jobs", "w1", "--json")
		if !strings.Contains(jl, `"logSize"`) {
			t.Fatalf("jobs --json: %s", jl)
		}
	})

	t.Run("gc", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not installed")
		}
		repo := filepath.Join(e.root, "gcrepo")
		os.MkdirAll(repo, 0o755)
		git(t, repo, "init", "-q", "-b", "main")
		os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644)
		git(t, repo, "add", ".")
		git(t, repo, "commit", "-qm", "c")
		out := e.mustLongarm(e.client, "run", "w1", "--repo", repo, "--wait", "--", bins.helper, "cat", "f.txt")
		wsJob := strings.SplitN(out, "\n", 2)[0]
		wt := filepath.Join(e.dataDir, "worktrees", wsJob)
		if _, err := os.Stat(wt); err != nil {
			t.Fatal("no worktree for the workspace job")
		}
		running := e.run("--", bins.helper, "sleep", "120")

		if r := e.longarm(e.client, "admin", "gc", "w1"); r.code == 0 || !strings.Contains(r.stderr, "PermissionDenied") {
			t.Fatalf("client ran gc: %+v", r)
		}
		dry := e.mustLongarm(e.admin, "admin", "gc", "w1", "--keep", "0s", "--dry-run")
		if !strings.Contains(dry, "jobs/"+wsJob) || strings.Contains(dry, running) || !strings.Contains(dry, "would free") {
			t.Fatalf("dry run:\n%s", dry)
		}
		if _, err := os.Stat(filepath.Join(e.dataDir, "jobs", wsJob)); err != nil {
			t.Fatal("dry run removed something")
		}
		res := e.mustLongarm(e.admin, "admin", "gc", "w1", "--keep", "0s")
		if !strings.Contains(res, "jobs/"+wsJob) {
			t.Fatalf("gc:\n%s", res)
		}
		for _, p := range []string{filepath.Join(e.dataDir, "jobs", wsJob), wt} {
			if _, err := os.Stat(p); err == nil {
				t.Fatalf("%s still exists after gc", p)
			}
		}
		if jl := e.mustLongarm(e.client, "jobs", "w1"); strings.Contains(jl, wsJob) || !strings.Contains(jl, running) {
			t.Fatalf("jobs after gc:\n%s", jl)
		}
		// The mirror and the running job survive.
		if _, err := os.Stat(filepath.Join(e.dataDir, "mirrors", "gcrepo.git")); err != nil {
			t.Fatal("mirror removed")
		}
		e.mustLongarm(e.client, "kill", "w1", running)

		// Offline longarmd gc refuses to run next to a live longarmd ...
		cmd := exec.Command(bins.longarmd, "gc", "--data-dir", e.dataDir, "--keep", "0s")
		b, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(b), "longarm admin gc") {
			t.Fatalf("longarmd gc next to longarmd: %v %s", err, b)
		}
		// ... and works when longarmd is stopped.
		e.killLongarmd()
		b, err = exec.Command(bins.longarmd, "gc", "--data-dir", e.dataDir, "--keep", "0s").CombinedOutput()
		if err != nil || !strings.Contains(string(b), "jobs/"+running) {
			t.Fatalf("longarmd gc: %v %s", err, b)
		}
		e.startLongarmd()
	})

	t.Run("tls_hot_reload", func(t *testing.T) {
		before := e.serverCertNotAfter(t)
		// Re-issue the worker certificate (shorter validity) in place.
		e.mustLongarm(e.client, "admin", "issue", "--role", "worker", "--name", "w1", "--days", "3", "--out", filepath.Join(e.dataDir, "tls"))
		want := time.Now().Add(3 * 24 * time.Hour)
		waitFor(t, "longarmd to serve the new certificate", 30*time.Second, func() bool {
			na := e.serverCertNotAfter(t)
			return !na.Equal(before) && na.Sub(want).Abs() < time.Hour
		})
		e.mustLongarm(e.client, "info", "w1")
		b, _ := os.ReadFile(filepath.Join(e.root, "longarmd.out"))
		if !strings.Contains(string(b), "TLS bundle reloaded") {
			t.Fatal("no reload log line")
		}
	})
}
