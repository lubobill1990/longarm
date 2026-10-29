package tray

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lubobill1990/longarm/internal/localapi"
	"github.com/lubobill1990/longarm/internal/platform"
)

// TestFirewallAllow: the firewall card's Allow runs the installed
// longarm.exe elevated with `firewall allow --program <the program the
// daemon reports>` - a copy another app ships included - then checks again;
// a reported path that is not an existing longarm/longarmd is refused
// before any administrator prompt, and a declined prompt changes nothing.
func TestFirewallAllow(t *testing.T) {
	install := filepath.Join(t.TempDir(), "Program Files", "Longarm")
	tmia := filepath.Join(t.TempDir(), "tmia-desktop-live", "longarm-1.2.3")
	for _, d := range []string{install, tmia} {
		os.MkdirAll(d, 0o755)
	}
	longarm := filepath.Join(install, "longarm.exe")
	copyExe := filepath.Join(tmia, "longarm.exe")
	other := filepath.Join(tmia, "evil.exe")
	for _, f := range []string{longarm, copyExe, other} {
		os.WriteFile(f, []byte("x"), 0o755)
	}

	cfg := t.TempDir()
	var mu sync.Mutex
	reported, reportedOK := copyExe, false
	ln, _, err := localapi.Listen("longarm", cfg, filepath.Join(cfg, "daemon.addr"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"identity": "client/alice", "firewall": map[string]any{"program": reported, "ok": reportedOK, "problem": "no allow rule"}})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	s, err := NewServer(Options{LongarmConfigDir: cfg, LongarmdDataDir: t.TempDir(), LongarmExe: longarm, LongarmdExe: "longarmd", Roles: "client"}, &fakeDesk{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var elevated [][]string
	var elevErr error
	allowed := map[string]bool{}
	s.runElevatedWait = func(exe string, args ...string) error {
		mu.Lock()
		defer mu.Unlock()
		elevated = append(elevated, append([]string{exe}, args...))
		if elevErr != nil {
			return elevErr
		}
		allowed[args[len(args)-1]] = true
		return nil
	}
	s.checkFirewall = func(p string) (platform.FirewallStatus, error) {
		mu.Lock()
		defer mu.Unlock()
		return platform.FirewallStatus{Program: p, Checked: true, Allowed: allowed[p]}, nil
	}
	go s.Serve()
	defer s.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if resp, err := c.Get(s.URL("/tray")); err != nil {
		t.Fatal(err)
	} else {
		resp.Body.Close()
	}
	call := func(body string, v any) int {
		t.Helper()
		req, _ := http.NewRequest("POST", "http://"+s.Addr()+"/trayapi/firewall/allow", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if v != nil {
			json.NewDecoder(resp.Body).Decode(v)
		}
		return resp.StatusCode
	}

	var info struct {
		FirewallFix bool   `json:"firewall_fix"`
		InstallDir  string `json:"install_dir"`
	}
	if resp, err := c.Get("http://" + s.Addr() + "/trayapi/info"); err == nil {
		json.NewDecoder(resp.Body).Decode(&info)
		resp.Body.Close()
	}
	if !info.FirewallFix || info.InstallDir != install {
		t.Fatalf("info: %+v", info)
	}

	// refused before any prompt
	if code := call(`{"for":"other"}`, nil); code != 400 {
		t.Fatalf("for other: %d", code)
	}
	for _, bad := range []string{other, filepath.Join(tmia, "missing", "longarm.exe"), "longarm.exe"} {
		mu.Lock()
		reported = bad
		mu.Unlock()
		if code := call(`{"for":"client"}`, nil); code != 400 {
			t.Fatalf("reported %s: %d", bad, code)
		}
	}
	if len(elevated) != 0 {
		t.Fatalf("elevated for a refused program: %q", elevated)
	}

	// declined
	mu.Lock()
	reported, elevErr = copyExe, ErrElevationCanceled
	mu.Unlock()
	if code := call(`{"for":"client"}`, nil); code != 409 || len(allowed) != 0 {
		t.Fatalf("declined: %d %v", code, allowed)
	}

	// allowed: the installed longarm.exe runs, for the copy the daemon runs from
	mu.Lock()
	elevErr = nil
	mu.Unlock()
	var res FirewallFix
	if code := call(`{"for":"client"}`, &res); code != 200 || !res.OK || res.Program != copyExe {
		t.Fatalf("allow: %d %+v", code, res)
	}
	last := elevated[len(elevated)-1]
	if want := fmt.Sprint([]string{longarm, "firewall", "allow", "--program", copyExe}); fmt.Sprint(last) != want {
		t.Fatalf("elevated %q, want %s", last, want)
	}

	// nothing to fix: no prompt
	n := len(elevated)
	mu.Lock()
	reportedOK = true
	mu.Unlock()
	if code := call(`{"for":"client"}`, &res); code != 200 || !res.OK || len(elevated) != n {
		t.Fatalf("ok already: %d %+v %d", code, res, len(elevated))
	}
	// longarmd isn't running
	if code := call(`{"for":"worker"}`, nil); code != 502 {
		t.Fatalf("worker not running: %d", code)
	}
}
