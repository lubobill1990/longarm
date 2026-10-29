package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/hub"
	"github.com/lubobill1990/longarm/internal/localapi"
)

// uiStack is a hub with two enrolled workers (wa accepts jobs submitted
// through the hub, wb only kill/logs), a panel user with TOTP, and alice's
// longarm daemon. It backs TestUI* and the Playwright run (TestUIPlaywright).
type uiStack struct {
	*hubEnv
	wa, wb     *hubWorker
	user, pass string
	secret     string // TOTP
}

func newUIStack(t *testing.T) *uiStack {
	t.Helper()
	h := newHubEnv(t, "")
	s := &uiStack{hubEnv: h, user: "ops", pass: "correct horse battery"}
	s.wa = h.newWorkerDir(t, "wa", "[hub]\nremote_jobs = \"all\"\n")
	s.wb = h.newWorkerDir(t, "wb", "")
	h.joinWorker(t, s.wa, true)
	h.joinWorker(t, s.wb, true)
	s.wa.start(t, true)
	s.wb.start(t, true)
	for _, n := range []string{"wa", "wb"} {
		h.waitNode(t, n, "online with metrics", func(n map[string]any) bool { return isTrue(n["online"]) && n["metrics"] != nil })
	}
	os.Remove(filepath.Join(h.client, "workers.toml")) // alice discovers workers through the hub
	out := h.hubCmdIn(t, s.pass+"\n", "user", "add", s.user, "--password-stdin")
	m := regexp.MustCompile(`totp_secret (\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("user add: %s", out)
	}
	s.secret = m[1]
	return s
}

// panelClient is a logged-in HTTP client of the hub's API.
type panelClient struct {
	t    *testing.T
	c    *http.Client
	base string
}

func (s *uiStack) login(t *testing.T) *panelClient {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	p := &panelClient{t: t, c: &http.Client{Jar: jar, Timeout: 60 * time.Second}, base: s.httpURL()}
	// a TOTP step is accepted once: wait for a fresh one if needed
	var code int
	for i := 0; i < 3; i++ {
		otp, _ := hub.TOTP(s.secret, time.Now())
		code, _ = p.do("POST", "/api/login", fmt.Sprintf(`{"user":%q,"password":%q,"totp":%q}`, s.user, s.pass, otp))
		if code == 200 {
			return p
		}
		time.Sleep(time.Until(time.Now().Truncate(30 * time.Second).Add(30*time.Second + 200*time.Millisecond)))
	}
	t.Fatalf("login: %d", code)
	return nil
}

func (p *panelClient) do(method, path, body string) (int, string) {
	p.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, p.base+path, r)
	if body != "" || method != "GET" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.c.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, compactJSON(string(b))
}

// sse collects the events of an SSE endpoint until "end"/"error" or timeout.
func sseEvents(t *testing.T, c *http.Client, url string, timeout time.Duration) []string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		ev := ""
		for sc.Scan() {
			l := sc.Text()
			switch {
			case strings.HasPrefix(l, "event: "):
				ev = strings.TrimPrefix(l, "event: ")
			case strings.HasPrefix(l, "data: "):
				out = append(out, ev+" "+strings.TrimPrefix(l, "data: "))
				if ev == "end" || ev == "error" {
					return
				}
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		resp.Body.Close()
		<-done
	}
	return out
}

// logText decodes the base64 data of "log" events.
func logText(events []string) string {
	var b strings.Builder
	for _, e := range events {
		if !strings.HasPrefix(e, "log ") {
			continue
		}
		var c struct {
			Data []byte `json:"data"`
		}
		json.Unmarshal([]byte(strings.TrimPrefix(e, "log ")), &c)
		b.Write(c.Data)
	}
	return b.String()
}

// TestUIPanelAPI: the panel's API additions against real workers: submit
// through the hub (auto placement and an explicit worker), follow logs,
// kill, remote_jobs refusal, metrics history, users with TOTP, install
// commands, toolchain apply, and the embedded frontend at / with a CSP.
func TestUIPanelAPI(t *testing.T) {
	t.Parallel()
	s := newUIStack(t)
	p := s.login(t)

	code, body := p.do("GET", "/api/me", "")
	if code != 200 || !strings.Contains(body, `"ca_fingerprint":"`+s.fp+`"`) || !strings.Contains(body, `"hub_address":"127.0.0.1:`) {
		t.Fatalf("me %d %s", code, body)
	}
	// Submit on wa, follow the logs to the end.
	code, body = p.do("POST", "/api/jobs", `{"worker":"wa","argv":["`+jsonPath(bins.helper)+`","print","3","0","0"],"name":"panel-print"}`)
	if code != 200 || !strings.Contains(body, `"worker":"wa"`) {
		t.Fatalf("submit %d %s", code, body)
	}
	id := regexp.MustCompile(`"id":"([0-9A-Z]+)"`).FindStringSubmatch(body)[1]
	evs := sseEvents(t, p.c, p.base+"/api/jobs/"+id+"/logs?follow=1", 60*time.Second)
	if txt := logText(evs); !strings.Contains(txt, "line 3") || !strings.HasPrefix(evs[len(evs)-1], "end") {
		t.Fatalf("logs %q %v", txt, evs)
	}
	waitFor(t, "job done in the index", 20*time.Second, func() bool {
		_, b := p.do("GET", "/api/jobs/"+id, "")
		return strings.Contains(b, "JOB_STATE_DONE") && strings.Contains(b, `"submitted_by":"web/ops"`)
	})
	// wb keeps the default remote_jobs ("control"): submitting is refused there.
	if code, body = p.do("POST", "/api/jobs", `{"worker":"wb","command":"whatever"}`); code != 403 || !strings.Contains(body, "remote_jobs") {
		t.Fatalf("submit on wb: %d %s", code, body)
	}
	// A long job placed automatically (wb is excluded by a label it lacks) and killed from the panel.
	p.do("PUT", "/api/nodes/wa/labels", `{"ui":"yes"}`)
	code, body = p.do("POST", "/api/jobs", `{"worker":"auto","argv":["`+jsonPath(bins.helper)+`","sleep","300"],"require":{"ui":"yes"}}`)
	if code != 200 || !strings.Contains(body, `"worker":"wa"`) {
		t.Fatalf("auto submit %d %s", code, body)
	}
	long := regexp.MustCompile(`"id":"([0-9A-Z]+)"`).FindStringSubmatch(body)[1]
	waitFor(t, "long job running", 20*time.Second, func() bool {
		_, b := p.do("GET", "/api/jobs/"+long, "")
		return strings.Contains(b, "JOB_STATE_RUNNING")
	})
	if code, body = p.do("POST", "/api/jobs/"+long+"/kill", ""); code != 200 || !strings.Contains(body, "JOB_STATE_KILLED") {
		t.Fatalf("kill %d %s", code, body)
	}
	// The worker audited the panel user.
	waitFor(t, "audit of the panel kill", 30*time.Second, func() bool {
		_, b := p.do("GET", "/api/audit?node=wa", "")
		return strings.Contains(b, `web/ops via hub`)
	})
	// Metrics history for sparklines.
	if code, body = p.do("GET", "/api/history", ""); code != 200 || !strings.Contains(body, `"node":"wa"`) || !strings.Contains(body, `"cpu"`) {
		t.Fatalf("history %d %s", code, body)
	}
	// Toolchain apply: the e2e workers have no profile reconciler running? It
	// is enabled whenever a hub is configured; the request is accepted.
	if code, body = p.do("POST", "/api/nodes/wb/toolchain/apply", ""); code != 200 && code != 409 {
		t.Fatalf("toolchain apply %d %s", code, body)
	}
	// Users: add (TOTP secret returned once), list, delete; not yourself.
	if code, body = p.do("POST", "/api/users", `{"name":"eve","password":"short","current_password":"correct horse battery"}`); code != 400 {
		t.Fatalf("weak password: %d %s", code, body)
	}
	if code, _ = p.do("POST", "/api/users", `{"name":"eve","password":"another long password","current_password":"wrong"}`); code != 403 {
		t.Fatalf("add user without the current password: %d", code)
	}
	if code, body = p.do("POST", "/api/users", `{"name":"eve","password":"another long password","current_password":"correct horse battery"}`); code != 200 || !strings.Contains(body, "otpauth://totp/") {
		t.Fatalf("add user %d %s", code, body)
	}
	if _, body = p.do("GET", "/api/users", ""); !strings.Contains(body, `"name":"eve"`) || !strings.Contains(body, `"name":"ops"`) {
		t.Fatalf("users %s", body)
	}
	if code, _ = p.do("DELETE", "/api/users/ops", `{"current_password":"correct horse battery"}`); code != 409 {
		t.Fatalf("delete self: %d", code)
	}
	if code, _ = p.do("DELETE", "/api/users/eve", `{"current_password":"correct horse battery"}`); code != 200 {
		t.Fatalf("delete eve: %d", code)
	}
	if _, body = p.do("GET", "/api/audit?node=hub", ""); !strings.Contains(body, "users/add") || !strings.Contains(body, "users/delete") {
		t.Fatalf("user changes not audited: %s", body)
	}
	// Install commands for a join code.
	if code, body = p.do("GET", "/api/install?code=K7P-Q2M&role=client", ""); code != 200 || !strings.Contains(body, "install.ps1))) -Join K7P-Q2M -Role client") ||
		!strings.Contains(body, "install.sh | sh -s -- --join K7P-Q2M --role client") {
		t.Fatalf("install %d %s", code, body)
	}
	if code, _ = p.do("GET", "/api/install?code=bad%3Brm", ""); code != 400 {
		t.Fatalf("bad code accepted: %d", code)
	}
	// The frontend: index.html at / and at client routes, with a CSP.
	for _, path := range []string{"/", "/nodes", "/jobs/" + id} {
		resp, err := p.c.Get(p.base + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(b), "<html") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
			t.Fatalf("GET %s: %d %q", path, resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
		}
	}
	// longarm admin equivalents.
	if out := s.mustLongarm(s.admin, "admin", "logs", id); !strings.Contains(out, "line 3") {
		t.Fatalf("longarm admin logs: %s", out)
	}
	r := s.longarm(s.admin, "admin", "run", "wb", "--", bins.helper, "print", "1", "0", "0")
	if r.code == 0 || !strings.Contains(r.stderr, "remote_jobs") {
		t.Fatalf("longarm admin run on wb: %+v", r)
	}
}

