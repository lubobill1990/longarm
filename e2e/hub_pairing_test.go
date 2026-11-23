package e2e

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"longarm/internal/hub"
	"longarm/internal/pki"
	pb "longarm/proto/longarm/v1"
)

const (
	entraTenant = "11111111-2222-3333-4444-555555555555"
	entraClient = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// fakeAuthority is a Microsoft Entra stand-in: its JWKS, and a signer for
// tokens of the hub's app (access tokens, ID tokens).
type fakeAuthority struct {
	url  string
	sign func(claims map[string]any) string
}

func newFakeAuthority(t *testing.T) *fakeAuthority {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding
	mux := http.NewServeMux()
	mux.HandleFunc("GET /"+entraTenant+"/discovery/v2.0/keys", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "use": "sig", "kid": "k1",
			"n": b64.EncodeToString(key.N.Bytes()), "e": b64.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	a := &fakeAuthority{url: srv.URL}
	a.sign = func(claims map[string]any) string {
		now := time.Now().Unix()
		for k, v := range map[string]any{"aud": entraClient, "iss": srv.URL + "/" + entraTenant + "/v2.0", "tid": entraTenant, "iat": now, "nbf": now, "exp": now + 3600} {
			claims[k] = v
		}
		h, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k1"})
		c, _ := json.Marshal(claims)
		in := b64.EncodeToString(h) + "." + b64.EncodeToString(c)
		sum := sha256.Sum256([]byte(in))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return in + "." + b64.EncodeToString(sig)
	}
	return a
}

// hubConfig is the hub's [auth.entra] for a (more settings in extra).
func (a *fakeAuthority) hubConfig(extra string) string {
	return fmt.Sprintf("[auth.entra]\ntenant_id = %q\nclient_id = %q\nauthority = %q\n%s\n", entraTenant, entraClient, a.url, extra)
}

// panel is a browser signed in to the hub's panel with Microsoft.
type panel struct {
	t    *testing.T
	base string
	c    *http.Client
}

// entraPanel signs in as (oid, upn) the way the panel's sign-in page does
// (spa mode: the ID token for the sign-in's nonce goes to /auth/spa-session).
func entraPanel(t *testing.T, h *hubEnv, a *fakeAuthority, oid, upn string) *panel {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	p := &panel{t: t, base: h.httpURL(), c: &http.Client{Jar: jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	req, _ := http.NewRequest("GET", p.base+"/link", nil)
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	resp, err := p.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/auth/login?next=/link" {
		t.Fatalf("/link without a session: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	req, _ = http.NewRequest("GET", p.base+"/auth/login?next=/link", nil)
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	if resp, err = p.c.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	tok := a.sign(map[string]any{"oid": oid, "preferred_username": upn, "nonce": loc.Query().Get("nonce")})
	b, _ := json.Marshal(map[string]string{"id_token": tok})
	if code, body := p.post("/auth/spa-session", "application/json", string(b)); code != 200 || !strings.Contains(body, "/link") {
		t.Fatalf("panel sign-in of %s: %d %s", upn, code, body)
	}
	return p
}

func (p *panel) post(path, ctype, body string) (int, string) {
	p.t.Helper()
	req, _ := http.NewRequest("POST", p.base+path, strings.NewReader(body))
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Origin", p.base)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := p.c.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// waitNode waits until the panel login's view of the hub's nodes
// (/api/nodes?all=1) reports cond for node: a user's own workers are only
// in that user's view (admins see them as counts).
func (p *panel) waitNode(name, what string, cond func(n map[string]any) bool) map[string]any {
	p.t.Helper()
	var last map[string]any
	waitFor(p.t, name+" "+what, 30*time.Second, func() bool {
		resp, err := p.c.Get(p.base + "/api/nodes?all=1")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var r struct {
			Nodes []map[string]any `json:"nodes"`
		}
		if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&r) != nil {
			return false
		}
		for _, n := range r.Nodes {
			if n["name"] == name {
				last = n
				return cond(n)
			}
		}
		return false
	})
	return last
}

var (
	formTokenRE = regexp.MustCompile(`name="token" value="([^"]+)"`)
	formIDRE    = regexp.MustCompile(`name="id" value="([^"]+)"`)
	linkCodeRE  = regexp.MustCompile(`Open (\S+) and enter code ([A-Z]{4}-[A-Z]{4})`)
)

// decide types code at /link, checks the confirmation page (want) and
// approves or denies; it returns the result page.
func (p *panel) decide(code, decision string, want ...string) string {
	p.t.Helper()
	resp, err := p.c.Get(p.base + "/link")
	if err != nil {
		p.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := formTokenRE.FindStringSubmatch(string(b))
	if m == nil {
		p.t.Fatalf("/link: %d %s", resp.StatusCode, b)
	}
	_, confirm := p.post("/link", "application/x-www-form-urlencoded", url.Values{"token": {m[1]}, "code": {code}}.Encode())
	id := formIDRE.FindStringSubmatch(confirm)
	if id == nil {
		p.t.Fatalf("confirmation for %s: %s", code, confirm)
	}
	for _, w := range want {
		if !strings.Contains(confirm, w) {
			p.t.Fatalf("confirmation without %q:\n%s", w, confirm)
		}
	}
	_, out := p.post("/link/decide", "application/x-www-form-urlencoded",
		url.Values{"token": {formTokenRE.FindStringSubmatch(confirm)[1]}, "id": {id[1]}, "decision": {decision}}.Encode())
	return out
}

// proc is a background command (output: stdout and stderr together).
type proc struct {
	cmd  *exec.Cmd
	out  syncBuf
	done chan struct{}
}

func startProc(t *testing.T, cmd *exec.Cmd) *proc {
	t.Helper()
	p := &proc{cmd: cmd, done: make(chan struct{})}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "LONGARM_NO_BROWSER=1")
	cmd.Stdout, cmd.Stderr = &p.out, &p.out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { cmd.Wait(); close(p.done) }()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

// code waits for the pairing code it prints.
func (p *proc) code(t *testing.T) (string, string) {
	t.Helper()
	var m []string
	waitFor(t, "a pairing code", 60*time.Second, func() bool {
		m = linkCodeRE.FindStringSubmatch(p.out.String())
		select {
		case <-p.done:
			if m == nil {
				t.Fatalf("exited without a code:\n%s", p.out.String())
			}
		default:
		}
		return m != nil
	})
	return m[1], m[2]
}

func (p *proc) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(60 * time.Second):
		t.Fatalf("did not exit:\n%s", p.out.String())
	}
	return p.cmd.ProcessState.ExitCode()
}

func TestHubPairingLoopbackGuidance(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	conn, err := grpc.NewClient(h.addr(), grpc.WithAuthority("hub.example.com:7443"),
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, csr, err := pki.NewKeyAndCSR("worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := pb.NewEnrollClient(conn).StartLink(ctx, &pb.StartLinkRequest{Kind: "client", CsrPem: csr})
	if err != nil {
		t.Fatal(err)
	}
	if st.VerificationUrl != h.httpURL()+"/link" || len(st.SignIn) != 0 ||
		!strings.Contains(st.VerificationHint, fmt.Sprintf("ssh -L %d:127.0.0.1:%d -- hub.example.com", h.httpPort, h.httpPort)) {
		t.Fatalf("remote pairing guidance: %v", st)
	}

	cfg := filepath.Join(h.root, "cfg-login")
	cmd := exec.Command(bins.longarm, "login", h.addr(), "--link", "--no-browser", "--ca-fingerprint", h.fp)
	cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+cfg)
	p := startProc(t, cmd)
	u, _ := p.code(t)
	if out := p.out.String(); u != h.httpURL()+"/link" ||
		!strings.Contains(out, "sign in at the hub's panel (with an account or your panel login)") ||
		strings.Contains(out, "panel listens on") {
		t.Fatalf("local pairing guidance: %s", out)
	}
}

func TestPanelLogin(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	h.mustLongarm(h.admin, "admin", "user", "add", "alice", "--no-code")
	h.mustLongarm(h.client, "whoami")
	if r := h.longarm(h.admin, "admin", "user", "set", "alice", "--panel"); r.code == 0 {
		t.Fatal("admin minted alice's panel login after she had a device")
	}
	r := h.longarm(h.client, "panel-login")
	creds := regexp.MustCompile(`web panel login alice \(shown once\): password (\S+)\nTOTP secret (\S+)\n(otpauth://\S+)`).FindStringSubmatch(r.stderr)
	if r.code != 0 || r.stdout != "" || creds == nil || !strings.Contains(r.stderr, "/link") {
		t.Fatalf("panel-login: %+v", r)
	}
	if r := h.longarm(h.client, "panel-login"); r.code == 0 || !strings.Contains(r.stderr, "--reset") {
		t.Fatalf("existing panel login: %+v", r)
	}
	jar, _ := cookiejar.New(nil)
	browser := &panel{t: t, base: h.httpURL(), c: &http.Client{Jar: jar, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	signIn := func(password, secret string) {
		t.Helper()
		code, err := hub.TOTP(secret, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		status, body := browser.post("/login", "application/x-www-form-urlencoded",
			url.Values{"user": {"alice"}, "password": {password}, "totp": {code}}.Encode())
		if status != http.StatusSeeOther {
			t.Fatalf("password sign-in: %d %s", status, body)
		}
	}
	signIn(creds[1], creds[2])
	r = h.longarm(h.client, "panel-login", "--reset", "--json")
	var next struct {
		Login      string `json:"login"`
		Password   string `json:"password"`
		TotpSecret string `json:"totp_secret"`
		Otpauth    string `json:"otpauth"`
	}
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &next) != nil || next.Login != "alice" ||
		next.Password == "" || next.Password == creds[1] || next.TotpSecret == "" || next.TotpSecret == creds[2] ||
		!strings.HasPrefix(next.Otpauth, "otpauth://") {
		t.Fatalf("panel-login --reset --json: %+v", r)
	}
	resp, err := browser.c.Get(browser.base + "/api/me")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old session survived reset: %d", resp.StatusCode)
	}
	signIn(next.Password, next.TotpSecret)
	second := filepath.Join(h.root, "cfg-alice-2")
	cmd := exec.Command(bins.longarm, "login", h.addr(), "--link", "--name", "alice-2", "--ca-fingerprint", h.fp)
	cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+second)
	p := startProc(t, cmd)
	_, code := p.code(t)
	if out := browser.decide(code, "approve", "client device"); !strings.Contains(out, "signed in as alice (client)") {
		t.Fatalf("approve second device: %s", out)
	}
	if c := p.wait(t); c != 0 {
		t.Fatalf("second device login: %d\n%s", c, p.out.String())
	}
	bundle, err := pki.LoadBundle(filepath.Join(second, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Identity.Role != pki.RoleClient || bundle.Identity.User != "alice" {
		t.Fatalf("second device identity: %v", bundle.Identity)
	}
	if out := h.mustLongarm(second, "whoami"); !strings.Contains(out, "user alice ") {
		t.Fatalf("second device: %s", out)
	}
}

// TestHubPairing: enrolling with a Microsoft account without az - longarmd
// login and longarm login (no az: falls back) print a code; the user,
// signed in to the panel with Microsoft, approves it at /link; the worker
// joins as theirs and serves them, the client acts as them. A denied
// pairing and one the hub forgot (restart) fail cleanly; longarmd login
// --role both enrolls the worker and the client with one code and one
// approval; the install script's --login --role both does the same.
func TestHubPairing(t *testing.T) {
	t.Parallel()
	a := newFakeAuthority(t)
	h := newHubEnv(t, a.hubConfig(`admins = ["boss@example.com"]`))
	erinWeb := entraPanel(t, h, a, "e7e70000-0000-4000-8000-000000000001", "erin@example.com")

	// a worker
	w := h.newWorkerDir(t, "pw", "")
	p := startProc(t, exec.Command(bins.longarmd, "login", h.addr(), "--data-dir", w.dataDir, "--ca-fingerprint", h.fp, "--name", "pw"))
	u, code := p.code(t)
	if u != h.httpURL()+"/link" {
		t.Fatalf("link URL %s", u)
	}
	if out := p.out.String(); !strings.Contains(out, "sign in with your account (Microsoft (work or school))") ||
		strings.Contains(out, "Google") || strings.Contains(out, "panel listens on") {
		t.Fatalf("sign-in guidance: %s", out)
	}
	if out := erinWeb.decide(code, "approve", "Only confirm if this is your machine", "<code>pw</code>", "127.0.0.1"); !strings.Contains(out, "worker pw is yours") {
		t.Fatalf("approve worker: %s", out)
	}
	if c := p.wait(t); c != 0 || !strings.Contains(p.out.String(), "joined as worker pw") || !strings.Contains(p.out.String(), "owned by erin") ||
		!strings.Contains(p.out.String(), "hub CA verified") {
		t.Fatalf("longarmd login: %d\n%s", c, p.out.String())
	}
	if b, err := os.ReadFile(filepath.Join(w.dataDir, "worker-access.json")); err != nil || !strings.Contains(string(b), `"erin"`) {
		t.Fatalf("worker access: %s %v", b, err)
	}
	w.start(t, true)
	erinWeb.waitNode("pw", "online, erin's", func(n map[string]any) bool { return isTrue(n["online"]) && n["owner"] == "erin" })

	// a client without az: longarm login falls back to pairing
	erin := filepath.Join(h.root, "erin")
	cmd := exec.Command(bins.longarm, "login", h.addr(), "--ca-fingerprint", h.fp)
	cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+erin, "LONGARM_AZ="+filepath.Join(h.root, "no-az"))
	p = startProc(t, cmd)
	_, code = p.code(t)
	if out := erinWeb.decide(code, "approve", "client device"); !strings.Contains(out, "signed in as erin (client)") {
		t.Fatalf("approve client: %s", out)
	}
	if c := p.wait(t); c != 0 || !strings.Contains(p.out.String(), "no az sign-in for this hub") || !strings.Contains(p.out.String(), "signed in to") ||
		!strings.Contains(p.out.String(), "client/erin-") {
		t.Fatalf("longarm login: %d\n%s", c, p.out.String())
	}
	if r := h.longarm(erin, "exec", "pw", "--", bins.helper, "print", "1", "0", "0"); r.code != 0 || !strings.Contains(r.stdout, "line 1") {
		t.Fatalf("erin exec on her worker: %+v", r)
	}
	if r := h.longarm(h.client, "exec", "pw", "--", bins.helper, "print", "1", "0", "0"); r.code == 0 {
		t.Fatalf("alice used erin's worker: %+v", r)
	}

	// denied
	w2 := h.newWorkerDir(t, "pw2", "")
	p = startProc(t, exec.Command(bins.longarmd, "login", h.addr(), "--data-dir", w2.dataDir, "--ca-fingerprint", h.fp, "--name", "pw2"))
	_, code = p.code(t)
	if out := erinWeb.decide(code, "deny"); !strings.Contains(out, "Denied") {
		t.Fatalf("deny: %s", out)
	}
	if c := p.wait(t); c == 0 || !strings.Contains(p.out.String(), "denied") {
		t.Fatalf("denied longarmd login: %d\n%s", c, p.out.String())
	}
	if _, err := os.Stat(filepath.Join(w2.dataDir, "tls", "node.crt")); err == nil {
		t.Fatal("a denied worker got a certificate")
	}

	// both roles, one code, one approval: longarmd login --role both makes
	// this machine erin's worker and her client device
	w3 := h.newWorkerDir(t, "pboth", "")
	erin2 := filepath.Join(h.root, "erin2")
	p = startProc(t, exec.Command(bins.longarmd, "login", h.addr(), "--data-dir", w3.dataDir, "--ca-fingerprint", h.fp,
		"--name", "pboth", "--role", "both", "--config-dir", erin2, "--device-name", "erin-pboth"))
	_, code = p.code(t)
	if out := erinWeb.decide(code, "approve", "Approving does <b>two</b> things", "client device (acts as you) + worker (runs jobs)",
		"<code>pboth</code>", "<code>erin-pboth</code>", "jobs you run may be placed on it"); !strings.Contains(out,
		"device erin-pboth is signed in as erin (client), and worker pboth is yours") {
		t.Fatalf("approve both: %s", out)
	}
	if c := p.wait(t); c != 0 || strings.Count(p.out.String(), "enter code") != 1 || !strings.Contains(p.out.String(), "joined as worker pboth") ||
		!strings.Contains(p.out.String(), "owned by erin") || !strings.Contains(p.out.String(), "client/erin-pboth") {
		t.Fatalf("longarmd login --role both: %d\n%s", c, p.out.String())
	}
	w3.start(t, true)
	erinWeb.waitNode("pboth", "online, erin's", func(n map[string]any) bool { return isTrue(n["online"]) && n["owner"] == "erin" })
	if r := h.longarm(erin2, "exec", "pboth", "--", bins.helper, "print", "1", "0", "0"); r.code != 0 || !strings.Contains(r.stdout, "line 1") {
		t.Fatalf("erin's device on her worker, one approval: %+v", r)
	}
	// forgotten by the hub (restarted): the code is dead, and login says so
	cmd = exec.Command(bins.longarm, "login", h.addr(), "--ca-fingerprint", h.fp, "--link")
	cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+filepath.Join(h.root, "frank"))
	p = startProc(t, cmd)
	p.code(t)
	h.killHub()
	h.startHub(t)
	if c := p.wait(t); c == 0 || !strings.Contains(p.out.String(), "expired") {
		t.Fatalf("login across a hub restart: %d\n%s", c, p.out.String())
	}

	// the install script's --login: install, then longarmd login
	rel := filepath.Join(h.root, "release")
	os.MkdirAll(rel, 0o755)
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	for tool, src := range map[string]string{"longarm": bins.longarm, "longarmd": bins.longarmd} {
		copyBin(t, src, filepath.Join(rel, fmt.Sprintf("%s-%s-%s%s", tool, runtime.GOOS, runtime.GOARCH, suffix)))
	}
	h.hubCmd(t, "release", "add", rel, "--version", "1.2.3")
	// --login without a role: both - one code for the worker and the client
	prefix, dd, cfg := filepath.Join(h.root, "inst-bin"), filepath.Join(h.root, "inst-data"), filepath.Join(h.root, "inst-cfg")
	if runtime.GOOS == "windows" {
		pwsh, err := exec.LookPath("pwsh")
		if err != nil {
			t.Skip("pwsh not installed")
		}
		cmd = exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", fmt.Sprintf("irm %s/install.ps1 | iex", h.httpURL()))
		cmd.Env = append(os.Environ(), "LONGARM_LOGIN=1", "LONGARM_NO_SERVICE=1", "LONGARM_NO_PATH=1", "LONGARM_NO_FIREWALL=1",
			"LONGARM_PREFIX="+prefix, "LONGARM_DATA_DIR="+dd, "LONGARM_NAME=inst", "LONGARM_CONFIG_DIR="+cfg)
	} else {
		if _, err := exec.LookPath("curl"); err != nil {
			t.Skip("curl not installed")
		}
		cmd = exec.Command("sh", "-c", fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- --login --no-service --prefix %s --data-dir %s --name inst",
			h.httpURL(), prefix, dd))
		cmd.Env = append(os.Environ(), "HOME="+h.root, "LONGARM_CONFIG_DIR="+cfg)
	}
	p = startProc(t, cmd)
	_, code = p.code(t)
	erinWeb.decide(code, "approve", "<code>inst</code>", "Approving does <b>two</b> things")
	if c := p.wait(t); c != 0 || strings.Count(p.out.String(), "enter code") != 1 || !strings.Contains(p.out.String(), "joined as worker inst") ||
		!strings.Contains(p.out.String(), "owned by erin") || !strings.Contains(p.out.String(), "client/erin-") {
		t.Fatalf("install --login --role both: %d\n%s", c, p.out.String())
	}
	if _, err := os.Stat(filepath.Join(cfg, "tls", "node.crt")); err != nil {
		t.Fatalf("install --login --role both: no client certificate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(prefix, "longarmd"+suffix)); err != nil {
		t.Fatalf("longarmd not installed: %v", err)
	}
}
