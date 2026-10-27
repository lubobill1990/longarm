package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hub"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/signin"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// testHub runs a longarm-hub in-process (extra: more hub.toml).
func testHub(t *testing.T, extra string) *hub.Hub {
	t.Helper()
	dir := t.TempDir()
	if _, err := hub.InitCA(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hub.toml")
	cfg := fmt.Sprintf("data_dir = %q\nlisten = \"127.0.0.1:0\"\nrelay_udp = \"off\"\nhttp = \"127.0.0.1:0\"\n%s\n[web]\ninsecure_cookie = true\n",
		filepath.ToSlash(dir), extra)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := hub.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := hub.Open(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; h.Close() })
	for h.Addr("grpc") == "" || h.Addr("http") == "" {
		time.Sleep(10 * time.Millisecond)
	}
	return h
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var codeRE = regexp.MustCompile(`Open (\S+) and enter code ([A-Z]{4}-[A-Z]{4})`)

type loginRun struct {
	stderr *syncBuf
	done   chan error
	cancel context.CancelFunc
}

// runLogin runs `longarm login args...` with a fresh config dir.
func runLogin(t *testing.T, args ...string) *loginRun {
	t.Helper()
	configDir = t.TempDir()
	c := loginCmd()
	c.SetArgs(append(args, "--no-browser"))
	r := &loginRun{stderr: &syncBuf{}, done: make(chan error, 1)}
	c.SetErr(r.stderr)
	c.SetOut(io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	r.cancel = cancel
	t.Cleanup(cancel)
	go func() { r.done <- c.ExecuteContext(ctx) }()
	return r
}

// code waits for the pairing code (or the command's end: "", its error).
func (r *loginRun) code(t *testing.T) (string, string, error) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		if m := codeRE.FindStringSubmatch(r.stderr.String()); m != nil {
			return m[1], m[2], nil
		}
		select {
		case err := <-r.done:
			if m := codeRE.FindStringSubmatch(r.stderr.String()); m != nil {
				return m[1], m[2], err
			}
			return "", "", err
		case <-deadline:
			t.Fatalf("no code; stderr:\n%s", r.stderr.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// approve signs in to the panel with a password (a hub without Entra) and
// approves code at /link.
func approve(t *testing.T, h *hub.Hub, linkURL, login, user, code string) {
	t.Helper()
	secret, err := hub.AddPanelUser(h.Store(), login, "correct horse battery", "user", user, true)
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSuffix(linkURL, "/link")
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path string, form url.Values) string {
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", base)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	totp, _ := hub.TOTP(secret, time.Now())
	post("/login", url.Values{"user": {login}, "password": {"correct horse battery"}, "totp": {totp}})
	resp, err := c.Get(base + "/link")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	tok := regexp.MustCompile(`name="token" value="([^"]+)"`)
	m := tok.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatalf("/link: %s", b)
	}
	page := post("/link", url.Values{"token": {m[1]}, "code": {code}})
	id := regexp.MustCompile(`name="id" value="([^"]+)"`).FindStringSubmatch(page)
	if id == nil {
		t.Fatalf("confirmation: %s", page)
	}
	if out := post("/link/decide", url.Values{"token": {tok.FindStringSubmatch(page)[1]}, "id": {id[1]}, "decision": {"approve"}}); !strings.Contains(out, "Approved") {
		t.Fatalf("approve: %s", out)
	}
}

// TestLoginPairing: on a hub without Entra sign-in, longarm login falls
// back to pairing (unless --az) and installs what the approval issued.
func TestLoginPairing(t *testing.T) {
	h := testHub(t, "")
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "alice", Create: true}); err != nil {
		t.Fatal(err)
	}
	fp := pki.Fingerprint(h.CA().Cert)
	if err := <-runLogin(t, h.Addr("grpc"), "--az", "--ca-fingerprint", fp).done; err != hubclient.ErrNoEntra {
		t.Fatalf("--az on a hub without Entra: %v", err)
	}
	r := runLogin(t, h.Addr("grpc"), "--ca-fingerprint", fp, "--name", "laptop")
	u, code, err := r.code(t)
	if err != nil || !strings.Contains(r.stderr.String(), "no Microsoft Entra sign-in for az: signing in through the browser instead") ||
		u != "http://"+h.Addr("http")+"/link" {
		t.Fatalf("fallback: %v\n%s", err, r.stderr.String())
	}
	approve(t, h, u, "alice-web", "alice", code)
	if err := <-r.done; err != nil {
		t.Fatalf("login: %v\n%s", err, r.stderr.String())
	}
	b, err := pki.LoadBundle(filepath.Join(configDir, client.TLSDir))
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := pki.IdentityFromCert(b.Leaf()); id.User != "alice" || id.Name != "laptop" || id.Role != pki.RoleClient {
		t.Fatalf("installed: %v", id)
	}
	if hc, _ := hubclient.LoadFile(configDir); hc.Address != h.Addr("grpc") {
		t.Fatalf("hub.toml: %+v", hc)
	}
}

// fakeAz installs an Azure CLI whose get-access-token fails with msg; it
// records its calls in args.txt.
func fakeAz(t *testing.T, msg string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "fail.txt"), []byte(msg+"\n"), 0o600)
	path, script := filepath.Join(dir, "az"), "#!/bin/sh\nd=$(dirname \"$0\")\necho \"$*\" >> \"$d/args.txt\"\n"+
		"if [ \"$2\" = show ]; then echo user; exit 0; fi\ncat \"$d/fail.txt\" >&2; exit 1\n"
	if runtime.GOOS == "windows" {
		path, script = filepath.Join(dir, "az.cmd"), "@echo off\r\necho %*>>\"%~dp0args.txt\"\r\n"+
			"if \"%2\"==\"show\" (echo user& exit /b 0)\r\ntype \"%~dp0fail.txt\" 1>&2\r\nexit /b 1\r\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(signin.EnvAz, path)
	return dir
}

