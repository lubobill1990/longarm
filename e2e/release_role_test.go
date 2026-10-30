package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/update"
)

// TestReleaseRole: the CI credential (`longarm admin issue --role release`, or
// `longarm-hub issue --role release` on the hub) can publish a signed release and
// list / push / inspect / abort rollouts - and nothing else: no hub node or
// job administration, no join codes, nothing on a worker (exec, jobs,
// files, shutdown), no relay. Join codes can't mint the role, and revoking
// the CI node locks it out.
func TestReleaseRole(t *testing.T) {
	t.Parallel()
	h, keyFile := newReleaseHub(t)
	h.startLongarmd() // w1, direct (workers.toml), not enrolled at the hub

	// The CI's config dir: the release bundle + the hub address, like the
	// workflow writes it.
	ci := filepath.Join(h.root, "ci")
	out := h.mustLongarm(h.client, "admin", "issue", "--role", "release", "--name", "ci", "--out", filepath.Join(ci, "tls"))
	if !strings.Contains(out, "issued release/ci") {
		t.Fatalf("issue: %s", out)
	}
	b, err := pki.LoadBundle(filepath.Join(ci, "tls"))
	if err != nil || b.Identity.Role != pki.RoleRelease || b.Identity.Name != "ci" || b.Identity.ID == "" {
		t.Fatalf("bundle: %v %v", b, err)
	}
	os.WriteFile(filepath.Join(ci, "hub.toml"), []byte(fmt.Sprintf("address = %q\n", h.addr())), 0o600)
	os.WriteFile(filepath.Join(ci, "workers.toml"), must(os.ReadFile(filepath.Join(h.client, "workers.toml"))), 0o600)

	// ---- allowed: publish, ls, push, status, abort
	rel := filepath.Join(h.root, "rel")
	os.MkdirAll(rel, 0o755)
	copyBin(t, bins.longarmd, filepath.Join(rel, update.ReleaseName("longarmd", runtime.GOOS, runtime.GOARCH)))
	copyBin(t, h.hubBin, filepath.Join(rel, update.ReleaseName("longarm-hub", "linux", "amd64")))
	out = h.mustLongarm(ci, "admin", "release", "publish", rel, "--version", "1.5.0", "--key", keyFile)
	if !strings.Contains(out, "1.5.0 (stable) on the hub: 2 files, signed=true") {
		t.Fatalf("publish: %s", out)
	}
	// a workflow re-run: same files -> nothing to do; other files -> refused
	out = h.mustLongarm(ci, "admin", "release", "publish", rel, "--version", "1.5.0", "--key", keyFile, "--skip-identical")
	if !strings.Contains(out, "already on the hub with these 2 files") {
		t.Fatalf("re-publish: %s", out)
	}
	os.WriteFile(filepath.Join(rel, update.ReleaseName("longarm-hub", "linux", "amd64")), []byte("another build"), 0o755)
	if r := h.longarm(ci, "admin", "release", "publish", rel, "--version", "1.5.0", "--key", keyFile, "--skip-identical"); r.code == 0 || !strings.Contains(r.stderr, "with a different longarm-hub-linux-amd64") {
		t.Fatalf("re-publish of other files: %+v", r)
	}
	// beta is the old name of preview: the hub stores preview
	out = h.mustLongarm(ci, "admin", "release", "publish", rel, "--version", "1.6.0-rc1", "--channel", "beta", "--key", keyFile)
	if !strings.Contains(out, "1.6.0-rc1 (preview) on the hub") {
		t.Fatalf("publish beta: %s", out)
	}
	if ls := h.mustLongarm(ci, "admin", "release", "ls"); !strings.Contains(ls, "1.5.0") || !strings.Contains(ls, "1.6.0-rc1") {
		t.Fatalf("release ls: %s", ls)
	}
	if st := h.mustLongarm(ci, "admin", "release", "status"); !strings.Contains(st, "no rollout") {
		t.Fatalf("status: %s", st)
	}
	// push is authorized and reaches the handler (no self-updating worker
	// here, so it is refused for that reason, not for the role).
	r := h.longarm(ci, "admin", "release", "push", "1.5.0", "--canary", "1", "--wait", "1s", "-f")
	if r.code == 0 || !strings.Contains(r.stderr, "can self-update") || isDenied(r) {
		t.Fatalf("push: %+v", r)
	}
	if r := h.longarm(ci, "admin", "release", "abort"); isDenied(r) {
		t.Fatalf("abort denied: %+v", r)
	}
	// ... but no downgrades and no replacing published files (admin only)
	if r := h.longarm(ci, "admin", "release", "push", "1.5.0", "--allow-downgrade"); !strings.Contains(r.stderr, "may not downgrade") {
		t.Fatalf("downgrade push as release: %+v", r)
	}
	if r := h.longarm(ci, "admin", "release", "publish", rel, "--version", "1.5.0", "--key", keyFile, "--replace"); !strings.Contains(r.stderr, "may not replace") {
		t.Fatalf("replace as release: %+v", r)
	}
	if r := h.longarm(ci, "admin", "release", "publish", rel, "--version", "1.6.0-rc1", "--key", keyFile, "--skip-identical"); r.code == 0 || !strings.Contains(r.stderr, "on channel preview") {
		t.Fatalf("skip-identical across channels: %+v", r)
	}
	if r := h.longarm(ci, "admin", "release", "publish", rel, "--version", "1.6.0-rc1", "--channel", "preview", "--key", keyFile, "--skip-identical"); r.code != 0 {
		t.Fatalf("skip-identical, beta published as preview: %+v", r)
	}

	// ---- denied on the hub: nodes, jobs, join codes, approval, revocation,
	// audit, labels, profiles, tailnet
	for _, args := range [][]string{
		{"admin", "nodes"},
		{"admin", "join-code", "--role", "client"},
		{"admin", "join-codes"},
		{"admin", "approve", "w1"},
		{"admin", "revoke", "w1"},
		{"admin", "drain", "w1"},
		{"admin", "label", "w1", "a=b"},
		{"admin", "audit"},
		{"admin", "profile", "ls"},
		{"admin", "tailnet", "status"},
	} {
		r := h.longarm(ci, args...)
		if r.code == 0 || !isDenied(r) {
			t.Errorf("longarm %v as release: want permission denied, got %+v", args, r)
		}
	}
	// ---- denied on a worker (longarmd's own table), straight over tcp
	for _, args := range [][]string{
		{"--no-hub", "exec", "w1", "--", "hostname"},
		{"--no-hub", "run", "w1", "--", "hostname"},
		{"--no-hub", "info", "w1"},
		{"--no-hub", "jobs", "w1"},
		{"--no-hub", "admin", "shutdown", "w1"},
	} {
		r := h.longarm(ci, args...)
		if r.code == 0 || !isDenied(r) {
			t.Errorf("longarm %v as release: want permission denied, got %+v", args, r)
		}
	}
	// the worker still serves the admin
	if out := h.mustLongarm(h.admin, "--no-hub", "info", "w1"); !strings.Contains(out, "w1") {
		t.Fatalf("admin info: %s", out)
	}

	// ---- join codes can't mint the role; longarm-hub issue can (on the hub)
	if r := h.longarm(h.admin, "admin", "join-code", "--role", "release"); r.code == 0 || !strings.Contains(r.stderr, "want worker, client or admin") {
		t.Fatalf("release join code: %+v", r)
	}
	out = h.hubCmd(t, "issue", "--role", "release", "--name", "ci2", "--out", filepath.Join(h.root, "ci2", "tls"))
	if !strings.Contains(out, "release/ci2") {
		t.Fatalf("longarm-hub issue: %s", out)
	}

	// ---- revoking the CI node locks it out
	if out := h.mustLongarm(h.admin, "admin", "revoke", "ci"); !strings.Contains(out, "revoked") {
		t.Fatalf("revoke ci: %s", out)
	}
	// (the hub refuses the revoked certificate in the TLS handshake)
	if r := h.longarm(ci, "admin", "release", "ls"); r.code == 0 || strings.Contains(r.stdout, "1.5.0") {
		t.Fatalf("revoked CI still lists releases: %+v", r)
	}
}

