package e2e

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/update"
)

// TestReleasePublish: the release key is created on the admin's machine
// (longarm-hub release keygen -> <longarm config>/release), longarm admin release publish
// signs there and uploads; the hub checks the signatures against its
// public_key and stores files + signatures, and never holds the private key.
// Tampered or unsigned update binaries and non-admins are refused.
func TestReleasePublish(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	// keygen on the "admin machine": the default dir is longarm's config dir.
	kg := exec.Command(h.hubBin, "release", "keygen")
	kg.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+h.admin, "LONGARM_RELEASE_KEY_PASSWORD=")
	var kout, kerr bytes.Buffer
	kg.Stdout, kg.Stderr = &kout, &kerr
	if err := kg.Run(); err != nil {
		t.Fatalf("keygen: %v %s", err, kerr.String())
	}
	pub := strings.TrimSpace(kout.String())
	keyFile := filepath.Join(h.admin, "release", update.KeyFile)
	if _, err := os.Stat(keyFile); err != nil || !strings.HasPrefix(pub, "RW") {
		t.Fatalf("keygen: key %v, pub %q, %s", err, pub, kerr.String())
	}
	// The hub learns the public key only.
	h.killHub()
	os.WriteFile(h.cfgPath, append(must(os.ReadFile(h.cfgPath)), []byte(fmt.Sprintf("[releases]\npublic_key = %q\n", pub))...), 0o600)
	h.startHub(t)

	rel := filepath.Join(h.root, "rel")
	os.MkdirAll(rel, 0o755)
	longarmdName := update.ReleaseName("longarmd", runtime.GOOS, runtime.GOARCH)
	longarmName := update.ReleaseName("longarm", runtime.GOOS, runtime.GOARCH)
	copyBin(t, bins.longarmd, filepath.Join(rel, longarmdName))
	copyBin(t, bins.longarm, filepath.Join(rel, longarmName))
	os.WriteFile(filepath.Join(rel, "notes.txt"), []byte("not a release file"), 0o644)

	out := h.mustLongarm(h.admin, "admin", "release", "publish", rel, "--version", "1.2.0")
	if !strings.Contains(out, "1.2.0 (stable) on the hub: 2 files, signed=true") {
		t.Fatalf("publish: %s", out)
	}
	if _, err := os.Stat(filepath.Join(rel, longarmdName+update.SigExt)); err != nil {
		t.Fatalf("not signed locally: %v", err)
	}
	if ls := h.mustLongarm(h.admin, "admin", "release", "ls"); !strings.Contains(ls, "1.2.0") || !strings.Contains(ls, longarmdName) {
		t.Fatalf("release ls: %s", ls)
	}
	stored := filepath.Join(h.hubDir, "releases", "1.2.0")
	for _, f := range []string{longarmdName, longarmdName + update.SigExt, longarmName} {
		if _, err := os.Stat(filepath.Join(stored, f)); err != nil {
			t.Fatalf("hub lacks %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stored, "notes.txt")); err == nil {
		t.Fatal("non-release file uploaded")
	}
	// The private key is nowhere in the hub's data.
	key := must(os.ReadFile(keyFile))
	filepath.WalkDir(h.hubDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, bytes.TrimSpace(key)) {
				t.Errorf("private key found on the hub: %s", p)
			}
		}
		return nil
	})
	// No upload staging left behind.
	if ents, _ := os.ReadDir(filepath.Join(h.hubDir, "releases")); len(ents) != 1 {
		t.Fatalf("releases dir: %v", ents)
	}

	// A tampered binary with the old signature: refused by the hub.
	bad := filepath.Join(h.root, "bad")
	os.MkdirAll(bad, 0o755)
	os.WriteFile(filepath.Join(bad, longarmdName), []byte("not longarmd"), 0o755)
	os.WriteFile(filepath.Join(bad, longarmdName+update.SigExt), must(os.ReadFile(filepath.Join(rel, longarmdName+update.SigExt))), 0o644)
	if r := h.longarm(h.admin, "admin", "release", "publish", bad, "--version", "1.2.0", "--no-sign"); r.code == 0 || !strings.Contains(r.stderr, "exists on the hub") {
		t.Fatalf("existing version overwritten without --replace: %+v", r)
	}
	if r := h.longarm(h.admin, "admin", "release", "publish", bad, "--version", "1.2.0", "--no-sign", "--replace"); r.code == 0 || !strings.Contains(r.stderr, "invalid signature") {
		t.Fatalf("tampered release published: %+v", r)
	}
	// Nothing of the refused upload was stored.
	if b := must(os.ReadFile(filepath.Join(stored, longarmdName))); !bytes.Equal(b, must(os.ReadFile(filepath.Join(rel, longarmdName)))) {
		t.Fatal("the refused upload replaced a stored file")
	}
	// Unsigned update binaries: refused.
	os.Remove(filepath.Join(bad, longarmdName+update.SigExt))
	if r := h.longarm(h.admin, "admin", "release", "publish", bad, "--version", "1.3.0", "--no-sign"); r.code == 0 || !strings.Contains(r.stderr, "no .minisig signature") {
		t.Fatalf("unsigned release published: %+v", r)
	}
	// Signing needs the key on this machine.
	if r := h.longarm(h.client, "admin", "release", "publish", rel, "--version", "1.4.0"); r.code == 0 || !strings.Contains(r.stderr, "sign:") {
		t.Fatalf("publish without a key: %+v", r)
	}
	// Only admins may publish.
	if r := h.longarm(h.client, "admin", "release", "publish", rel, "--version", "1.4.0", "--key", keyFile); r.code == 0 || !strings.Contains(strings.ToLower(r.stderr), "permission") {
		t.Fatalf("client published: %+v", r)
	}
	if ls := h.mustLongarm(h.admin, "admin", "release", "ls"); strings.Contains(ls, "1.3.0") || strings.Contains(ls, "1.4.0") {
		t.Fatalf("refused releases listed: %s", ls)
	}
}
