package tray

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/uipage"
)

// TestUIOpen: `longarm ui open` over the control channel - the page in the
// shell's windows, else in the browser; --print-url mints a URL that works
// once; bad pages are refused; one tray serves the channel.
func TestUIOpen(t *testing.T) {
	cfg := t.TempDir()
	s, err := NewServer(Options{LongarmConfigDir: cfg, LongarmdDataDir: t.TempDir(), Roles: "client"}, &fakeDesk{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var opened []string
	s.openURL = func(u string) error { opened = append(opened, u); return nil }
	go s.Serve()
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.ServeControl(ctx) }()
	addrFile := uipage.AddrFile(cfg)
	open := func(req uipage.Request) (uipage.Result, error) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			res, err := uipage.Open(context.Background(), addrFile, req)
			if !errors.Is(err, uipage.ErrNotRunning) || time.Now().After(deadline) {
				return res, err
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	base := "http://" + s.Addr()

	// no windows (--browser): the default browser, with a one-time code
	res, err := open(uipage.Request{Page: "jobs", Target: uipage.Target{Job: "01JZABC"}})
	if err != nil || !res.Opened || res.URL != base+"/tray?job=01JZABC&tab=jobs" {
		t.Fatalf("jobs: %+v %v", res, err)
	}
	if len(opened) != 1 || !strings.HasPrefix(opened[0], base+"/tray?job=01JZABC&tab=jobs&token=") {
		t.Fatalf("browser: %v", opened)
	}
	if res, err = open(uipage.Request{Page: "sessions", Target: uipage.Target{Worker: "w1", Job: "x7k2"}}); err != nil || !res.Opened ||
		res.URL != base+"/sessions#job=x7k2&worker=w1" || len(opened) != 2 || !strings.HasSuffix(opened[1], "#job=x7k2&worker=w1") {
		t.Fatalf("sessions in the browser: %+v %v %v", res, err, opened)
	}

	// the shell's windows
	var popup, hashes []string
	s.ShowPopup = func(p string) { popup = append(popup, p) }
	s.OpenSessions = func(h string) { hashes = append(hashes, h) }
	open(uipage.Request{Page: "add-machine"})
	open(uipage.Request{})
	open(uipage.Request{Page: "sessions", Target: uipage.Target{Session: "dev"}})
	if strings.Join(popup, " ") != "/tray?add=1&tab=pool /tray?tab=pool" || strings.Join(hashes, " ") != "session=dev" || len(opened) != 2 {
		t.Fatalf("windows: popup %q sessions %q browser %v", popup, hashes, opened)
	}

	// --print-url: nothing opens; the URL signs one browser in, once
	res, err = open(uipage.Request{Page: "settings", PrintURL: true})
	if err != nil || res.Opened || !strings.HasPrefix(res.URL, base+"/tray?tab=settings&token=") || len(popup) != 2 || len(opened) != 2 {
		t.Fatalf("print-url: %+v %v", res, err)
	}
	jar, _ := cookiejar.New(nil)
	resp, err := (&http.Client{Jar: jar}).Get(res.URL)
	if err != nil || resp.StatusCode != 200 || resp.Request.URL.RequestURI() != "/tray?tab=settings" {
		t.Fatalf("the printed URL: %v %v", err, resp)
	}
	resp.Body.Close()
	if resp, err := http.Get(res.URL); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the printed URL again: %v %v", err, resp)
	}
	if res, err = open(uipage.Request{Page: "sessions", Target: uipage.Target{Worker: "w1"}, PrintURL: true}); err != nil ||
		!strings.HasPrefix(res.URL, base+"/sessions?token=") || !strings.HasSuffix(res.URL, "#worker=w1") {
		t.Fatalf("print-url sessions: %+v %v", res, err)
	}

	// refused: an unknown page (the list of pages), a flag the page doesn't take
	if _, err := open(uipage.Request{Page: "pool"}); err == nil || !strings.Contains(err.Error(), "pages: workers, add-machine") {
		t.Fatalf("unknown page: %v", err)
	}
	if _, err := open(uipage.Request{Page: "workers", Target: uipage.Target{Worker: "w1"}}); err == nil || !strings.Contains(err.Error(), "--worker") {
		t.Fatalf("flag: %v", err)
	}
	if len(popup) != 2 || len(hashes) != 1 || len(opened) != 2 {
		t.Fatal("opened something for a refused request")
	}

	// a second tray leaves the channel to the first
	if err := s.ServeControl(context.Background()); err == nil || !strings.Contains(err.Error(), "another longarm-tray") {
		t.Fatalf("second tray: %v", err)
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(addrFile); !os.IsNotExist(err) {
		t.Fatalf("tray.addr left behind: %v", err)
	}
}
