package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/hub"
	"github.com/lubobill1990/longarm/internal/pki"
)

func certSerial(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	c, err := pki.ParseCertPEM(b)
	if err != nil {
		return ""
	}
	return pki.Serial(c)
}

// TestHubRevocation: a client enrolled with a join code works, is revoked
// by an admin, and is then refused by the hub and - after the revocation
// list reached it with the next heartbeat - by the worker.
func TestHubRevocation(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	wa := h.newWorkerDir(t, "wa", "")
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })

	bob := filepath.Join(h.root, "bob")
	code := strings.TrimSpace(h.mustLongarm(h.admin, "admin", "join-code", "--role", "client"))
	if out := h.mustLongarm(bob, "join", h.addr(), code, "--name", "bob", "--ca-fingerprint", h.fp); !strings.Contains(out, "joined") {
		t.Fatalf("longarm join: %s", out)
	}
	// A used code is gone.
	if r := h.longarm(filepath.Join(h.root, "eve"), "join", h.addr(), code, "--ca-fingerprint", h.fp); r.code == 0 {
		t.Fatalf("join code reused: %+v", r)
	}
	// A wrong CA pin is refused before anything is sent.
	if r := h.longarm(filepath.Join(h.root, "eve"), "join", h.addr(), "AAA-AAA", "--ca-fingerprint", strings.Repeat("ab", 32)); r.code == 0 || !strings.Contains(r.stderr, "does not match") {
		t.Fatalf("wrong pin: %+v", r)
	}
	if r := h.longarm(bob, "exec", "wa", "--", bins.helper, "print", "1", "0", "0"); r.code != 0 {
		t.Fatalf("bob exec before revocation: %+v", r)
	}
	// A stream bob already has open (follow a long job) must end too.
	follow := h.longarmBG(t, bob, "--no-hub", "run", "wa", "-f", "--", bins.helper, "print", "200", "200", "0")
	waitOutput(t, follow.stdout.String, "line 3", 30*time.Second)
	h.mustLongarm(h.admin, "admin", "revoke", "bob")
	if code := follow.wait(t, 30*time.Second); code == 0 || !strings.Contains(follow.stderr.String(), "revoked") {
		t.Fatalf("open stream survived the revocation: exit %d, stderr %s", code, follow.stderr.String())
	}
	waitFor(t, "the worker to refuse bob", 20*time.Second, func() bool {
		r := h.longarm(bob, "--no-hub", "exec", "wa", "--", bins.helper, "print", "1", "0", "0")
		return r.code != 0 && strings.Contains(r.stderr, "revoked")
	})
	if r := h.longarm(bob, "admin", "nodes"); r.code == 0 {
		t.Fatalf("hub still accepts bob: %+v", r)
	}
	// alice is unaffected.
	if r := h.longarm(h.client, "exec", "wa", "--", bins.helper, "print", "1", "0", "0"); r.code != 0 {
		t.Fatalf("alice: %+v", r)
	}
	revoked, _ := os.ReadFile(filepath.Join(wa.dataDir, "revoked.json"))
	if !strings.Contains(string(revoked), certSerial(t, filepath.Join(bob, "tls", "node.crt"))) {
		t.Fatalf("revocation list not persisted: %s", revoked)
	}
	// The audit trail of wa reached the hub.
	waitFor(t, "forwarded audit", 20*time.Second, func() bool {
		r := h.longarm(h.admin, "admin", "audit", "--node", "wa", "--limit", "100")
		return strings.Contains(r.stdout, "client/bob") && strings.Contains(r.stdout, "client/alice")
	})
}

