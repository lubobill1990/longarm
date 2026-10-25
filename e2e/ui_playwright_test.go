package e2e

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestUIPlaywright drives the web UI in headless Chromium (web/e2e/ui.spec.ts)
// against a real longarm-hub with workers wa, wb and a pending wp, alice's longarm
// daemon and longarm-tray --serve: TOTP login, approving wp, live metrics,
// submitting / following / killing jobs, every panel page and the tray
// tabs. Screenshots go to docs/ui-screenshots/.
//
// Opt-in (needs node, pnpm install in web/ and `npx playwright install chromium`):
//
//	LONGARM_PLAYWRIGHT=1 go test -run TestUIPlaywright -v ./e2e
func TestUIPlaywright(t *testing.T) {
	if os.Getenv("LONGARM_PLAYWRIGHT") != "1" {
		t.Skip("set LONGARM_PLAYWRIGHT=1 (needs node + Playwright Chromium in web/)")
	}
	s := newUIStack(t)
	wp := s.newWorkerDir(t, "wp", "")
	s.joinWorker(t, wp, false)
	wp.start(t, false) // waits for approval
	s.waitNode(t, "wp", "pending", func(n map[string]any) bool { return n["state"] == "pending" })

	t.Cleanup(func() { s.longarm(s.client, "daemon", "stop") })
	os.Setenv("LONGARM_POOL_INTERVAL", "1s")
	s.mustLongarm(s.client, "daemon", "start")
	// alice's own job (the tray's "My jobs") and a link report at the hub
	s.mustLongarm(s.client, "run", "wa", "--name", "alice-build", "--", bins.helper, "print", "3", "0", "0")
	waitFor(t, "alice's link report", 40*time.Second, func() bool {
		r := s.longarm(s.admin, "admin", "links")
		return strings.Contains(r.stdout, "alice")
	})
	// something to forward to on "wa" (the workers run on this machine)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "HTTP/1.0 200 OK\r\n\r\nhello\n")
			c.Close()
		}
	}()
	trayURL := startTrayServe(t, s, s.wa.dataDir, "client,worker")

	web, _ := filepath.Abs(filepath.Join("..", "web"))
	shots, _ := filepath.Abs(filepath.Join("..", "docs", "ui-screenshots"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/c", "npx", "playwright", "test")
	} else {
		cmd = exec.CommandContext(ctx, "npx", "playwright", "test")
	}
	cmd.Dir = web
	cmd.Env = append(os.Environ(),
		"LONGARM_TEST_HUB_URL="+s.httpURL(), "LONGARM_TEST_USER="+s.user, "LONGARM_TEST_PASS="+s.pass, "LONGARM_TEST_TOTP="+s.secret,
		"LONGARM_TEST_TRAY_URL="+trayURL, "LONGARM_TEST_HELPER="+bins.helper, "LONGARM_TEST_PENDING=wp", "LONGARM_TEST_SHOTS="+shots,
		"LONGARM_TEST_FORWARD_PORT="+strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	out, err := cmd.CombinedOutput()
	t.Logf("playwright:\n%s", out)
	if err != nil {
		t.Fatalf("playwright: %v", err)
	}
}
