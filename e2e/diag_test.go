package e2e

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var reportIDRE = regexp.MustCompile(`^LAR-\d{8}-[0-9A-HJKMNP-TV-Z]{6}$`)

// TestDiag: diagnostic bundles. The hub stores uploads in a file:// blob
// dir; `longarmd diag --upload --yes` on a worker and `longarm diag
// --upload --yes` on a client print report ids; `longarm admin diag ls`
// lists them and `get` downloads the stored zip byte for byte; a client
// without admin rights can't list or download.
func TestDiag(t *testing.T) {
	t.Parallel()
	blobDir := filepath.Join(t.TempDir(), "blobs")
	h := newHubEnv(t, fmt.Sprintf("[diagnostics]\nblob_url = %q\n", "file:///"+strings.TrimPrefix(filepath.ToSlash(blobDir), "/")))
	w := h.newWorkerDir(t, "wdiag", "")
	h.joinWorker(t, w, true)
	w.start(t, true)
	h.waitNode(t, "wdiag", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	h.mustLongarm(h.client, "exec", "wdiag", "--", bins.helper, "print", "1", "0", "0")

	// the worker
	wzip := filepath.Join(h.root, "worker-diag.zip")
	r := longarmdCmd(t, "diag", "--data-dir", w.dataDir, "--log-file", w.logPath, "--out", wzip, "--upload", "--yes", "--note", "exec hangs on wdiag")
	wid := strings.TrimSpace(r.stdout)
	if r.code != 0 || !reportIDRE.MatchString(wid) || !strings.Contains(r.stderr, "uploaded: file:///") {
		t.Fatalf("longarmd diag: %+v", r)
	}
	if !strings.Contains(r.stderr, "logs/log-file-1.log") || !strings.Contains(r.stderr, "not included") {
		t.Fatalf("no summary: %s", r.stderr)
	}
	files := zipNames(t, wzip)
	for _, f := range []string{"manifest.json", "system.json", "status.json", "logs/log-file-1.log", "certs.json", "jobs.json"} {
		if !files[f] {
			t.Errorf("worker bundle lacks %s: %v", f, files)
		}
	}
	// the worker's name as a word (the test's data dir w-wdiag is a longer
	// identifier, left alone like any)
	nameRE := regexp.MustCompile(`(^|[^A-Za-z0-9_-])(wdiag)($|[^A-Za-z0-9_-])|BEGIN`)
	if content := zipText(t, wzip); nameRE.MatchString(content) {
		i := nameRE.FindStringIndex(content)[0]
		t.Errorf("worker bundle leaks the name or a key: ...%s...", content[max(0, i-300):min(len(content), i+100)])
	}

	// the client (alice, not an admin)
	czip := filepath.Join(h.root, "client-diag.zip")
	cr := h.longarm(h.client, "diag", "--out", czip, "--upload", "--yes")
	cid := strings.TrimSpace(cr.stdout)
	if cr.code != 0 || !reportIDRE.MatchString(cid) {
		t.Fatalf("longarm diag: %+v", cr)
	}
	if !zipNames(t, czip)["connectivity.json"] {
		t.Error("client bundle lacks connectivity.json")
	}
	if c := zipText(t, czip); !strings.Contains(c, `"reachable": true`) || nameRE.MatchString(c) {
		t.Errorf("connectivity probe: %s", c)
	}
	// --worker only tells how
	if r := h.longarm(h.client, "diag", "--worker", "wdiag"); r.code == 0 || !strings.Contains(r.stderr, "longarm exec wdiag -- longarmd diag --upload --yes") {
		t.Fatalf("diag --worker: %+v", r)
	}

	// admins list and download
	ls := h.mustLongarm(h.admin, "admin", "diag", "ls")
	if !strings.Contains(ls, wid) || !strings.Contains(ls, cid) || !strings.Contains(ls, "exec hangs on") || strings.Contains(ls, "wdiag") {
		t.Fatalf("admin diag ls:\n%s", ls)
	}
	got := filepath.Join(h.root, "got.zip")
	h.mustLongarm(h.admin, "admin", "diag", "get", wid, "--out", got)
	day := wid[4:12]
	stored, err := os.ReadFile(filepath.Join(blobDir, day[:4], day[4:6], day[6:], wid+".zip"))
	if err != nil {
		t.Fatal(err)
	}
	dl, _ := os.ReadFile(got)
	local, _ := os.ReadFile(wzip)
	if !bytes.Equal(dl, stored) || !bytes.Equal(stored, local) {
		t.Fatalf("downloaded %d bytes, stored %d, local %d", len(dl), len(stored), len(local))
	}

	// a client without admin rights can't
	if r := h.longarm(h.client, "admin", "diag", "ls"); r.code == 0 || !strings.Contains(r.stderr, "PermissionDenied") {
		t.Fatalf("client ls: %+v", r)
	}
	if r := h.longarm(h.client, "admin", "diag", "get", cid, "--out", filepath.Join(h.root, "x.zip")); r.code == 0 || !strings.Contains(r.stderr, "PermissionDenied") {
		t.Fatalf("client get: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(h.root, "x.zip")); err == nil {
		t.Fatal("a refused download left a file")
	}
	// the hub logged the uploads, not their content
	waitFor(t, "upload log lines", 10*time.Second, func() bool {
		b, _ := os.ReadFile(filepath.Join(h.root, "hub.log"))
		return strings.Contains(string(b), "report="+wid) && strings.Contains(string(b), "report="+cid)
	})
}

func zipNames(t *testing.T, p string) map[string]bool {
	t.Helper()
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	m := map[string]bool{}
	for _, f := range zr.File {
		m[f.Name] = true
	}
	return m
}

func zipText(t *testing.T, p string) string {
	t.Helper()
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var sb strings.Builder
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		b.ReadFrom(rc)
		rc.Close()
		sb.WriteString(b.String())
	}
	return sb.String()
}
