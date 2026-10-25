package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	traypkg "github.com/lubobill1990/longarm/internal/tray"
)

var trayBin struct {
	once sync.Once
	path string
	err  error
	skip string
}

func buildTray(t *testing.T) string {
	t.Helper()
	trayBin.once.Do(func() {
		trayBin.path = filepath.Join(bins.dir, exeName("longarm-tray"))
		goBin := filepath.Join(runtime.GOROOT(), "bin", exeName("go"))
		if _, err := os.Stat(goBin); err != nil {
			goBin = "go"
		}
		cmd := exec.Command(goBin, "build", "-o", trayBin.path, "../cmd/longarm-tray")
		// The tests drive longarm-tray headless (--headless-smoke, --serve), which
		// needs no webview: outside Windows build the no-cgo variant, so a
		// machine with gcc but without the GTK4/WebKitGTK 6 dev packages
		// still runs them. LONGARM_TRAY_CGO=1 builds the native (Wails/cgo)
		// tray instead, if pkg-config finds its libraries.
		if runtime.GOOS != "windows" {
			if os.Getenv("LONGARM_TRAY_CGO") == "1" {
				if runtime.GOOS == "linux" {
					if b, err := exec.Command("pkg-config", "--exists", "--print-errors", "gtk4", "webkitgtk-6.0", "libsoup-3.0").CombinedOutput(); err != nil {
						trayBin.skip = "LONGARM_TRAY_CGO=1 but pkg-config can't find gtk4/webkitgtk-6.0/libsoup-3.0 " +
							"(apt install libgtk-4-dev libwebkitgtk-6.0-dev libsoup-3.0-dev): " + strings.TrimSpace(string(b)) + " " + err.Error()
						return
					}
				}
				if runtime.GOOS == "linux" {
					cmd = exec.Command(goBin, "build", "-tags", "traynative", "-o", trayBin.path, "../cmd/longarm-tray")
				}
				cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
			} else {
				cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			}
		}
		if b, err := cmd.CombinedOutput(); err != nil {
			trayBin.err = fmt.Errorf("build longarm-tray: %v\n%s", err, b)
		}
	})
	if trayBin.skip != "" {
		t.Skip(trayBin.skip)
	}
	if trayBin.err != nil {
		t.Fatal(trayBin.err)
	}
	// longarm-tray finds longarm / longarmd next to itself
	return trayBin.path
}

// TestUITraySmoke: `longarm-tray --headless-smoke` against a live stack (hub, two
// workers, alice's longarm daemon started by longarm-tray itself): both local APIs
// reached, the icon rendered from the pool view, and one frame of the
// popup page rendered by a headless Edge/Chrome when installed.
func TestUITraySmoke(t *testing.T) {
	t.Parallel()
	s := newUIStack(t)
	tray := buildTray(t)
	t.Cleanup(func() { s.longarm(s.client, "daemon", "stop") })
	out := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// The daemon is not running yet: the smoke run starts it (longarm daemon start).
	cmd := exec.CommandContext(ctx, tray, "--headless-smoke", "--config-dir", s.client, "--data-dir", s.wa.dataDir,
		"--roles", "client,worker", "--out", out)
	cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+s.client, "LONGARM_POOL_INTERVAL=500ms")
	// A browser that can't render even a blank page here (Chrome under a
	// macOS CI runner's launchd service dies in its path service) can't
	// take the frame: the smoke runs without one. macOS only - elsewhere a
	// failing screenshot is a failure.
	if b := traypkg.FindBrowser(); b != "" && runtime.GOOS == "darwin" {
		if err := traypkg.Screenshot(ctx, b, "data:text/html,<p>probe</p>", filepath.Join(t.TempDir(), "probe.png"), 500, 300); err != nil {
			t.Logf("headless %s does not run here, no page frame: %v", b, err)
			cmd.Env = append(cmd.Env, "LONGARM_TRAY_BROWSER=none")
		}
	}
	b, err := cmd.Output()
	if err != nil {
		ee, _ := err.(*exec.ExitError)
		var stderr []byte
		if ee != nil {
			stderr = ee.Stderr
		}
		t.Fatalf("smoke: %v\n%s\n%s", err, b, stderr)
	}
	var res struct {
		Daemon   string   `json:"longarm_daemon"`
		Longarmd string   `json:"longarmd"`
		IconPNG  string   `json:"icon_png"`
		PagePNG  string   `json:"page_png"`
		Browser  string   `json:"browser"`
		Problems []string `json:"problems"`
	}
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("smoke output: %v\n%s", err, b)
	}
	t.Logf("smoke: %s", b)
	if !strings.HasPrefix(res.Daemon, "ok: 2 workers") || !strings.HasPrefix(res.Longarmd, "ok: wa") {
		t.Fatalf("local APIs: %+v", res)
	}
	if fi, err := os.Stat(res.IconPNG); err != nil || fi.Size() < 100 {
		t.Fatalf("icon png: %v", err)
	}
	if res.Browser != "" {
		fi, err := os.Stat(res.PagePNG)
		if err != nil || fi.Size() < 1000 {
			t.Fatalf("page png: %v", err)
		}
		if os.Getenv("LONGARM_SCREENSHOTS") == "1" {
			b, _ := os.ReadFile(res.PagePNG)
			os.WriteFile(filepath.Join("..", "docs", "ui-screenshots", "tray-popup-smoke.png"), b, 0o644)
		}
	} else {
		t.Log("no headless browser here: page frame not rendered")
	}
}

