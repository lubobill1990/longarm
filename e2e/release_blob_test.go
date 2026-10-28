package e2e

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/update"
)

// TestReleaseBlobStore: a hub with [releases] blob_url (deploy/azure with
// storageAuth identity, whose data dir doesn't survive a restart) keeps the
// published files in the container: after a restart with an empty data dir
// /releases/... is served from there; a file gone everywhere takes its
// version out of the install scripts and `release ls` marks it; publishing
// the same files again (publish --skip-identical, CI's re-run) restores it.
func TestReleaseBlobStore(t *testing.T) {
	t.Parallel()
	blobDir := filepath.Join(t.TempDir(), "blobs")
	h := newHubEnv(t, fmt.Sprintf("[releases]\nblob_url = %q\n", "file:///"+strings.TrimPrefix(filepath.ToSlash(blobDir), "/")))

	rel := filepath.Join(h.root, "rel")
	os.MkdirAll(rel, 0o755)
	longarmdName := update.ReleaseName("longarmd", "linux", "amd64")
	files := map[string]string{longarmdName: "longarmd 1.0.0", "longarm-windows-amd64.msi": "msi 1.0.0"}
	for n, c := range files {
		os.WriteFile(filepath.Join(rel, n), []byte(c), 0o644)
	}
	out := h.mustLongarm(h.admin, "admin", "release", "publish", rel, "--version", "1.0.0", "--no-sign", "--skip-identical")
	if !strings.Contains(out, "1.0.0 (stable) on the hub: 2 files") {
		t.Fatalf("publish: %s", out)
	}
	for n, c := range files {
		if b, err := os.ReadFile(filepath.Join(blobDir, "1.0.0", n)); err != nil || string(b) != c {
			t.Fatalf("blob %s: %q %v", n, b, err)
		}
	}
	get := func(path string) (int, string) {
		resp, err := http.Get(h.httpURL() + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// waitScript polls install.sh until it advertises version (the hub
	// lists the container in the background after a start)
	waitScript := func(version string) {
		t.Helper()
		want := "VERSION='" + version + "'"
		var s string
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if _, s = get("/install.sh"); strings.Contains(s, want) {
				return
			}
		}
		t.Fatalf("install.sh never had %s:\n%.400s", want, s)
	}
	waitScript("1.0.0")

	// restart with an empty data dir: served from the container
	restart := func() {
		h.killHub()
		if err := os.RemoveAll(filepath.Join(h.hubDir, "releases")); err != nil {
			t.Fatal(err)
		}
		h.startHub(t)
	}
	restart()
	if code, body := get("/releases/1.0.0/longarm-windows-amd64.msi"); code != 200 || body != "msi 1.0.0" {
		t.Fatalf("after a restart: %d %q", code, body)
	}
	waitScript("1.0.0")
	if code, _ := get("/releases/1.0.0/..%2F..%2Fhub.db"); code != 404 {
		t.Fatalf("traversal: %d", code)
	}

	// gone everywhere: not advertised, marked in release ls
	h.killHub()
	os.RemoveAll(filepath.Join(h.hubDir, "releases"))
	os.Remove(filepath.Join(blobDir, "1.0.0", "longarm-windows-amd64.msi"))
	h.startHub(t)
	waitScript("")
	if code, _ := get("/releases/1.0.0/longarm-windows-amd64.msi"); code != 404 {
		t.Fatalf("missing file: %d", code)
	}
	r := h.longarm(h.admin, "admin", "release", "ls")
	if r.code != 0 || !strings.Contains(r.stdout, "longarm-windows-amd64.msi(missing)") || !strings.Contains(r.stderr, "the hub lost files of 1.0.0") {
		t.Fatalf("release ls: %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(h.root, "hub.log")); !strings.Contains(string(b), "RELEASE FILES MISSING") {
		t.Fatal("no loud log line")
	}

	// other files under the same version: still refused
	other := filepath.Join(h.root, "other")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, longarmdName), []byte("rebuilt"), 0o644)
	os.WriteFile(filepath.Join(other, "longarm-windows-amd64.msi"), []byte("msi 1.0.0"), 0o644)
	if r := h.longarm(h.admin, "admin", "release", "publish", other, "--version", "1.0.0", "--no-sign", "--skip-identical"); r.code == 0 || !strings.Contains(r.stderr, "exists on the hub with other files") {
		t.Fatalf("other files: %+v", r)
	}
	// the same files again (CI's re-run): restored
	out = h.mustLongarm(h.admin, "admin", "release", "publish", rel, "--version", "1.0.0", "--no-sign", "--skip-identical")
	if !strings.Contains(out, "1.0.0 (stable) on the hub: 2 files") {
		t.Fatalf("re-publish: %s", out)
	}
	if code, body := get("/releases/1.0.0/longarm-windows-amd64.msi"); code != 200 || body != "msi 1.0.0" {
		t.Fatalf("after re-publish: %d %q", code, body)
	}
	if b, err := os.ReadFile(filepath.Join(blobDir, "1.0.0", "longarm-windows-amd64.msi")); err != nil || string(b) != "msi 1.0.0" {
		t.Fatalf("blob not restored: %v", err)
	}
	waitScript("1.0.0")
	// now complete: a further re-run skips the upload
	if out := h.mustLongarm(h.admin, "admin", "release", "publish", rel, "--version", "1.0.0", "--no-sign", "--skip-identical"); !strings.Contains(out, "nothing to upload") {
		t.Fatalf("re-run of a complete version: %s", out)
	}
}
