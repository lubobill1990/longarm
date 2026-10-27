// Package gitserver is a test git remote over https: git http-backend (smart
// HTTP, protocol v2) behind an httptest TLS server that requires a token -
// "Authorization: Bearer <token>" or basic auth with the token as password
// - plus a minimal Git LFS batch API. For tests only: no network beyond
// 127.0.0.1, nothing real.
package gitserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Server serves the bare repositories under Root at /<name>.git.
type Server struct {
	*httptest.Server
	Root  string
	Token string

	backend http.Handler
	mu      sync.Mutex
	lfs     map[string][]byte
	stats   Stats
}

// Stats counts requests.
type Stats struct {
	Authorized   int // carried the token
	Unauthorized int // refused with 401
	LFSBatch     int
	LFSDownloads int
	// UploadPackBytes is what the server sent for fetches (git-upload-pack
	// responses).
	UploadPackBytes int64
}

// New starts a server for the bare repositories in root (closed with t).
func New(t testing.TB, root, token string) *Server {
	t.Helper()
	execPath, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git not installed")
	}
	backend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if runtime.GOOS == "windows" {
		backend += ".exe"
	}
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("no git-http-backend: %v", err)
	}
	s := &Server{Root: root, Token: token, lfs: map[string][]byte{}}
	s.backend = &cgi.Handler{
		Path:       backend,
		Env:        []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
		InheritEnv: []string{"PATH", "SYSTEMROOT", "TEMP", "TMP", "HOME", "USERPROFILE", "LD_LIBRARY_PATH"},
	}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// RepoURL is the https URL of the repository <name>.git.
func (s *Server) RepoURL(name string) string { return s.URL + "/" + name + ".git" }

// Stats returns the counters so far.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// AddLFS stores an LFS object and returns its oid.
func (s *Server) AddLFS(content []byte) string {
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	s.mu.Lock()
	s.lfs[oid] = bytes.Clone(content)
	s.mu.Unlock()
	return oid
}

func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if scheme, v, ok := strings.Cut(h, " "); ok {
		switch strings.ToLower(scheme) {
		case "bearer":
			return v == s.Token
		case "basic":
			b, err := base64.StdEncoding.DecodeString(v)
			if err == nil {
				_, pw, _ := strings.Cut(string(b), ":")
				return pw == s.Token
			}
		}
	}
	return false
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.mu.Lock()
		s.stats.Unauthorized++
		s.mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Basic realm="gitserver"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	s.stats.Authorized++
	s.mu.Unlock()
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/info/lfs/objects/batch") && r.Method == http.MethodPost:
		s.batch(w, r, strings.TrimSuffix(p, "/objects/batch"))
	case strings.Contains(p, "/info/lfs/objects/") && r.Method == http.MethodGet:
		oid := p[strings.LastIndex(p, "/")+1:]
		s.mu.Lock()
		b, ok := s.lfs[oid]
		if ok {
			s.stats.LFSDownloads++
		}
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	default:
		cw := &countingWriter{ResponseWriter: w}
		s.backend.ServeHTTP(cw, r)
		if strings.HasSuffix(p, "/git-upload-pack") {
			s.mu.Lock()
			s.stats.UploadPackBytes += cw.n
			s.mu.Unlock()
		}
	}
}

func (s *Server) batch(w http.ResponseWriter, r *http.Request, lfsBase string) {
	var req struct {
		Operation string `json:"operation"`
		Objects   []struct {
			Oid  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Operation != "download" {
		http.Error(w, "bad batch request", http.StatusBadRequest)
		return
	}
	type action struct {
		Href string `json:"href"`
	}
	type object struct {
		Oid     string            `json:"oid"`
		Size    int64             `json:"size"`
		Actions map[string]action `json:"actions,omitempty"`
		Error   map[string]any    `json:"error,omitempty"`
	}
	var out struct {
		Transfer string   `json:"transfer"`
		Objects  []object `json:"objects"`
	}
	out.Transfer = "basic"
	s.mu.Lock()
	s.stats.LFSBatch++
	for _, o := range req.Objects {
		obj := object{Oid: o.Oid, Size: o.Size}
		if _, ok := s.lfs[o.Oid]; ok {
			obj.Actions = map[string]action{"download": {Href: s.URL + lfsBase + "/objects/" + o.Oid}}
		} else {
			obj.Error = map[string]any{"code": 404, "message": "not found"}
		}
		out.Objects = append(out.Objects, obj)
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	json.NewEncoder(w).Encode(out)
}

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

// LFSPointer is the pointer file of content.
func LFSPointer(content []byte) []byte {
	sum := sha256.Sum256(content)
	return []byte("version https://git-lfs.github.com/spec/v1\noid sha256:" + hex.EncodeToString(sum[:]) +
		"\nsize " + strconv.Itoa(len(content)) + "\n")
}