// startTrayServe runs `longarm-tray --serve` and returns its URL (with token).
func startTrayServe(t *testing.T, s *uiStack, dataDir, roles string) string {
	t.Helper()
	tray := buildTray(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, tray, "--serve", "127.0.0.1:0", "--config-dir", s.client, "--data-dir", dataDir, "--roles", roles)
	cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+s.client)
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	line := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		if sc.Scan() {
			line <- sc.Text()
		}
		io.Copy(io.Discard, stdout)
	}()
	select {
	case u := <-line:
		return u
	case <-time.After(30 * time.Second):
		t.Fatal("longarm-tray --serve printed no URL")
	}
	return ""
}

// TestUITrayServe: the tray's server with a real daemon and longarmd behind it:
// pool through the proxy (SSE), worker tab actions (drain, limits) through
// longarmd's local API, and the transport policy setting.
func TestUITrayServe(t *testing.T) {
	t.Parallel()
	s := newUIStack(t)
	t.Cleanup(func() { s.longarm(s.client, "daemon", "stop") })
	s.mustLongarm(s.client, "daemon", "start")
	u := startTrayServe(t, s, s.wa.dataDir, "client,worker")
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	resp, err := c.Get(u)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("tray page: %v %v", err, resp)
	}
	resp.Body.Close()
	base := strings.SplitN(u, "/tray", 2)[0]
	do := func(method, p, body string) (int, string) {
		req, _ := http.NewRequest(method, base+p, strings.NewReader(body))
		if method != "GET" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, compactJSON(string(b))
	}
	if code, body := do("GET", "/trayapi/info", ""); code != 200 || !strings.Contains(body, `"worker":{"data_dir"`) || !strings.Contains(body, `"enrolled":true`) {
		t.Fatalf("info %d %s", code, body)
	}
	waitFor(t, "pool via the tray proxy", 30*time.Second, func() bool {
		_, body := do("GET", "/local/longarm/v1/pool", "")
		return strings.Contains(body, `"name":"wa"`) && strings.Contains(body, `"name":"wb"`) && strings.Contains(body, `"state":"online"`)
	})
	evs := sseEvents(t, c, base+"/local/longarm/v1/pool/watch", 5*time.Second)
	if len(evs) == 0 || !strings.HasPrefix(evs[0], "pool ") {
		t.Fatalf("pool SSE through the proxy: %v", evs)
	}
	if code, body := do("POST", "/local/longarmd/v1/drain", `{"draining":true,"reason":"tray"}`); code != 200 || !strings.Contains(body, `"draining":true`) {
		t.Fatalf("drain via tray %d %s", code, body)
	}
	s.waitNode(t, "wa", "drained by the tray", func(n map[string]any) bool { return isTrue(n["draining"]) && n["drain_reason"] == "tray" })
	do("POST", "/local/longarmd/v1/drain", `{"draining":false}`)
	if code, body := do("POST", "/local/longarmd/v1/limits", `{"max_jobs":3,"reserved_mem_gb":8}`); code != 200 || !strings.Contains(body, `"max_jobs":3`) {
		t.Fatalf("limits via tray %d %s", code, body)
	}
	if code, body := do("POST", "/local/longarm/v1/settings", `{"transport":"race"}`); code != 200 || !strings.Contains(body, `"race"`) {
		t.Fatalf("transport via tray %d %s", code, body)
	}
	if code, body := do("POST", "/local/longarm/v1/settings", `{"transport":"bogus"}`); code != 400 || !strings.Contains(body, "transport policy") {
		t.Fatalf("bad transport policy %d %s", code, body)
	}
}