func jsonPath(p string) string { return strings.ReplaceAll(p, `\`, `\\`) }

// TestUIDaemonPool: longarm daemon's pool view and latency monitor: both workers
// online over a measured leg, metrics from the hub, then (hub killed)
// directly from the workers; transport policy switching; my jobs; link reports to
// the hub (longarm admin links).
func TestUIDaemonPool(t *testing.T) {
	t.Parallel()
	s := newUIStack(t)
	os.Setenv("LONGARM_POOL_INTERVAL", "500ms") // inherited by longarm daemon
	t.Cleanup(func() { s.longarm(s.client, "daemon", "stop") })
	s.mustLongarm(s.client, "daemon", "start")
	addr, err := localapi.ReadAddr(filepath.Join(s.client, "daemon.addr"))
	if err != nil {
		t.Fatal(err)
	}
	hc := localapi.HTTPClient(addr)
	pool := func() map[string]any {
		resp, err := hc.Get("http://local/v1/pool?history=1")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v map[string]any
		json.NewDecoder(resp.Body).Decode(&v)
		return v
	}
	worker := func(v map[string]any, name string) map[string]any {
		ws, _ := v["workers"].([]any)
		for _, w := range ws {
			if m, _ := w.(map[string]any); m["name"] == name {
				return m
			}
		}
		return nil
	}
	activeLeg := func(w map[string]any) map[string]any {
		link, _ := w["link"].(map[string]any)
		legs, _ := link["legs"].([]any)
		for _, l := range legs {
			if m, _ := l.(map[string]any); isTrue(m["active"]) {
				return m
			}
		}
		return nil
	}
	var v map[string]any
	waitFor(t, "both workers online with direct metrics, the hub's node info and RTT history", 30*time.Second, func() bool {
		v = pool()
		for _, n := range []string{"wa", "wb"} {
			w := worker(v, n)
			if w == nil || w["state"] != "online" || w["source"] != "direct" || w["metrics"] == nil || w["node"] == nil { // direct first, the hub fills in
				return false
			}
			l := activeLeg(w)
			if l == nil || !isTrue(l["up"]) || l["label"] != "tcp" {
				return false
			}
			if h, _ := l["history"].([]any); len(h) < 3 {
				return false
			}
		}
		return v["source"] == "direct"
	})
	// longarm pool (CLI) shows the same. (A worker's direct stream can lag
	// for a moment on a loaded runner - "source mixed" - so poll.)
	var out string
	waitFor(t, "longarm pool: source direct", 30*time.Second, func() bool {
		out = s.mustLongarm(s.client, "pool")
		return strings.Contains(out, "source direct") && strings.Contains(out, "wa") && strings.Contains(out, "online")
	})
	// My jobs: a job alice submits counts; the pool and /v1/jobs?mine=1 show it.
	id := strings.TrimSpace(s.mustLongarm(s.client, "run", "wa", "--", bins.helper, "sleep", "300"))
	waitFor(t, "my job listed", 30*time.Second, func() bool {
		resp, err := hc.Get("http://local/v1/jobs?mine=1&active=1")
		if err != nil {
			return false
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return strings.Contains(string(b), id) && strings.Contains(compactJSON(string(b)), `"submitted_by":"client/alice"`)
	})
	waitFor(t, "my_jobs in the pool", 30*time.Second, func() bool {
		w := worker(pool(), "wa")
		return w != nil && fmt.Sprint(w["my_jobs"]) == "1"
	})
	// Links reach the hub.
	waitFor(t, "longarm admin links", 30*time.Second, func() bool {
		r := s.longarm(s.admin, "admin", "links")
		return r.code == 0 && strings.Contains(r.stdout, "alice") && strings.Contains(r.stdout, "wa") && strings.Contains(r.stdout, "tcp")
	})
	// Transport policy switch through the daemon (persisted in settings.json).
	if out := s.mustLongarm(s.client, "transport", "tcp"); strings.TrimSpace(out) != "tcp" {
		t.Fatalf("longarm transport tcp: %s", out)
	}
	if v = pool(); v["transport"] != "tcp" {
		t.Fatalf("pool transport %v", v["transport"])
	}
	if b, err := os.ReadFile(filepath.Join(s.client, "settings.json")); err != nil || !strings.Contains(string(b), `"tcp"`) {
		t.Fatalf("settings.json: %s %v", b, err)
	}
	if out := s.mustLongarm(s.client, "transport", "race"); strings.TrimSpace(out) != "race" {
		t.Fatalf("longarm transport race: %s", out)
	}
	// Kill from the client tab (daemon -> worker directly).
	resp, err := hc.Post("http://local/v1/jobs/"+id+"/kill", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "JOB_STATE_KILLED") {
		t.Fatalf("kill via daemon: %s", b)
	}
	// Hub down: metrics come straight from the workers; the view keeps working.
	s.killHub()
	// Both are eventually consistent and independent: direct metrics take
	// over as soon as the hub's are stale (which also happens with the hub
	// up but slow), the link goes down when the daemon sees its event
	// stream fail.
	waitFor(t, "direct metrics and the hub link down", 60*time.Second, func() bool {
		v = pool()
		wa, wb := worker(v, "wa"), worker(v, "wb")
		hub, _ := v["hub"].(map[string]any)
		return v["source"] == "direct" && wa != nil && wb != nil && wa["state"] == "online" && wa["source"] == "direct" && wb["source"] == "direct" &&
			hub != nil && !isTrue(hub["connected"])
	})
	// A worker going away is offline from this client's view.
	s.wb.kill()
	waitFor(t, "wb offline", 30*time.Second, func() bool {
		w := worker(pool(), "wb")
		return w != nil && w["state"] == "offline" && w["error"] != ""
	})
	// Logs of an ended job through the daemon (direct, hub down).
	evs := sseEvents(t, hc, "http://local/v1/jobs/"+id+"/logs?worker=wa", 20*time.Second)
	if len(evs) == 0 || !strings.HasPrefix(evs[len(evs)-1], "end") {
		t.Fatalf("daemon logs: %v", evs)
	}
}

// TestUIWorkerLocal: longarmd's local additions (limits honoured by the
// scheduler, jobs/logs/kill over the CLI and gateway) and longarmd leave.
func TestUIWorkerLocal(t *testing.T) {
	t.Parallel()
	s := newUIStack(t)
	// Limits: wa takes at most 1 job; with one running, auto placement goes to wb.
	if out := longarmdCmd(t, "limits", "--data-dir", s.wa.dataDir, "--max-jobs", "1", "--reserve-mem", "2"); !strings.Contains(out.stdout, "max jobs         1") {
		t.Fatalf("longarmd limits: %+v", out)
	}
	s.waitNode(t, "wa", "max_jobs reported", func(n map[string]any) bool { return fmt.Sprint(n["max_jobs"]) == "1" })
	id := strings.TrimSpace(s.mustLongarm(s.client, "run", "wa", "--", bins.helper, "sleep", "300"))
	s.waitNode(t, "wa", "1 active job", func(n map[string]any) bool { return fmt.Sprint(n["jobs_active"]) == "1" })
	for i := 0; i < 2; i++ {
		r := s.longarm(s.client, "exec", "--on", "auto", "--", bins.helper, "print", "1", "0", "0")
		if r.code != 0 || !strings.Contains(r.stderr, "on wb") {
			t.Fatalf("auto placement with wa at its limit: %+v", r)
		}
	}
	// longarmd jobs / logs / kill (local control API).
	if out := longarmdCmd(t, "jobs", "--data-dir", s.wa.dataDir); !strings.Contains(out.stdout, id) || !strings.Contains(out.stdout, "client/alice") {
		t.Fatalf("longarmd jobs: %+v", out)
	}
	addr, _ := localapi.ReadAddr(filepath.Join(s.wa.dataDir, "longarmd.local"))
	hc := localapi.HTTPClient(addr)
	resp, err := hc.Get("http://local/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if st := compactJSON(string(b)); !strings.Contains(st, `"max_jobs":1`) || !strings.Contains(st, `"os":"`) {
		t.Fatalf("status: %s", st)
	}
	if out := longarmdCmd(t, "kill", id, "--data-dir", s.wa.dataDir); !strings.Contains(out.stdout, "killed") {
		t.Fatalf("longarmd kill: %+v", out)
	}
	evs := sseEvents(t, hc, "http://local/v1/jobs/"+id+"/logs", 20*time.Second)
	if len(evs) == 0 || !strings.HasPrefix(evs[len(evs)-1], "end") {
		t.Fatalf("gateway logs: %v", evs)
	}
	if out := longarmdCmd(t, "logs", id, "--data-dir", s.wa.dataDir); out.code != 0 {
		t.Fatalf("longarmd logs: %+v", out)
	}
	// longarmd leave: revoked on the hub, longarmd stops, credentials removed.
	if out := longarmdCmd(t, "leave", "--data-dir", s.wb.dataDir, "--reason", "e2e"); out.code != 0 || !strings.Contains(out.stdout, "left the fleet") {
		t.Fatalf("longarmd leave: %+v", out)
	}
	select {
	case <-s.wb.exited:
	case <-time.After(20 * time.Second):
		t.Fatal("longarmd did not stop after leave")
	}
	s.waitNode(t, "wb", "revoked", func(n map[string]any) bool { return n["state"] == "revoked" })
	if _, err := os.Stat(filepath.Join(s.wb.dataDir, "tls")); !os.IsNotExist(err) {
		t.Fatalf("tls kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.wb.dataDir, "hub.toml")); !os.IsNotExist(err) {
		t.Fatalf("hub.toml kept: %v", err)
	}
}
