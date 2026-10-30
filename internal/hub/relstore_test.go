package hub

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/lubobill1990/longarm/internal/diag"
	"github.com/lubobill1990/longarm/internal/hub/store"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// memBlob is an in-memory blob container.
type memBlob struct {
	mu    sync.Mutex
	blobs map[string][]byte
	gets  int
	fail  error // List and Get fail with it
	// advertise: the size Get reports (-1 = unknown), instead of the real one
	advertise map[string]int64
	// hold: Get waits for it (nil = no wait)
	hold                  chan struct{}
	inflight, maxInflight int
	read                  int64 // bytes read from Get bodies
}

func newMemBlob() *memBlob { return &memBlob{blobs: map[string][]byte{}} }

func (m *memBlob) Put(_ context.Context, name string, r io.Reader, size int64, _ map[string]string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return errors.New("short body")
	}
	m.mu.Lock()
	m.blobs[name] = b
	m.mu.Unlock()
	return nil
}

func (m *memBlob) Get(ctx context.Context, name string) (io.ReadCloser, diag.BlobInfo, error) {
	m.mu.Lock()
	m.inflight++
	m.maxInflight = max(m.maxInflight, m.inflight)
	hold := m.hold
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.inflight--
		m.mu.Unlock()
	}()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, diag.BlobInfo{}, ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, diag.BlobInfo{}, m.fail
	}
	m.gets++
	b, ok := m.blobs[name]
	if !ok {
		return nil, diag.BlobInfo{}, diag.ErrNotFound
	}
	size := int64(len(b))
	if a, ok := m.advertise[name]; ok {
		size = a
	}
	return io.NopCloser(&countReader{r: bytes.NewReader(b), m: m}), diag.BlobInfo{Name: name, Size: size}, nil
}

type countReader struct {
	r io.Reader
	m *memBlob
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.m.mu.Lock()
	c.m.read += int64(n)
	c.m.mu.Unlock()
	return n, err
}

func (m *memBlob) getCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gets
}