// TestLoginAzFallback: on a hub with Entra sign-in, a missing az or one
// without a token falls back to pairing; --az doesn't; --link skips az.
func TestLoginAzFallback(t *testing.T) {
	h := testHub(t, `[auth.entra]
tenant_id = "11111111-2222-3333-4444-555555555555"
client_id = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
authority = "http://127.0.0.1:9"`)
	addr := h.Addr("grpc")
	pairs := func(what string, args ...string) {
		t.Helper()
		r := runLogin(t, append([]string{addr}, args...)...)
		_, code, err := r.code(t)
		if code == "" {
			t.Fatalf("%s: no pairing: %v\n%s", what, err, r.stderr.String())
		}
		r.cancel()
		if err := <-r.done; err == nil {
			t.Fatalf("%s: canceled login succeeded", what)
		}
	}
	t.Setenv(signin.EnvAz, filepath.Join(t.TempDir(), "missing"))
	pairs("no az")
	if r := runLogin(t, addr, "--az"); !strings.Contains(fmt.Sprint(<-r.done), "was not found") || strings.Contains(r.stderr.String(), "enter code") {
		t.Fatalf("--az without az: %s", r.stderr.String())
	}
	dir := fakeAz(t, "ERROR: AADSTS70043: The refresh token has expired. Please run 'az login' to setup account.")
	pairs("az without a token")
	r := runLogin(t, addr, "--az")
	if err := <-r.done; err == nil || !strings.Contains(err.Error(), "az login --tenant 11111111-2222-3333-4444-555555555555") ||
		strings.Contains(r.stderr.String(), "enter code") {
		t.Fatalf("--az, az without a token: %v\n%s", err, r.stderr.String())
	}
	os.Remove(filepath.Join(dir, "args.txt"))
	pairs("--link", "--link")
	if _, err := os.Stat(filepath.Join(dir, "args.txt")); err == nil {
		t.Fatal("--link ran az")
	}
	if err := <-runLogin(t, addr, "--az", "--link").done; err == nil {
		t.Fatal("--az --link accepted")
	}
}
