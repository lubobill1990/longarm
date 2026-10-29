// longarm-tray is the desktop tray app of Longarm (DESIGN §11.3): one app for
// both roles, with tabs by the installed roles. It is only an API client:
// the client tab talks to longarm daemon's local control API, the worker tab to
// longarmd's, through a loopback server that also serves the shared web UI
// (internal/webui). The tray icon shows one CPU/memory column per worker,
// left click opens the popup (/tray), notifications report workers going
// offline / back, memory >= 90 %, a link relayed for > 5 min and this
// client's jobs ending.
//
//	longarm-tray                    tray icon + popup (Wails: WebView2 / WKWebView / WebKitGTK)
//	longarm-tray --browser          no webview: serve and open the page in the default browser
//	longarm-tray --serve ADDR       serve only (development, tests): prints the URL
//	longarm-tray --headless-smoke   start, connect to the local APIs, render the icon and one
//	                           frame of the popup into PNGs (--out DIR), print a JSON report, exit
//	longarm-tray --signin           (after an install) open the popup at once when this machine
//	                           still needs a sign-in ("Sign in with Microsoft")
//	longarm-tray --worker-host      no UI: host the user-session worker (Windows; the scheduled
//	                           task's action): run longarmd, restart it when it exits
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/stopsignal"
	"github.com/lubobill1990/longarm/internal/telemetry"
	"github.com/lubobill1990/longarm/internal/tray"
	"github.com/lubobill1990/longarm/internal/version"
)

func main() {
	var o tray.Options
	var serve, out string
	var smoke, browser, showVersion, workerHost bool
	flag.StringVar(&o.LongarmConfigDir, "config-dir", client.DefaultConfigDir(), "longarm's config dir (longarm daemon); env LONGARM_CONFIG_DIR")
	flag.StringVar(&o.LongarmdDataDir, "data-dir", longarmd.DefaultDataDir(), "longarmd's data dir (worker tab); env LONGARM_DATA_DIR")
	flag.StringVar(&o.HubURL, "hub-url", os.Getenv("LONGARM_HUB_URL"), "the hub's web panel URL (default: https://<hub host>/)")
	flag.StringVar(&o.Roles, "roles", "", "client,worker (default: detected)")
	flag.StringVar(&serve, "serve", "", "only serve the UI on this loopback address (e.g. 127.0.0.1:7490) and print its URL")
	flag.BoolVar(&smoke, "headless-smoke", false, "start without a desktop, check the local APIs, render PNGs, exit")
	flag.StringVar(&out, "out", ".", "directory for --headless-smoke PNGs")
	flag.BoolVar(&browser, "browser", false, "no webview: open the popup page in the default browser")
	flag.BoolVar(&showVersion, "version", false, "print the version")
	flag.BoolVar(&o.ShowSignIn, "signin", false, "open the popup at once when this machine still needs a sign-in")
	flag.BoolVar(&workerHost, "worker-host", false, "no UI: host the user-session worker (run longarmd, restart it when it exits)")
	flag.Parse()
	if showVersion {
		fmt.Println("longarm-tray", version.Version)
		return
	}
	if workerHost {
		// a GUI-subsystem binary: the logon task opens no console window
		os.Exit(runWorkerHost(o.LongarmdDataDir))
	}
	o.Version = version.Version
	o.LongarmExe, o.LongarmdExe = sibling("longarm"), sibling("longarmd")
	o.Log = slog.New(telemetry.NewHandler(slog.NewTextHandler(os.Stderr, nil)))
	// Telemetry goes to longarm's queue; longarm daemon sends it.
	tcfg, _ := client.TelemetryConfig(o.LongarmConfigDir, "longarm-tray")
	tel := telemetry.Start(tcfg)
	defer tel.WatchCrashes()()
	tel.Event(context.Background(), "process.start", nil, nil)
	defer func() {
		tel.Event(context.Background(), "process.stop", nil, map[string]float64{"uptime_s": tel.Uptime().Seconds()})
		tel.Close()
	}()
	startDaemon := func() error {
		cmd := exec.Command(o.LongarmExe, "--config-dir", o.LongarmConfigDir, "daemon", "start")
		b, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("longarm daemon start: %v: %s", err, b)
		}
		if out := strings.TrimSpace(string(b)); out != "" && out != "longarm daemon running" {
			o.Log.Info("longarm daemon start", "output", out) // e.g. an older daemon replaced
		}
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// the installer asks the tray to exit before an upgrade (its stop
	// event), and so do Windows' end of session and Restart Manager
	// (watchEndSession): the installer starts it again afterwards
	ctx, stopEv := stopsignal.NotifyContext(ctx)
	defer stopEv()
	go tel.Run(ctx)

	switch {
	case smoke:
		res, err := tray.Smoke(ctx, o, out, startDaemon)
		if res != nil {
			b, _ := json.MarshalIndent(res, "", "  ")
			fmt.Println(string(b))
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "longarm-tray smoke:", err)
			os.Exit(1)
		}
	case serve != "" || browser:
		if err := serveOnly(ctx, o, serve, browser, startDaemon); err != nil {
			fmt.Fprintln(os.Stderr, "longarm-tray:", err)
			os.Exit(1)
		}
	default:
		defer watchEndSession(stopEv)()
		if err := runGUI(ctx, o, startDaemon); err != nil {
			fmt.Fprintln(os.Stderr, "longarm-tray:", err)
			os.Exit(1)
		}
	}
}

// runWorkerHost hosts the user-session worker of dataDir until the logon
// session ends (longarmd.SessionHost).
func runWorkerHost(dataDir string) int {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return 1
	}
	h, done, err := longarmd.NewSessionHost(dir, sibling("longarmd"))
	if err != nil {
		return 1
	}
	defer done()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, stopEv := stopsignal.NotifyContext(ctx) // the installer's stop (upgrade)
	defer stopEv()
	if err := h.Run(ctx); err != nil && !errors.Is(err, longarmd.ErrHostRunning) {
		h.Log.Error("worker host", "err", err)
		return 1
	}
	return 0
}

// sibling finds longarm / longarmd next to longarm-tray, else on PATH.
func sibling(name string) string {
	exe := name
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	if self, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(self), exe)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

// browserDesk is the desktop of the browser / serve modes: no window.
type browserDesk struct{}

func (browserDesk) Hide()          {}
func (browserDesk) SetPinned(bool) {}
func (browserDesk) Pinned() bool   { return true }

type logShell struct{ log *slog.Logger }

func (logShell) SetIcon([]byte)    {}
func (logShell) SetTooltip(string) {}
func (s logShell) Notify(title, body string) {
	s.log.Info("notification", "title", title, "body", body)
}

// serveOnly serves the UI without a webview (and opens it in the browser).
func serveOnly(ctx context.Context, o tray.Options, addr string, open bool, startDaemon func() error) error {
	srv, err := tray.NewServer(o, browserDesk{}, addr)
	if err != nil {
		return err
	}
	go srv.Serve()
	defer srv.Close()
	url := srv.URL("/tray")
	fmt.Println(url)
	if open {
		if err := tray.OpenURL(url); err != nil {
			fmt.Fprintln(os.Stderr, "open browser:", err)
		}
	}
	ctrl := tray.NewController(o, logShell{o.Log}, startDaemon)
	go ctrl.Run(ctx)
	<-ctx.Done()
	time.Sleep(50 * time.Millisecond)
	return nil
}
