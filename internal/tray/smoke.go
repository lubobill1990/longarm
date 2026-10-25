package tray

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/localapi"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// SmokeResult is what `longarm-tray --headless-smoke` checked.
type SmokeResult struct {
	URL      string   `json:"url"`
	Client   bool     `json:"client_role"`
	Worker   bool     `json:"worker_role"`
	Daemon   string   `json:"longarm_daemon"` // "ok: N workers, source S" or the error
	Longarmd string   `json:"longarmd"`
	Tooltip  string   `json:"tooltip,omitempty"`
	IconPNG  string   `json:"icon_png,omitempty"`
	PagePNG  string   `json:"page_png,omitempty"`
	Browser  string   `json:"browser,omitempty"`
	Problems []string `json:"problems,omitempty"`
}

type smokeDesk struct{}

func (smokeDesk) Hide()          {}
func (smokeDesk) SetPinned(bool) {}
func (smokeDesk) Pinned() bool   { return false }

// Smoke starts the tray's server without a desktop, connects to the local
// APIs, renders the tray icon and one frame of the popup page (/tray) into
// PNGs in outDir with a headless Chromium/Edge (if one is installed), and
// returns what worked. err is set when an installed role's API is
// unreachable, the page does not load, or the screenshot fails.
func Smoke(ctx context.Context, opts Options, outDir string, startDaemon func() error) (*SmokeResult, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	s, err := NewServer(opts, smokeDesk{}, "")
	if err != nil {
		return nil, err
	}
	s.SetHeadless(true)
	go s.Serve()
	defer s.Close()
	res := &SmokeResult{URL: "http://" + s.Addr() + "/tray"}
	res.Client, res.Worker = s.Roles()
	problem := func(f string, a ...any) { res.Problems = append(res.Problems, fmt.Sprintf(f, a...)) }

	if res.Client {
		v, err := smokePool(ctx, opts)
		if err != nil && startDaemon != nil {
			if serr := startDaemon(); serr == nil {
				v, err = smokePool(ctx, opts)
			}
		}
		if err != nil {
			res.Daemon = "unreachable: " + err.Error()
			problem("longarm daemon: %v", err)
		} else {
			res.Daemon = fmt.Sprintf("ok: %d workers, source %s", len(v.Workers), v.Source)
			res.Tooltip = Tooltip(v)
			res.IconPNG = filepath.Join(outDir, "tray-icon-smoke.png")
			if err := os.WriteFile(res.IconPNG, IconPNG(IconWorkers(v)), 0o644); err != nil {
				problem("icon: %v", err)
			}
		}
	}
	if res.Worker {
		cc, err := localapi.DialFile(filepath.Join(opts.LongarmdDataDir, "longarmd.local"), "longarmd")
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			var st *pb.WorkerStatus
			st, err = pb.NewLocalWorkerClient(cc).Status(cctx, &pb.LocalStatusRequest{})
			cancel()
			cc.Close()
			if err == nil {
				res.Longarmd = fmt.Sprintf("ok: %s, %d jobs running, hub connected=%v", st.Name, st.JobsRunning, st.Hub.GetConnected())
			}
		}
		if err != nil {
			res.Longarmd = "unreachable: " + err.Error()
			problem("longarmd: %v", err)
		}
	}

	// The page itself, through the token handshake.
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 15 * time.Second}
	resp, err := hc.Get(s.URL("/tray"))
	if err != nil {
		problem("page: %v", err)
	} else {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), `id="root"`) {
			problem("page: HTTP %d", resp.StatusCode)
		}
	}

	if b := FindBrowser(); b != "" {
		res.Browser = b
		res.PagePNG = filepath.Join(outDir, "tray-popup-smoke.png")
		if err := Screenshot(ctx, b, s.URL("/tray?poll=1"), res.PagePNG, 500, 900); err != nil { // Chromium windows are >= 500 px wide
			problem("screenshot: %v", err)
			res.PagePNG = ""
		}
	}
	if len(res.Problems) > 0 {
		return res, errors.New(strings.Join(res.Problems, "; "))
	}
	return res, nil
}

func smokePool(ctx context.Context, opts Options) (*pb.PoolView, error) {
	cc, err := localapi.DialFile(filepath.Join(opts.LongarmConfigDir, "daemon.addr"), "longarm daemon")
	if err != nil {
		return nil, err
	}
	defer cc.Close()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return pb.NewLocalClientClient(cc).Pool(cctx, &pb.PoolRequest{})
}

// FindBrowser finds a Chromium-based browser for headless screenshots:
// $LONGARM_TRAY_BROWSER ("none": no browser), Edge, Chrome or Chromium.
func FindBrowser() string {
	if b := os.Getenv("LONGARM_TRAY_BROWSER"); b != "" {
		if b == "none" {
			return ""
		}
		return b
	}
	var cands []string
	switch runtime.GOOS {
	case "windows":
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
			base := os.Getenv(env)
			if base == "" {
				continue
			}
			cands = append(cands, filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`), filepath.Join(base, `Google\Chrome\Application\chrome.exe`))
		}
	case "darwin":
		cands = []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge", "/Applications/Chromium.app/Contents/MacOS/Chromium"}
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	for _, n := range []string{"chromium", "chromium-browser", "google-chrome", "microsoft-edge"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// Screenshot renders url into a PNG with a headless browser. The virtual
// time budget lets the page fetch its data before the frame is taken.
func Screenshot(ctx context.Context, browser, url, out string, w, h int) error {
	prof, err := os.MkdirTemp("", "longarm-tray-shot-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(prof)
	os.Remove(out)
	abs, _ := filepath.Abs(out)
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, browser, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-extensions", "--hide-scrollbars", "--user-data-dir="+prof, fmt.Sprintf("--window-size=%d,%d", w, h),
		"--virtual-time-budget=8000", "--screenshot="+abs, url)
	b, err := cmd.CombinedOutput()
	if fi, serr := os.Stat(abs); serr == nil && fi.Size() > 0 {
		return nil
	}
	if err == nil {
		err = errors.New("no screenshot written")
	}
	return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(b)))
}