// newReleaseHub is a hub env whose hub.toml has [releases] public_key of a
// fresh release key (made like on the admin machine; the hub only gets the
// public key). Returns the private key file.
func newReleaseHub(t *testing.T) (*hubEnv, string) {
	t.Helper()
	h := newHubEnv(t, "")
	kg := exec.Command(h.hubBin, "release", "keygen", "--out", filepath.Join(h.root, "keys"))
	kg.Env = append(os.Environ(), "LONGARM_RELEASE_KEY_PASSWORD=")
	var kout, kerr bytes.Buffer
	kg.Stdout, kg.Stderr = &kout, &kerr
	if err := kg.Run(); err != nil {
		t.Fatalf("keygen: %v %s", err, kerr.String())
	}
	pub := strings.TrimSpace(kout.String())
	h.killHub()
	os.WriteFile(h.cfgPath, append(must(os.ReadFile(h.cfgPath)), []byte(fmt.Sprintf("[releases]\npublic_key = %q\n", pub))...), 0o600)
	h.startHub(t)
	return h, filepath.Join(h.root, "keys", update.KeyFile)
}

func isDenied(r result) bool {
	s := strings.ToLower(r.stderr + r.stdout)
	return strings.Contains(s, "permissiondenied") || strings.Contains(s, "permission denied") ||
		strings.Contains(s, "may not call")
}