// TestWorkerAccessPolicy: a worker whose owner (alice) keeps the hub's
// admins out ([access] admins = "none") and the command lines of its jobs
// from the hub (hub_job_details = false). alice works as before; root's
// admin certificate only gets node info, the hub session's remote jobs are
// off by default, and the hub learns the job but not what it runs.
func TestWorkerAccessPolicy(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	wa := h.newWorkerDir(t, "wa", "[access]\nowners = [\"alice\"]\nadmins = \"none\"\nhub_job_details = false\n")
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })

	if r := h.longarm(h.client, "exec", "wa", "--", bins.helper, "print", "1", "0", "0"); r.code != 0 || !strings.Contains(r.stdout, "line 1") {
		t.Fatalf("owner exec: %+v", r)
	}
	id := strings.TrimSpace(h.mustLongarm(h.client, "run", "wa", "--", bins.helper, "print", "2", "0", "0"))
	h.mustLongarm(h.client, "wait", "wa", id)
	if r := h.longarm(h.admin, "exec", "wa", "--", bins.helper, "print", "1", "0", "0"); r.code == 0 || !strings.Contains(r.stderr, "admins = none") {
		t.Fatalf("admin exec: %+v", r)
	}
	if r := h.longarm(h.admin, "admin", "logs", id); r.code == 0 || !strings.Contains(r.stderr, "remote_jobs") {
		t.Fatalf("admin logs through the hub: %+v", r)
	}
	var jobs string
	waitFor(t, "the job in the hub index", 20*time.Second, func() bool {
		jobs = h.longarm(h.admin, "jobs", "--json").stdout
		return strings.Contains(jobs, id) && strings.Contains(jobs, "DONE")
	})
	if strings.Contains(jobs, "print") || strings.Contains(jobs, "argv") {
		t.Fatalf("command line reached the hub: %s", jobs)
	}
	waitFor(t, "forwarded audit", 20*time.Second, func() bool {
		r := h.longarm(h.admin, "admin", "audit", "--node", "wa", "--limit", "100", "--json")
		return strings.Contains(r.stdout, "Jobs/Submit") && strings.Contains(r.stdout, "admins = none")
	})
	if r := h.longarm(h.admin, "admin", "audit", "--node", "wa", "--limit", "100", "--json"); strings.Contains(r.stdout, "print") {
		t.Fatalf("command line in the hub's audit: %s", r.stdout)
	}
	if local, _ := os.ReadFile(filepath.Join(wa.dataDir, "audit.log")); !strings.Contains(string(local), `"print"`) {
		t.Fatalf("local audit.log lost the command line:\n%s", local)
	}
	if log, _ := os.ReadFile(wa.logPath); !strings.Contains(string(log), "users=any owners=alice admins=none") {
		t.Fatalf("no access policy log line:\n%s", log)
	}
}

// TestHubRenewalOverRelay: a worker reachable only through longnet via the
// relay built into the hub; its certificate is renewed automatically (short
// TTL) and the longnet listener presents the renewed certificate: new
// connections still work after the original certificate expired.
func TestHubRenewalOverRelay(t *testing.T) {
	t.Parallel()
	// Renewal every ~6 s (TTL - renew_before), with 34 s of slack before a
	// certificate expires: a worker stalled for a while on a loaded runner
	// (seen: ~20 s) still renews in time. With 20s/14s one such stall let
	// the certificate expire, and nothing could renew it any more.
	h := newHubEnv(t, "[certs]\nworker_ttl = \"40s\"\nrenew_before = \"34s\"\n")
	os.Remove(filepath.Join(h.client, "workers.toml"))
	wa := h.newWorkerDir(t, "wa", "")
	os.WriteFile(filepath.Join(wa.dataDir, "longarmd.toml"), []byte(fmt.Sprintf("listen = [\"longnet://%s?name=wa\"]\n", h.addr())), 0o600)
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	n := h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	if eps := fmt.Sprint(n["endpoints"]); !strings.Contains(eps, "longnet://") {
		t.Fatalf("endpoints %s", eps)
	}
	certPath := filepath.Join(wa.dataDir, "tls", "node.crt")
	first, _ := os.ReadFile(certPath)
	firstCert, _ := pki.ParseCertPEM(first)
	r := h.longarm(h.client, "exec", "wa", "--", bins.helper, "print", "1", "0", "0")
	if r.code != 0 {
		t.Fatalf("exec over longnet via the hub relay: %+v", r)
	}
	if info := h.mustLongarm(h.client, "info", "wa"); !strings.Contains(info, "longnet/") {
		t.Fatalf("path: %s", info)
	}
	serials := map[string]bool{pki.Serial(firstCert): true}
	waitFor(t, "two renewals", time.Until(firstCert.NotAfter), func() bool {
		serials[certSerial(t, certPath)] = true
		return len(serials) >= 3
	})
	// Past the first certificate's expiry: a fresh longnet connection (new longarm
	// process) must see a valid certificate.
	if d := time.Until(firstCert.NotAfter) + time.Second; d > 0 {
		time.Sleep(d)
	}
	r = h.longarm(h.client, "exec", "wa", "--", bins.helper, "print", "1", "0", "7")
	if r.code != 7 {
		t.Fatalf("exec after the original certificate expired: %+v", r)
	}
	st := longarmdCmd(t, "status", "--data-dir", wa.dataDir, "--json")
	var ws struct {
		Hub struct {
			Connected bool `json:"connected"`
		} `json:"hub"`
	}
	if st.code != 0 || json.Unmarshal([]byte(st.stdout), &ws) != nil || !ws.Hub.Connected {
		t.Fatalf("longarmd status: %+v", st)
	}
}

