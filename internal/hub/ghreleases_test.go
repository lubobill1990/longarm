package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// fakeGitHub serves a repository's releases API and its download URLs.
type fakeGitHub struct {
	srv       *httptest.Server
	releases  []map[string]any
	files     map[string]string // download path -> content
	apiHits   atomic.Int32
	downloads atomic.Int32
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{files: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/releases", func(w http.ResponseWriter, r *http.Request) {
		f.apiHits.Add(1)
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "no user agent", 403)
			return
		}
		json.NewEncoder(w).Encode(f.releases)
	})
	mux.HandleFunc("GET /download/", func(w http.ResponseWriter, r *http.Request) {
		c, ok := f.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.downloads.Add(1)
		io.WriteString(w, c)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// release adds tag with files (name -> content; .minisig files become
// signatures). digestOf overrides a file's advertised sha256 content.
func (f *fakeGitHub) release(tag string, pre, draft bool, files map[string]string, digestOf map[string]string) {
	var assets []map[string]any
	for name, c := range files {
		p := "/download/" + tag + "/" + name
		f.files[p] = c
		d := c
		if o, ok := digestOf[name]; ok {
			d = o
		}
		sum := sha256.Sum256([]byte(d))
		assets = append(assets, map[string]any{"name": name, "size": len(c), "digest": "sha256:" + hex.EncodeToString(sum[:]),
			"browser_download_url": f.srv.URL + p})
	}
	f.releases = append(f.releases, map[string]any{"tag_name": tag, "prerelease": pre, "draft": draft,
		"published_at": time.Now().UTC().Format(time.RFC3339), "assets": assets})
}

func newGHHub(t *testing.T, gh *fakeGitHub) *Hub {
	t.Helper()
	dir := t.TempDir()
	if _, err := InitCA(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvReleasesGitHub, "o/r")
	cfg := &Config{DataDir: dir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "off"}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	cfg.Releases.githubAPI = gh.srv.URL
	h, err := Open(cfg, slog.New(slog.NewTextHandler(&syncWriter{w: &bytes.Buffer{}}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	if h.gh == nil {
		t.Fatal("no GitHub releases")
	}
	return h
}

func TestGitHubReleasesConfig(t *testing.T) {
	for _, c := range []struct {
		env, repoURL, want string
		bad                bool
	}{
		{"", "", strings.TrimPrefix(DefaultRepoURL, "https://github.com/"), false}, // the public repository
		{"", "https://github.com/acme/longarm-fork", "acme/longarm-fork", false},
		{"", "https://git.example.com/acme/longarm", "", false}, // not GitHub: off
		{"off", "", "", false},
		{"acme/fork", "", "acme/fork", false},
		{"https://github.com/acme/fork.git", "", "acme/fork", false},
		{"not a repo", "", "", true},
	} {
		t.Setenv(EnvReleasesGitHub, c.env)
		if c.env == "" {
			os.Unsetenv(EnvReleasesGitHub)
		}
		cfg := &Config{DataDir: t.TempDir()}
		cfg.Web.RepoURL = c.repoURL
		err := cfg.applyDefaults()
		if (err != nil) != c.bad {
			t.Fatalf("%+v: %v", c, err)
		}
		if err != nil {
			continue
		}
		if got, _ := ghRepo(cfg); got != c.want {
			t.Errorf("%+v: repo %q", c, got)
		}
	}
}

// TestGitHubReleases: a hub without releases of its own offers GitHub's:
// latest.json, install scripts and channels see them, HTTP downloads go to
// GitHub, a worker's download (gRPC path) is fetched once, checked and
// cached, signatures come along; drafts and files without digests are
// left out; the hub's first own release turns GitHub's off.
func TestGitHubReleases(t *testing.T) {
	ctx := context.Background()
	gh := newFakeGitHub(t)
	gh.release("v1.2.0", false, false, map[string]string{"longarmd-linux-amd64": "daemon-1.2.0", "longarmd-linux-amd64.minisig": "sig-1.2.0",
		"longarm-windows-amd64.msi": "msi-1.2.0", "checksums.txt": "x"}, nil)
	gh.release("v1.3.0-rc.1", true, false, map[string]string{"longarm-windows-amd64.msi": "msi-1.3.0-rc.1"}, nil)
	gh.release("v1.1.0", false, false, map[string]string{"longarmd-linux-amd64": "daemon-1.1.0"}, map[string]string{"longarmd-linux-amd64": "tampered"})
	gh.release("v9.9.9", false, true, map[string]string{"longarmd-linux-amd64": "draft"}, nil)
	h := newGHHub(t, gh)
	// asked before the first poll (a hub just started): nothing - and not
	// cached past the poll
	if v, _ := h.latestNotice(); v != "" {
		t.Fatalf("latest before the poll: %s", v)
	}
	if err := h.gh.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := h.latestNotice(); v != "1.2.0" {
		t.Fatalf("latest right after the poll: %q", v)
	}
	latest := func(q string) (v string, files []string) {
		expireLatest(h)
		w := httptest.NewRecorder()
		h.latestJSON(w, httptest.NewRequest(http.MethodGet, "/releases/latest.json"+q, nil))
		var lj struct {
			Version string
			Files   []struct{ Name string }
		}
		json.Unmarshal(w.Body.Bytes(), &lj)
		for _, f := range lj.Files {
			files = append(files, f.Name)
		}
		return lj.Version, files
	}
	if v, files := latest(""); v != "1.2.0" || len(files) != 2 {
		t.Fatalf("latest: %s %v", v, files)
	}
	if v, _ := latest("?channel=preview"); v != "1.3.0-rc.1" {
		t.Fatalf("latest preview: %s", v)
	}
	w := httptest.NewRecorder()
	h.installScript("ps1")(w, httptest.NewRequest(http.MethodGet, "/install.ps1", nil))
	if !strings.Contains(w.Body.String(), "1.2.0") {
		t.Fatalf("install.ps1 without the version: %.300s", w.Body.String())
	}
	get := func(v, f string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/releases/"+v+"/"+f, nil)
		r.SetPathValue("version", v)
		r.SetPathValue("file", f)
		w := httptest.NewRecorder()
		h.releaseFile(w, r)
		return w
	}
	// HTTP downloads: to GitHub; the signature from the index
	if w := get("1.2.0", "longarm-windows-amd64.msi"); w.Code != http.StatusFound || w.Header().Get("Location") != gh.srv.URL+"/download/v1.2.0/longarm-windows-amd64.msi" {
		t.Fatalf("msi: %d %s", w.Code, w.Header().Get("Location"))
	}
	// the Windows update service follows no redirect off the hub: the hub
	// fetches the file (checked against the digest) and serves it
	{
		r := httptest.NewRequest(http.MethodGet, "/releases/1.2.0/longarm-windows-amd64.msi", nil)
		r.SetPathValue("version", "1.2.0")
		r.SetPathValue("file", "longarm-windows-amd64.msi")
		r.Header.Set("User-Agent", "longarm-update")
		w := httptest.NewRecorder()
		h.releaseFile(w, r)
		if w.Code != 200 || w.Body.String() != "msi-1.2.0" {
			t.Fatalf("update service msi: %d %q %s", w.Code, w.Body.String(), w.Header().Get("Location"))
		}
		gh.downloads.Add(-1) // the counts below are the worker's
	}
	if w := get("1.2.0", "longarmd-linux-amd64.minisig"); w.Code != 200 || w.Body.String() != "sig-1.2.0" {
		t.Fatalf("signature: %d %q", w.Code, w.Body.String())
	}
	if w := get("9.9.9", "longarmd-linux-amd64"); w.Code != 404 {
		t.Fatalf("draft: %d", w.Code)
	}
	if w := get("1.2.0", "checksums.txt"); w.Code != 404 {
		t.Fatalf("not a release file: %d", w.Code)
	}
	// a worker's download: fetched once, checked, then served from the cache
	p, err := h.releasePath(ctx, "1.2.0", "longarmd-linux-amd64")
	if err != nil || readAll(t, p) != "daemon-1.2.0" {
		t.Fatalf("worker download: %v", err)
	}
	if _, err := h.releasePath(ctx, "1.2.0", "longarmd-linux-amd64"); err != nil || gh.downloads.Load() != 2 { // + the .minisig at poll
		t.Fatalf("second download: %v, %d downloads", err, gh.downloads.Load())
	}
	if w := get("1.2.0", "longarmd-linux-amd64"); w.Code != 200 || w.Body.String() != "daemon-1.2.0" {
		t.Fatalf("cached file: %d %q", w.Code, w.Body.String())
	}
	if _, err := h.releasePath(ctx, "1.1.0", "longarmd-linux-amd64"); err == nil {
		t.Fatal("a file that doesn't match its digest was served")
	}
	// gRPC: the update check and the admin list see them
	as := &hubAdminServer{h: h}
	resp, err := as.ListReleases(ctx, &pb.ListReleasesRequest{})
	if err != nil || len(resp.Releases) != 3 {
		t.Fatalf("ListReleases: %v %v", resp, err)
	}
	// the hub publishes its own: GitHub's are gone
	if _, err := addRelease(ctx, h.rel, h.store, releaseDir(t, map[string]string{"longarmd-linux-amd64": "own"}), "2.0.0", "", "", false); err != nil {
		t.Fatal(err)
	}
	if v, _ := latest(""); v != "2.0.0" {
		t.Fatalf("latest with an own release: %s", v)
	}
	if w := get("1.2.0", "longarm-windows-amd64.msi"); w.Code != 404 {
		t.Fatalf("GitHub's file with an own release: %d", w.Code)
	}
	if h.ghWanted() {
		t.Fatal("still polling GitHub")
	}
}

func TestGitHubReleasesPollFailure(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.release("v1.2.0", false, false, map[string]string{"longarmd-linux-amd64": "d", "longarmd-linux-amd64.minisig": "s"}, nil)
	h := newGHHub(t, gh)
	if err := h.gh.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	// a signature that can't be read: the index stays as it was
	gh.release("v1.3.0", false, false, map[string]string{"longarmd-linux-amd64": "d2", "longarmd-linux-amd64.minisig": "s2"}, nil)
	delete(gh.files, "/download/v1.3.0/longarmd-linux-amd64.minisig")
	if err := h.gh.poll(context.Background()); err == nil {
		t.Fatal("poll with a missing signature succeeded")
	}
	if _, latest := h.gh.snapshot(); latest != "1.2.0" {
		t.Fatalf("index after a failed poll: %s", latest)
	}
}