func (m *memBlob) List(_ context.Context, prefix string) ([]diag.BlobInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	var out []diag.BlobInfo
	for n, b := range m.blobs {
		if strings.HasPrefix(n, prefix) {
			out = append(out, diag.BlobInfo{Name: n, Size: int64(len(b))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *memBlob) Location() string { return "mem://releases" }

func (m *memBlob) names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for n := range m.blobs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// releaseDir writes fake release files (no public key: signatures are
// stored as they are).
func releaseDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func testReleaseStore(t *testing.T, dataDir string, blob diag.Store) (*releaseStore, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	rs, err := openReleaseStore(&Config{DataDir: dataDir}, nil, slog.New(slog.NewTextHandler(&syncWriter{w: logs}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if blob != nil {
		rs.blob = blob
	}
	return rs, logs
}

func readAll(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestReleaseStoreBlob: published files go to the blob container; a data
// dir that lost them (a container restart) gets them back from there,
// checked against the index; files that are nowhere are reported, and the
// same files published again restore them without touching the index.
func TestReleaseStoreBlob(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	blob := newMemBlob()
	data := t.TempDir()
	rs, _ := testReleaseStore(t, data, blob)
	files := map[string]string{
		"longarmd-linux-amd64":         "longarmd 1.0.0",
		"longarmd-linux-amd64.minisig": "sig-1",
		"longarm-windows-amd64.msi":    "msi 1.0.0",
	}
	if _, err := addRelease(ctx, rs, st, releaseDir(t, files), "1.0.0", "", "", false); err != nil {
		t.Fatal(err)
	}
	want := []string{"1.0.0/longarm-windows-amd64.msi", "1.0.0/longarmd-linux-amd64", "1.0.0/longarmd-linux-amd64.minisig"}
	if got := blob.names(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("blobs %v, want %v", got, want)
	}
	if readAll(t, filepath.Join(data, ReleasesDir, "1.0.0", "longarm-windows-amd64.msi")) != "msi 1.0.0" {
		t.Fatal("no local copy")
	}

	// a restart: the data dir is new, the blobs and the database stay
	data2 := t.TempDir()
	rs2, logs := testReleaseStore(t, data2, blob)
	rs2.reconcile(ctx, st)
	if strings.Contains(logs.String(), "MISSING") {
		t.Fatalf("reconcile: %s", logs)
	}
	rels, _ := st.Releases()
	if m := rs2.missingFiles(rels); len(m) != 0 {
		t.Fatalf("missing %v", m)
	}
	var msi, longarmd store.Release
	for _, r := range rels {
		switch r.File {
		case "longarm-windows-amd64.msi":
			msi = r
		case "longarmd-linux-amd64":
			longarmd = r
		}
	}
	p, err := rs2.file(ctx, msi, false)
	if err != nil || readAll(t, p) != "msi 1.0.0" || !strings.HasPrefix(p, data2) {
		t.Fatalf("file from blob: %s %v", p, err)
	}
	gets := blob.gets
	if _, err := rs2.file(ctx, msi, false); err != nil || blob.gets != gets {
		t.Fatalf("second read not from the cache: %v (gets %d -> %d)", err, gets, blob.gets)
	}
	if p, err := rs2.file(ctx, longarmd, true); err != nil || readAll(t, p) != "sig-1" {
		t.Fatalf("signature: %v", err)
	}

	// a blob that doesn't match the index is refused
	blob.blobs["1.0.0/longarmd-linux-amd64"] = []byte("LONGARMD 1.0.0") // same size
	if _, err := rs2.file(ctx, longarmd, false); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("tampered blob: %v", err)
	}
	if regularFile(rs2.localPath("1.0.0", "longarmd-linux-amd64")) {
		t.Fatal("tampered blob cached")
	}

	// gone everywhere: reported, not servable
	delete(blob.blobs, "1.0.0/longarmd-linux-amd64")
	data3 := t.TempDir()
	rs3, logs3 := testReleaseStore(t, data3, blob)
	rs3.reconcile(ctx, st)
	if !strings.Contains(logs3.String(), "RELEASE FILES MISSING") || !strings.Contains(logs3.String(), "longarmd-linux-amd64") {
		t.Fatalf("no loud log: %s", logs3)
	}
	if m := rs3.missingFiles(rels); len(m) != 1 || m[0] != "longarmd-linux-amd64" {
		t.Fatalf("missing %v", m)
	}
	if _, err := rs3.file(ctx, longarmd, false); !errors.Is(err, errReleaseMissing) {
		t.Fatalf("missing file: %v", err)
	}

	// a newer version is latest; the old one published again with other
	// files is refused, with the same files restored
	if _, err := addRelease(ctx, rs3, st, releaseDir(t, map[string]string{"longarmd-linux-amd64": "longarmd 2.0.0"}), "2.0.0", "", "", false); err != nil {
		t.Fatal(err)
	}
	other := map[string]string{"longarmd-linux-amd64": "rebuilt", "longarm-windows-amd64.msi": "msi 1.0.0"}
	if _, err := addRelease(ctx, rs3, st, releaseDir(t, other), "1.0.0", "", "", true); !errors.Is(err, errReleaseDiffers) {
		t.Fatalf("other files: %v", err)
	}
	if _, err := addRelease(ctx, rs3, st, releaseDir(t, map[string]string{"longarm-windows-amd64.msi": "msi 1.0.0"}), "1.0.0", "", "", true); !errors.Is(err, errReleaseDiffers) {
		t.Fatalf("fewer files: %v", err)
	}
	if _, err := addRelease(ctx, rs3, st, releaseDir(t, files), "1.0.0", "beta", "", true); !errors.Is(err, errReleaseDiffers) {
		t.Fatalf("other channel: %v", err)
	}
	resig := map[string]string{
		"longarmd-linux-amd64":         "longarmd 1.0.0",
		"longarmd-linux-amd64.minisig": "sig-2", // signed again: the index keeps the first
		"longarm-windows-amd64.msi":    "msi 1.0.0",
	}
	if _, err := addRelease(ctx, rs3, st, releaseDir(t, resig), "1.0.0", "", "", true); err != nil {
		t.Fatal(err)
	}
	if string(blob.blobs["1.0.0/longarmd-linux-amd64"]) != "longarmd 1.0.0" || string(blob.blobs["1.0.0/longarmd-linux-amd64.minisig"]) != "sig-1" {
		t.Fatalf("not restored: %v", blob.names())
	}
	if m := rs3.missingFiles(rels); len(m) != 0 {
		t.Fatalf("missing after restore %v", m)
	}
	if v, _ := st.Meta("latest_release"); v != "2.0.0" {
		t.Fatalf("latest_release %q after restoring 1.0.0", v)
	}
	rels2, _ := st.Releases()
	if len(rels2) != len(rels)+1 {
		t.Fatalf("index changed: %d rows", len(rels2))
	}
}

// TestReleaseStoreReconcileUploads: files only the data dir has (published
// before blob_url was set) are copied to the container at start; a
// container that can't be listed yet doesn't make files "missing".
func TestReleaseStoreReconcileUploads(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	data := t.TempDir()
	local, _ := testReleaseStore(t, data, nil)
	files := map[string]string{"longarmd-linux-amd64": "d", "longarmd-linux-amd64.minisig": "s"}
	if _, err := addRelease(ctx, local, st, releaseDir(t, files), "1.0.0", "", "", false); err != nil {
		t.Fatal(err)
	}
	blob := newMemBlob()
	rs, _ := testReleaseStore(t, data, blob)
	rels, _ := st.Releases()
	os.Remove(rs.localPath("1.0.0", "longarmd-linux-amd64"))
	if !rs.has(rels[0]) {
		t.Fatal("unlisted container: file called missing")
	}
	os.WriteFile(rs.localPath("1.0.0", "longarmd-linux-amd64"), []byte("d"), 0o644)
	rs.reconcile(ctx, st)
	if got := strings.Join(blob.names(), " "); got != "1.0.0/longarmd-linux-amd64 1.0.0/longarmd-linux-amd64.minisig" {
		t.Fatalf("blobs after reconcile: %s", got)
	}
}

// TestReleaseServing: the HTTP route and install scripts with a release
// store that lost files.
func TestReleaseServing(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := InitCA(dir); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: dir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "off"}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	h, err := Open(cfg, slog.New(slog.NewTextHandler(&syncWriter{w: logs}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	blob := newMemBlob()
	h.rel.blob = blob
	for _, v := range []string{"1.0.0", "1.1.0"} {
		files := map[string]string{"longarmd-linux-amd64": "d" + v, "longarmd-linux-amd64.minisig": "s" + v, "longarm-linux-amd64.deb": "deb" + v}
		if _, err := addRelease(ctx, h.rel, h.store, releaseDir(t, files), v, "", "", false); err != nil {
			t.Fatal(err)
		}
	}
	get := func(v, f string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/releases/"+v+"/"+f, nil)
		r.SetPathValue("version", v)
		r.SetPathValue("file", f)
		w := httptest.NewRecorder()
		h.releaseFile(w, r)
		return w
	}
	script := func() string {
		w := httptest.NewRecorder()
		h.installScript("sh")(w, httptest.NewRequest(http.MethodGet, "/install.sh", nil))
		return w.Body.String()
	}
	if s := script(); !strings.Contains(s, "VERSION='1.1.0'") {
		t.Fatalf("install.sh: %.300s", s)
	}
	// the data dir is lost (a restart): served from the blobs
	if err := os.RemoveAll(filepath.Join(dir, ReleasesDir)); err != nil {
		t.Fatal(err)
	}
	if w := get("1.1.0", "longarm-linux-amd64.deb"); w.Code != 200 || w.Body.String() != "deb1.1.0" {
		t.Fatalf("from blob: %d %q", w.Code, w.Body.String())
	}
	if w := get("1.1.0", "longarmd-linux-amd64.minisig"); w.Code != 200 || w.Body.String() != "s1.1.0" {
		t.Fatalf("signature: %d %q", w.Code, w.Body.String())
	}
	for _, bad := range [][2]string{{"1.1.0", "..%2Fhub.db"}, {"1.1.0", "hub.db"}, {"..", "longarmd-linux-amd64"}, {"9.9.9", "longarmd-linux-amd64"}, {"1.1.0", "longarmd-darwin-arm64"}} {
		if w := get(bad[0], bad[1]); w.Code != 404 {
			t.Fatalf("%v: %d", bad, w.Code)
		}
	}
	// 1.1.0's .deb is gone everywhere: install scripts fall back to 1.0.0
	delete(blob.blobs, "1.1.0/longarm-linux-amd64.deb")
	os.Remove(h.rel.localPath("1.1.0", "longarm-linux-amd64.deb")) // cached above
	h.rel.reconcile(ctx, h.store)
	if w := get("1.1.0", "longarm-linux-amd64.deb"); w.Code != 404 {
		t.Fatalf("missing file: %d", w.Code)
	}
	if s := script(); !strings.Contains(s, "VERSION='1.0.0'") {
		t.Fatalf("install.sh with 1.1.0 incomplete: %.300s", s)
	}
	if !strings.Contains(logs.String(), "RELEASE FILES MISSING") {
		t.Fatalf("no loud log: %s", logs)
	}
	// ListReleases: incomplete versions only on request
	as := &hubAdminServer{h: h}
	resp, err := as.ListReleases(ctx, &pb.ListReleasesRequest{})
	if err != nil || len(resp.Releases) != 1 || resp.Releases[0].Version != "1.0.0" {
		t.Fatalf("ListReleases: %v %v", resp, err)
	}
	resp, err = as.ListReleases(ctx, &pb.ListReleasesRequest{IncludeIncomplete: true})
	if err != nil || len(resp.Releases) != 2 {
		t.Fatalf("ListReleases include_incomplete: %v %v", resp, err)
	}
	for _, f := range resp.Releases[0].Files {
		if f.Missing != (f.Name == "longarm-linux-amd64.deb") {
			t.Fatalf("1.1.0 %s missing=%v", f.Name, f.Missing)
		}
	}
	if _, err := h.pushRelease(&pb.PushReleaseRequest{Version: "1.1.0", AllowNone: true}, "test"); err == nil || !strings.Contains(err.Error(), "lost files") {
		t.Fatalf("push of an incomplete release: %v", err)
	}
	// nothing complete: no release
	delete(blob.blobs, "1.0.0/longarmd-linux-amd64")
	h.rel.reconcile(ctx, h.store)
	if s := script(); !strings.Contains(s, "VERSION=''") {
		t.Fatalf("install.sh with nothing complete: %.300s", s)
	}
}

func TestReleasesBlobURLConfig(t *testing.T) {
	t.Setenv(EnvReleasesBlobURL, "https://acct.blob.core.windows.net/releases")
	cfg := &Config{DataDir: t.TempDir()}
	if err := cfg.applyDefaults(); err != nil || cfg.Releases.BlobURL != "https://acct.blob.core.windows.net/releases" {
		t.Fatalf("env: %q %v", cfg.Releases.BlobURL, err)
	}
	rs, err := openReleaseStore(cfg, nil, nil)
	if err != nil || rs.blob == nil || rs.blob.(*diag.BlobStore).ContentType != "application/octet-stream" {
		t.Fatalf("store: %+v %v", rs, err)
	}
	t.Setenv(EnvReleasesBlobURL, "https://acct.blob.core.windows.net/releases?sig=x")
	if err := (&Config{DataDir: t.TempDir()}).applyDefaults(); err == nil || !strings.Contains(err.Error(), "[releases] blob_url") {
		t.Fatalf("SAS in the URL: %v", err)
	}
}