var totpRE = regexp.MustCompile(`totp_secret (\S+)`)

// TestHubHTTP: the panel API behind the reverse proxy: accounts with
// password + TOTP, JSON API, CSRF rule, SSE events, health, install
// scripts (and running install.ps1 / install.sh against the release store).
func TestHubHTTP(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	wa := h.newWorkerDir(t, "wa", "")
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })

	out := h.hubCmdIn(t, "correct horse battery\n", "user", "add", "ops", "--password-stdin")
	m := totpRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("user add: %s", out)
	}
	secret := m[1]
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	base := h.httpURL()
	post := func(path, ctype, body string) (int, string) {
		t.Helper()
		resp, err := c.Post(base+path, ctype, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, compactJSON(string(b))
	}
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := c.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, compactJSON(string(b))
	}
	if code, body := get("/healthz"); code != 200 || !strings.Contains(body, `"online":1`) {
		t.Fatalf("healthz %d %s", code, body)
	}
	if code, _ := get("/api/nodes"); code != 401 {
		t.Fatalf("unauthenticated api: %d", code)
	}
	login := func(code string) int {
		st, _ := post("/api/login", "application/json", fmt.Sprintf(`{"user":"ops","password":"correct horse battery","totp":%q}`, code))
		return st
	}
	if st := login("000000"); st != 401 {
		t.Fatalf("wrong TOTP: %d", st)
	}
	now, _ := hub.TOTP(secret, time.Now())
	if st := login(now); st != 200 {
		t.Fatalf("login: %d", st)
	}
	if st := login(now); st != 401 {
		t.Fatalf("TOTP code reused: %d", st)
	}
	if code, body := get("/api/nodes"); code != 200 || !strings.Contains(body, `"name":"wa"`) {
		t.Fatalf("nodes %d %s", code, body)
	}
	// State-changing calls must be JSON (CSRF).
	if st, _ := post("/api/nodes/wa/drain", "application/x-www-form-urlencoded", ""); st != 415 {
		t.Fatalf("form post accepted: %d", st)
	}
	// SSE: subscribe, then trigger a node event.
	req, _ := http.NewRequest("GET", base+"/api/events?kinds=node", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data: ") {
				events <- sc.Text()
			}
		}
		close(events)
	}()
	if st, body := post("/api/nodes/wa/drain", "application/json", "{}"); st != 200 || !strings.Contains(body, `"draining":true`) {
		t.Fatalf("drain via API: %d %s", st, body)
	}
	select {
	case ev := <-events:
		if !strings.Contains(ev, "drained by web/ops") {
			t.Fatalf("event %s", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no SSE event")
	}
	post("/api/nodes/wa/undrain", "application/json", "{}")
	if st, body := post("/api/joincodes", "application/json", `{"role":"client","ttl":"5m"}`); st != 200 || !regexp.MustCompile(`"code":"[0-9A-Z]{3}-[0-9A-Z]{3}"`).MatchString(body) {
		t.Fatalf("join code via API: %d %s", st, body)
	}
	if code, body := get("/status"); code != 200 || !strings.Contains(body, "wa") {
		t.Fatalf("status page %d", code)
	}

	// Install scripts with the release store.
	rel := filepath.Join(h.root, "release")
	os.MkdirAll(rel, 0o755)
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	for tool, src := range map[string]string{"longarm": bins.longarm, "longarmd": bins.longarmd} {
		copyBin(t, src, filepath.Join(rel, fmt.Sprintf("%s-%s-%s%s", tool, runtime.GOOS, runtime.GOARCH, suffix)))
	}
	if out := h.hubCmd(t, "release", "add", rel, "--version", "1.2.3"); !strings.Contains(out, "1.2.3") {
		t.Fatalf("release add: %s", out)
	}
	for _, s := range []string{"/install.sh", "/install.ps1"} {
		code, body := get(s)
		if code != 200 || !strings.Contains(body, h.fp) || !strings.Contains(body, h.addr()) || !strings.Contains(body, "1.2.3") {
			t.Fatalf("%s %d:\n%s", s, code, body)
		}
	}
	code := strings.TrimSpace(h.mustLongarm(h.admin, "admin", "join-code", "--role", "worker", "--name", "inst"))
	prefix, dd := filepath.Join(h.root, "inst-bin"), filepath.Join(h.root, "inst-data")
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		pwsh, err := exec.LookPath("pwsh")
		if err != nil {
			t.Skip("pwsh not installed")
		}
		// LONGARM_NO_PATH / LONGARM_NO_FIREWALL: an elevated test run must not leave
		// the temp dir on the machine PATH or a firewall rule behind.
		script := fmt.Sprintf("$env:LONGARM_JOIN='%s'; $env:LONGARM_NO_SERVICE='1'; $env:LONGARM_NO_PATH='1'; $env:LONGARM_NO_FIREWALL='1'; $env:LONGARM_PREFIX='%s'; $env:LONGARM_DATA_DIR='%s'; irm %s/install.ps1 | iex",
			code, prefix, dd, base)
		cmd = exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", script)
	} else {
		if _, err := exec.LookPath("curl"); err != nil {
			t.Skip("curl not installed")
		}
		cmd = exec.Command("sh", "-c", fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- --join %s --no-service --prefix %s --data-dir %s", base, code, prefix, dd))
	}
	cmd.Env = append(os.Environ(), "HOME="+h.root, "USERPROFILE="+os.Getenv("USERPROFILE"))
	var ob bytes.Buffer
	cmd.Stdout, cmd.Stderr = &ob, &ob
	if err := cmd.Run(); err != nil {
		t.Fatalf("install script: %v\n%s", err, ob.String())
	}
	if !strings.Contains(ob.String(), "pending approval") {
		t.Fatalf("install output:\n%s", ob.String())
	}
	if _, err := os.Stat(filepath.Join(prefix, "longarmd"+suffix)); err != nil {
		t.Fatalf("longarmd not installed: %v\n%s", err, ob.String())
	}
	if b, _ := os.ReadFile(filepath.Join(dd, "hub.toml")); !strings.Contains(string(b), "poll_token") {
		t.Fatalf("no pending enrollment in %s: %s", dd, b)
	}
	h.waitNode(t, "inst", "pending", func(n map[string]any) bool { return n["state"] == "pending" })
	// A tampered binary is refused.
	f, _ := os.OpenFile(filepath.Join(h.hubDir, "releases", "1.2.3", fmt.Sprintf("longarm-%s-%s%s", runtime.GOOS, runtime.GOARCH, suffix)), os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte("tampered"))
	f.Close()
	code2 := strings.TrimSpace(h.mustLongarm(h.admin, "admin", "join-code"))
	if runtime.GOOS == "windows" {
		cmd = exec.Command("pwsh", "-NoProfile", "-NonInteractive", "-Command", fmt.Sprintf("$env:LONGARM_JOIN='%s'; $env:LONGARM_NO_SERVICE='1'; $env:LONGARM_NO_PATH='1'; $env:LONGARM_NO_FIREWALL='1'; $env:LONGARM_PREFIX='%s'; irm %s/install.ps1 | iex", code2, prefix+"2", base))
	} else {
		cmd = exec.Command("sh", "-c", fmt.Sprintf("curl -fsSL %s/install.sh | sh -s -- --join %s --no-service --prefix %s2", base, code2, prefix))
	}
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "mismatch") {
		t.Fatalf("tampered binary installed: %v\n%s", err, out)
	}
}

func copyBin(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}
