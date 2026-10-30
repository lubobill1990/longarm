package tray

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

// TestSessionsOpen: /trayapi/sessions/open validates the target and hands
// the location hash to the shell's window, or opens the page (with a
// one-time code) in the browser when there is no window.
func TestSessionsOpen(t *testing.T) {
	s, err := NewServer(Options{LongarmConfigDir: t.TempDir(), LongarmdDataDir: t.TempDir(), Roles: "client"}, &fakeDesk{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var opened []string
	s.openURL = func(u string) error { opened = append(opened, u); return nil }
	go s.Serve()
	defer s.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.Get(s.URL(SessionsPath))
	if err != nil || resp.StatusCode != 200 || resp.Request.URL.Path != SessionsPath {
		t.Fatalf("the page: %v %v", err, resp)
	}
	resp.Body.Close()
	post := func(body string) (int, string) {
		resp, err := c.Post("http://"+s.Addr()+"/trayapi/sessions/open", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	for _, bad := range []string{
		`{"worker":"w1; rm -rf /"}`, `{"worker":"-evil"}`, `{"worker":"` + strings.Repeat("w", 65) + `"}`,
		`{"agent":"aider"}`, `{"agent":"Claude"}`,
		`{"key":"w1/claude/<script>"}`, `{"key":"w1/claude/a b"}`, `{"key":"` + strings.Repeat("k", 201) + `"}`,
		`{"worker":`,
	} {
		if code, body := post(bad); code != 400 {
			t.Errorf("%s: %d %s", bad, code, body)
		}
	}
	if len(opened) != 0 {
		t.Fatalf("opened on bad input: %v", opened)
	}

	// no window (--browser, no webview): the default browser, with a code
	if code, body := post(`{"worker":"w1","agent":"claude","key":"w1/claude/9f1c2d3e-0000"}`); code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("open: %d %s", code, body)
	}
	if len(opened) != 1 || !strings.HasPrefix(opened[0], "http://"+s.Addr()+"/sessions?token=") ||
		!strings.HasSuffix(opened[0], "#agent=claude&key=w1%2Fclaude%2F9f1c2d3e-0000&worker=w1") {
		t.Fatalf("browser: %v", opened)
	}

	// the shell's window gets the hash
	var hashes []string
	s.OpenSessions = func(h string) { hashes = append(hashes, h) }
	post(`{"worker":"w1","agent":"copilot"}`)
	post(`{}`)
	if len(hashes) != 2 || hashes[0] != "agent=copilot&worker=w1" || hashes[1] != "" || len(opened) != 1 {
		t.Fatalf("window: %q (browser %v)", hashes, opened)
	}
	if u := s.SessionsURL("worker=w1", false); u != "http://"+s.Addr()+"/sessions#worker=w1" {
		t.Fatalf("URL for an open window: %s", u)
	}
	if u := s.SessionsURL("", true); !strings.Contains(u, "/sessions?token=") || strings.Contains(u, "#") {
		t.Fatalf("fresh URL: %s", u)
	}
}
