package hub

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

var linkCodeRE = regexp.MustCompile(`^[BCDFGHJKLMNPQRSTVWXZ]{4}-[BCDFGHJKLMNPQRSTVWXZ]{4}$`)

func TestLinkCodes(t *testing.T) {
	seen := map[string]bool{}
	for range 2000 {
		c := newLinkCode()
		if !linkCodeRE.MatchString(formatLinkCode(c)) {
			t.Fatalf("code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 1990 {
		t.Fatalf("%d distinct codes of 2000", len(seen))
	}
	for in, want := range map[string]string{"bcdf-ghjk": "BCDFGHJK", " BCDF GHJK ": "BCDFGHJK", "BCDFGHJK": "BCDFGHJK"} {
		if got, ok := normalizeLinkCode(in); !ok || got != want {
			t.Errorf("normalize %q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"BCDF-GHJ", "BCDF-GHJKL", "BCDF-GHJ0", "AEIO-UBCD", "BCDF-GHJK"} {
		if _, ok := normalizeLinkCode(in); ok {
			t.Errorf("normalize %q accepted", in)
		}
	}
}

// web is a browser of the panel.
type web struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newWeb(t *testing.T, h *Hub) *web {
	jar, _ := cookiejar.New(nil)
	return &web{t: t, base: "http://" + h.Addr("http"),
		c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (w *web) do(method, path string, body io.Reader, hdr map[string]string) (*http.Response, string) {
	w.t.Helper()
	req, _ := http.NewRequest(method, w.base+path, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := w.c.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (w *web) nav(path string) (*http.Response, string) {
	return w.do("GET", path, nil, map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"})
}

// post sends a form the way the hub's own page does; hdr overrides.
func (w *web) post(path string, form url.Values, hdr map[string]string) (int, string) {
	h := map[string]string{"Origin": w.base, "Sec-Fetch-Site": "same-origin", "Content-Type": "application/x-www-form-urlencoded"}
	for k, v := range hdr {
		if v == "" {
			delete(h, k)
		} else {
			h[k] = v
		}
	}
	resp, body := w.do("POST", path, strings.NewReader(form.Encode()), h)
	return resp.StatusCode, body
}

var (
	tokenRE = regexp.MustCompile(`name="token" value="([^"]+)"`)
	idRE    = regexp.MustCompile(`name="id" value="([^"]+)"`)
)

func field(t *testing.T, re *regexp.Regexp, body string) string {
	t.Helper()
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no %s in\n%s", re, body)
	}
	return m[1]
}

// entraSignIn signs w in to the panel with Microsoft (spa mode: the ID
// token the callback page would get, posted to /auth/spa-session).
func (w *web) entraSignIn(f *fakeEntra, oid, upn string) {
	w.t.Helper()
	resp, _ := w.nav("/auth/login?next=/link")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	claims := f.accessClaims(oid, upn)
	delete(claims, "scp")
	claims["nonce"] = loc.Query().Get("nonce")
	b, _ := json.Marshal(map[string]string{"id_token": f.sign(claims)})
	resp, body := w.do("POST", "/auth/spa-session", strings.NewReader(string(b)),
		map[string]string{"Origin": w.base, "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"})
	if resp.StatusCode != 200 || !strings.Contains(body, `/link`) {
		w.t.Fatalf("sign-in of %s: %d %s", upn, resp.StatusCode, body)
	}
}

// passwordSignIn logs w in with a panel password (+ TOTP).
func (w *web) passwordSignIn(h *Hub, login, role, tenant string) {
	w.t.Helper()
	secret, err := AddPanelUser(h.store, login, "correct horse battery", role, tenant, true)
	if err != nil {
		w.t.Fatal(err)
	}
	code, _ := TOTP(secret, time.Now())
	if c, body := w.post("/login", url.Values{"user": {login}, "password": {"correct horse battery"}, "totp": {code}}, nil); c != http.StatusSeeOther {
		w.t.Fatalf("password login %s: %d %s", login, c, body)
	}
}

// lookup types code at /link and returns the confirmation page.
func (w *web) lookup(code string) (int, string) {
	w.t.Helper()
	_, page := w.nav("/link")
	return w.post("/link", url.Values{"token": {field(w.t, tokenRE, page)}, "code": {code}}, nil)
}

// decide answers a confirmation page.
func (w *web) decide(confirm, decision string) (int, string) {
	w.t.Helper()
	return w.post("/link/decide", url.Values{"token": {field(w.t, tokenRE, confirm)}, "id": {field(w.t, idRE, confirm)},
		"decision": {decision}}, nil)
}

func enrollClient(t *testing.T, h *Hub) pb.EnrollClient {
	t.Helper()
	conn, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(
		&tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2"}})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewEnrollClient(conn)
}

type device struct {
	st     *pb.StartLinkResponse
	keyPEM []byte
}

func startLink(t *testing.T, ec pb.EnrollClient, kind, name string, tr *pki.CSRTrust) (*device, error) {
	t.Helper()
	keyPEM, csrPEM, err := pki.NewKeyAndCSR(name, tr)
	if err != nil {
		t.Fatal(err)
	}
	st, err := ec.StartLink(tctx(t), &pb.StartLinkRequest{Kind: kind, CsrPem: csrPEM, Name: name, Hostname: "box-" + kind + ".corp.example",
		Os: "linux", Arch: "amd64", Version: "1.2.3"})
	if err != nil {
		return nil, err
	}
	return &device{st: st, keyPEM: keyPEM}, nil
}

// poll asks about d's pairing as if the interval had passed.
func poll(t *testing.T, h *Hub, ec pb.EnrollClient, d *device) *pb.JoinResponse {
	t.Helper()
	h.pair.mu.Lock()
	for _, l := range h.pair.byPoll {
		l.lastPoll = time.Time{}
	}
	h.pair.mu.Unlock()
	r, err := ec.PollLink(tctx(t), &pb.PollLinkRequest{PollToken: d.st.PollToken})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	return r
}

func identityOf(t *testing.T, r *pb.JoinResponse) (pki.Identity, *pki.TrustExt) {
	t.Helper()
	c, err := pki.ParseCertPEM(r.CertPem)
	if err != nil {
		t.Fatalf("certificate of %v: %v", r, err)
	}
	id, err := pki.IdentityFromCert(c)
	if err != nil {
		t.Fatal(err)
	}
	te, err := pki.TrustOf(c)
	if err != nil {
		t.Fatal(err)
	}
	return id, te
}

// TestLinkEntra: pairing on a hub with Entra sign-in - only Microsoft
// sign-ins approve; clients become the approver's devices (admins':
// admin certificates), workers the approver's own workers (never admin);
// codes are typed, single use and expire; forms are CSRF-proof.
func TestLinkEntra(t *testing.T) {
	f := newFakeEntra(t)
	h := newEntraHub(t, f, func(e *EntraConfig) { e.Admins = []string{"boss@example.com"} })
	h.loginLimit, h.joinLimit = newIPLimiter(100, 100), newIPLimiter(100, 100) // every request comes from 127.0.0.1
	const (
		aliceOID = "a11ce000-0000-4000-8000-000000000101"
		bossOID  = "b0550000-0000-4000-8000-000000000102"
	)
	ec := enrollClient(t, h)
	base := "http://" + h.Addr("http")

	// a client device, with an end-to-end trust node key
	nk := ed25519.NewKeyFromSeed(make([]byte, 32))
	d, err := startLink(t, ec, linkClient, "", &pki.CSRTrust{NodeKey: nk})
	if err != nil {
		t.Fatal(err)
	}
	if !linkCodeRE.MatchString(d.st.UserCode) || d.st.VerificationUrl != base+"/link" || d.st.Interval.AsDuration() != 5*time.Second ||
		time.Until(d.st.ExpiresAt.AsTime()) > 10*time.Minute || time.Until(d.st.ExpiresAt.AsTime()) < 9*time.Minute {
		t.Fatalf("start: %v", d.st)
	}
	r, err := ec.PollLink(tctx(t), &pb.PollLinkRequest{PollToken: d.st.PollToken})
	if err != nil || r.State != linkPending {
		t.Fatalf("first poll: %v %v", r, err)
	}
	if _, err := ec.PollLink(tctx(t), &pb.PollLinkRequest{PollToken: d.st.PollToken}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("too fast a poll: %v", err)
	}
	if _, err := ec.PollLink(tctx(t), &pb.PollLinkRequest{PollToken: "nope"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown poll token: %v", err)
	}

	// Not signed in: off to Microsoft, back to /link.
	alice := newWeb(t, h)
	if resp, _ := alice.nav("/link"); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/auth/login?next=/link" {
		t.Fatalf("/link without a session: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// A password session does not count when the hub has Entra sign-in.
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "pat", Create: true}); err != nil {
		t.Fatal(err)
	}
	pat := newWeb(t, h)
	pat.passwordSignIn(h, "pat", "user", "pat")
	if resp, _ := pat.nav("/link"); resp.StatusCode != http.StatusFound {
		t.Fatalf("/link with a password session: %d", resp.StatusCode)
	}
	if c, body := pat.post("/link", url.Values{"code": {d.st.UserCode}}, nil); c != http.StatusUnauthorized || strings.Contains(body, "box-client") {
		t.Fatalf("password session posting a code: %d %s", c, body)
	}

	alice.entraSignIn(f, aliceOID, "alice@example.com")
	// A code in the URL is never taken.
	resp, page := alice.nav("/link?code=" + url.QueryEscape(d.st.UserCode) + "&user_code=" + url.QueryEscape(d.st.UserCode))
	if resp.StatusCode != 200 || strings.Contains(page, d.st.UserCode) || strings.Contains(page, "box-client") ||
		!strings.Contains(page, "Signed in as <b>alice</b>") || resp.Header.Get("Content-Security-Policy") != linkCSP {
		t.Fatalf("/link page: %d %s", resp.StatusCode, page)
	}
	tok := field(t, tokenRE, page)
	form := url.Values{"token": {tok}, "code": {strings.ToLower(d.st.UserCode)}}
	// CSRF: no Origin, a foreign Origin, cross-site fetch metadata, a bad or
	// another session's token; the code in the query string only.
	for what, c := range map[string]func() (int, string){
		"no origin": func() (int, string) { return alice.post("/link", form, map[string]string{"Origin": ""}) },
		"foreign origin": func() (int, string) {
			return alice.post("/link", form, map[string]string{"Origin": "https://evil.example"})
		},
		"cross-site": func() (int, string) {
			return alice.post("/link", form, map[string]string{"Sec-Fetch-Site": "cross-site"})
		},
		"bad token": func() (int, string) {
			return alice.post("/link", url.Values{"token": {tok + "x"}, "code": {d.st.UserCode}}, nil)
		},
		"no token": func() (int, string) { return alice.post("/link", url.Values{"code": {d.st.UserCode}}, nil) },
		"query code": func() (int, string) {
			return alice.post("/link?code="+url.QueryEscape(d.st.UserCode), url.Values{"token": {tok}}, nil)
		},
	} {
		if code, body := c(); code == 200 || strings.Contains(body, "box-client") {
			t.Errorf("%s: %d %s", what, code, body)
		}
	}
	code, confirm := alice.post("/link", form, nil)
	if code != 200 || !strings.Contains(confirm, "box-client.corp.example") || !strings.Contains(confirm, "127.0.0.1") ||
		!strings.Contains(confirm, pki.KeyID(nk.Public().(ed25519.PublicKey))) || !strings.Contains(confirm, "client device") ||
		!strings.Contains(confirm, "linux/amd64, version 1.2.3") || strings.Contains(confirm, "administrator") {
		t.Fatalf("confirmation: %d %s", code, confirm)
	}
	// Another session's form token is no good.
	bob := newWeb(t, h)
	bob.entraSignIn(f, "b0b00000-0000-4000-8000-000000000103", "bob@example.com")
	if c, _ := bob.decide(confirm, "approve"); c != http.StatusForbidden {
		t.Fatalf("another session's form: %d", c)
	}
	if c, body := alice.decide(confirm, "approve"); c != 200 || !strings.Contains(body, "Approved: device alice-box-client is signed in as alice (client)") {
		t.Fatalf("approve: %d %s", c, body)
	}
	r = poll(t, h, ec, d)
	id, te := identityOf(t, r)
	if r.State != linkApproved || id.Role != pki.RoleClient || id.User != "alice" || id.Name != "alice-box-client" || te == nil {
		t.Fatalf("approved client: %v %v %v", r, id, te)
	}
	if _, err := tls.X509KeyPair(r.CertPem, d.keyPEM); err != nil {
		t.Fatal(err)
	}
	// single use: the code is gone; the decision can't be replayed
	if c, body := alice.lookup(d.st.UserCode); c != http.StatusNotFound || !strings.Contains(body, "No device is waiting") {
		t.Fatalf("code used twice: %d %s", c, body)
	}
	if c, _ := alice.decide(confirm, "deny"); c != http.StatusGone {
		t.Fatalf("decision replayed: %d", c)
	}

	// A worker: alice's own, approved at once; the hub pushes its access.
	dw, err := startLink(t, ec, linkWorker, "aw", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, confirm = alice.lookup(dw.st.UserCode)
	if !strings.Contains(confirm, "Only confirm if this is your machine: jobs you run may be placed on it.") || !strings.Contains(confirm, "worker (runs jobs)") {
		t.Fatalf("worker confirmation: %s", confirm)
	}
	if c, body := alice.decide(confirm, "approve"); c != 200 || !strings.Contains(body, "worker aw is yours") {
		t.Fatalf("approve worker: %d %s", c, body)
	}
	r = poll(t, h, ec, dw)
	if id, _ := identityOf(t, r); r.Owner != "alice" || id.Role != pki.RoleWorker || id.Name != "aw" {
		t.Fatalf("approved worker: %v %v", r, id)
	}
	h.mu.Lock()
	n := h.nodeLocked("aw")
	ok := n.rec.Owner == "alice" && n.rec.State == stateApproved && h.mayUseLocked("alice", n) && !h.mayUseLocked("bob", n)
	h.mu.Unlock()
	if !ok {
		t.Fatalf("worker aw: %+v", n.rec)
	}
	if wa := connectWorker(t, h, r, dw.keyPEM); wa.GetOwner() != "alice" {
		t.Fatalf("worker access pushed: %v", wa)
	}

	// An admin: clients get admin certificates, workers never.
	boss := newWeb(t, h)
	boss.entraSignIn(f, bossOID, "boss@example.com")
	dc, _ := startLink(t, ec, linkClient, "bossbox", nil)
	_, confirm = boss.lookup(dc.st.UserCode)
	if !strings.Contains(confirm, "an <b>administrator</b> certificate") {
		t.Fatalf("admin client confirmation: %s", confirm)
	}
	boss.decide(confirm, "approve")
	if id, _ := identityOf(t, poll(t, h, ec, dc)); id.Role != pki.RoleAdmin || id.User != "boss" {
		t.Fatalf("admin's client: %v", id)
	}
	dw2, _ := startLink(t, ec, linkWorker, "bw", nil)
	_, confirm = boss.lookup(dw2.st.UserCode)
	boss.decide(confirm, "approve")
	if r := poll(t, h, ec, dw2); r.Role != string(pki.RoleWorker) || r.Owner != "boss" {
		t.Fatalf("admin's worker: %v", r)
	}
	// A taken worker name: the device is told why.
	dw3, _ := startLink(t, ec, linkWorker, "aw", nil)
	_, confirm = alice.lookup(dw3.st.UserCode)
	if c, body := alice.decide(confirm, "approve"); c != http.StatusConflict || !strings.Contains(body, "taken") {
		t.Fatalf("taken name: %d %s", c, body)
	}
	if r := poll(t, h, ec, dw3); r.State != linkDenied || !strings.Contains(r.Message, "taken") {
		t.Fatalf("taken name, device: %v", r)
	}

	// Denied.
	dd, _ := startLink(t, ec, linkClient, "", nil)
	_, confirm = alice.lookup(dd.st.UserCode)
	if c, body := alice.decide(confirm, "deny"); c != 200 || !strings.Contains(body, "Denied") {
		t.Fatalf("deny: %d %s", c, body)
	}
	if r := poll(t, h, ec, dd); r.State != linkDenied || len(r.CertPem) != 0 {
		t.Fatalf("denied: %v", r)
	}

	// Expired.
	de, _ := startLink(t, ec, linkClient, "", nil)
	h.pair.mu.Lock()
	for _, l := range h.pair.byCode {
		l.expires = time.Now().Add(-time.Second)
	}
	h.pair.mu.Unlock()
	if c, _ := alice.lookup(de.st.UserCode); c != http.StatusNotFound {
		t.Fatalf("expired code looked up: %d", c)
	}
	if r := poll(t, h, ec, de); r.State != linkExpired {
		t.Fatalf("expired: %v", r)
	}

	// A disabled user's session approves nothing.
	dx, _ := startLink(t, ec, linkClient, "", nil)
	_, confirm = alice.lookup(dx.st.UserCode)
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "alice", SetDisabled: true, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if c, _ := alice.decide(confirm, "approve"); c == http.StatusOK { // its sessions ended
		t.Fatalf("disabled user approved: %d", c)
	}
	if r := poll(t, h, ec, dx); r.State != linkPending {
		t.Fatalf("after a disabled user's attempt: %v", r)
	}

	// Audited: start, approve, deny, expire.
	res, err := h.Audit(&pb.AuditRequest{Node: "hub", Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, a := range res.Records {
		got[a.Method]++
	}
	if got["link/start"] != 8 || got["link/approve"] != 5 || got["link/deny"] != 1 || got["link/expire"] != 1 {
		t.Errorf("audit: %v", got)
	}
}

// connectWorker opens a worker session with r's certificate and returns
// the worker access the hub pushes.
func connectWorker(t *testing.T, h *Hub, r *pb.JoinResponse, keyPEM []byte) *pb.WorkerAccess {
	t.Helper()
	b := writeTestBundle(t, h, &hubclient.JoinResult{Resp: r, KeyPEM: keyPEM})
	conn, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := pb.NewHubNodeClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Send(&pb.NodeReport{Msg: &pb.NodeReport_Hello{Hello: &pb.NodeHello{Version: "test"}}}); err != nil {
		t.Fatal(err)
	}
	for {
		m, err := st.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if wa := m.GetWorkerAccess(); wa != nil {
			return wa
		}
	}
}

// TestLinkPassword: a hub without Entra sign-in takes any panel login of
// a Longarm user; hubclient.Link end to end; the limits.
func TestLinkPassword(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	alice := newWeb(t, h)
	alice.passwordSignIn(h, "alice-web", "user", "alice")
	root := newWeb(t, h)
	root.passwordSignIn(h, "root-web", "admin", "")
	if resp, body := root.nav("/link"); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "not a Longarm user") {
		t.Fatalf("an admin login without a user: %d %s", resp.StatusCode, body)
	}
	anon := newWeb(t, h)
	if resp, body := anon.nav("/link"); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, `href="/status"`) {
		t.Fatalf("no session: %d %s", resp.StatusCode, body)
	}

	// hubclient.Link: shows the code, waits for the approval.
	codeCh := make(chan string, 1)
	done := make(chan error, 1)
	var res *hubclient.JoinResult
	go func() {
		var err error
		res, err = hubclient.Link(context.Background(), h.Addr("grpc"), hubclient.LinkOptions{Kind: linkClient,
			JoinOptions: hubclient.JoinOptions{Name: "laptop", Fingerprint: pki.Fingerprint(th.ca.Cert)},
			Show:        func(st *pb.StartLinkResponse) { codeCh <- st.UserCode }})
		done <- err
	}()
	var code string
	select {
	case code = <-codeCh:
	case err := <-done:
		t.Fatal(err)
	}
	_, confirm := alice.lookup(code)
	if c, body := alice.decide(confirm, "approve"); c != 200 || !strings.Contains(body, "device laptop is signed in as alice") {
		t.Fatalf("approve: %d %s", c, body)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Link did not finish")
	}
	if id, _ := identityOf(t, res.Resp); id.User != "alice" || id.Role != pki.RoleClient || id.Name != "laptop" || !res.Pinned {
		t.Fatalf("linked: %v %v", id, res)
	}

	// Limits: pending pairings per address, StartLink attempts, code lookups.
	ec := enrollClient(t, h)
	var err error
	for i := range maxLinksPerIP + 1 {
		if _, err = startLink(t, ec, linkWorker, "", nil); i < maxLinksPerIP && err != nil {
			t.Fatal(err)
		}
	}
	if status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("pending pairings beyond the limit: %v", err)
	}
	for range 10 {
		_, err = startLink(t, ec, linkWorker, "", nil)
	}
	if status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "too many join attempts") {
		t.Fatalf("StartLink rate: %v", err)
	}
	if _, err := startLink(t, ec, "root", "", nil); err == nil {
		t.Fatal("kind root accepted")
	}
	var c int
	for range 12 {
		c, _ = alice.lookup("BCDF-GHJK")
	}
	if c != http.StatusTooManyRequests {
		t.Fatalf("code lookups: %d", c)
	}
}

// TestEntraJoinWorker: longarmd login --az - a user's token enrolls an
// owned worker; app-only tokens may not.
func TestEntraJoinWorker(t *testing.T) {
	f := newFakeEntra(t)
	h := newEntraHub(t, f, func(e *EntraConfig) {
		e.Apps = []string{ciApp + "=ci"}
		e.Admins = []string{"boss@example.com"}
	})
	h.joinLimit = newIPLimiter(100, 100)
	login := func(tok, name string) (*hubclient.JoinResult, error) {
		return hubclient.Login(tctx(t), h.Addr("grpc"), hubclient.LoginOptions{Kind: linkWorker,
			JoinOptions: hubclient.JoinOptions{Name: name, Fingerprint: pki.Fingerprint(h.ca.Cert)},
			Token:       func(context.Context, *pb.AuthInfoResponse) (string, error) { return tok, nil }})
	}
	if _, err := login(f.sign(f.appClaims(ciApp)), "ciw"); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "app-only") {
		t.Fatalf("app-only token enrolled a worker: %v", err)
	}
	r, err := login(f.sign(f.accessClaims("b0550000-0000-4000-8000-000000000201", "boss@example.com")), "bw")
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := identityOf(t, r.Resp); r.Resp.Owner != "boss" || id.Role != pki.RoleWorker || id.Name != "bw" {
		t.Fatalf("admin's worker: %v %v", r.Resp, id)
	}
	if _, err := login(f.sign(f.accessClaims("a11ce000-0000-4000-8000-000000000202", "alice@example.com")), "bw"); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("taken worker name: %v", err)
	}
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "boss", SetDisabled: true, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := login(f.sign(f.accessClaims("b0550000-0000-4000-8000-000000000201", "boss@example.com")), "bw2"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("disabled user enrolled a worker: %v", err)
	}
}

// TestInstallScriptsLogin: --login / -Login sign in instead of joining; the
// scripts still parse.
func TestInstallScriptsLogin(t *testing.T) {
	th := newTenancyHub(t)
	base := "http://" + th.h.Addr("http")
	sh, ps1 := "", ""
	for _, s := range []struct {
		path string
		out  *string
		want []string
	}{
		{"/install.sh", &sh, []string{`--login) LOGIN=1`, `LOGIN="${LONGARM_LOGIN:-0}"`, `"$PREFIX/longarmd" login "$HUB_ADDR" --ca-fingerprint "$CA_FP"`,
			`"$PREFIX/longarm" login "$HUB_ADDR"`, `$AS longarmd login "$HUB_ADDR"`, `"$PREFIX/longarmd" join "$HUB_ADDR" "$JOIN"`,
			`--ca-fingerprint "$CA_FP" --role "$R"`, `--role "$ROLE" $NAMEARG`,
			`LABELARGS="$LABELARGS --label $2"`, `--role "$R" $NAMEARG $DD $LABELARGS`, `--role "$ROLE" $NAMEARG $LABELARGS`,
			`if [ "$LOGIN" = 1 ]; then ROLE=both; else ROLE=worker; fi`}},
		{"/install.ps1", &ps1, []string{`[switch]$Login = ($env:LONGARM_LOGIN -eq '1')`, `'longarmd.exe') login $HubAddr --ca-fingerprint $CaFp`,
			`'longarm.exe') login $HubAddr`, `-not $Login)`, `'longarmd.exe') join $HubAddr $Join`, `--ca-fingerprint $CaFp --role $Role`,
			`[string]$Label = $env:LONGARM_LABELS`, `"LABELS=$Label"`, `@nameArgs @ddArgs @labelArgs`,
			`if (-not $Role) { $Role = if ($Login) { 'both' } else { 'worker' } }`}},
	} {
		code, body := plainGet(t, base+s.path)
		if code != 200 {
			t.Fatalf("%s: %d", s.path, code)
		}
		for _, w := range s.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s without %q", s.path, w)
			}
		}
		*s.out = body
	}
	if p, err := exec.LookPath("sh"); err == nil {
		cmd := exec.Command(p, "-n")
		cmd.Stdin = strings.NewReader(sh)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("install.sh: %v %s", err, out)
		}
	}
	if p, err := exec.LookPath("pwsh"); err == nil {
		f := filepath.Join(t.TempDir(), "install.ps1")
		os.WriteFile(f, []byte(ps1), 0o600)
		cmd := exec.Command(p, "-NoProfile", "-NonInteractive", "-Command",
			"$e=$null; [void][System.Management.Automation.Language.Parser]::ParseFile('"+f+"', [ref]$null, [ref]$e); if ($e) { $e; exit 1 }")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("install.ps1: %v %s", err, out)
		}
	}
}

// TestLinkBounds: what StartLink keeps is bounded (CSR size, no trailing
// data, device fields), finished pairings stop counting against the limits
// and are forgotten soon after their expiry; an approved result is
// delivered once, before the expiry, and only while the node is approved.
func TestLinkBounds(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	h.joinLimit = newIPLimiter(100, 100)
	ec := enrollClient(t, h)
	_, csr, err := pki.NewKeyAndCSR("", nil)
	if err != nil {
		t.Fatal(err)
	}
	start := func(csr []byte, hostname, version string) error {
		_, err := ec.StartLink(tctx(t), &pb.StartLinkRequest{Kind: linkWorker, CsrPem: csr, Hostname: hostname, Os: "linux", Arch: "amd64", Version: version})
		return err
	}
	for what, err := range map[string]error{
		"padded CSR":         start(append(append([]byte{}, csr...), make([]byte, pki.MaxCSR)...), "box", "1"),
		"trailing data":      start(append(append([]byte{}, csr...), "junk"...), "box", "1"),
		"second PEM block":   start(append(append([]byte{}, csr...), csr...), "box", "1"),
		"long hostname":      start(csr, strings.Repeat("h", 256), "1"),
		"control characters": start(csr, "box\x1b[2J", "1"),
		"long version":       start(csr, "box", strings.Repeat("1", 65)),
	} {
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", what, err)
		}
	}
	if err := start(append(append([]byte{}, csr...), "\n\n"...), "box", "1"); err != nil {
		t.Fatalf("CSR with trailing newlines: %v", err)
	}
	h.pair.mu.Lock()
	for _, l := range h.pair.byPoll {
		if string(l.csr) != string(csr) {
			t.Errorf("kept CSR %q, want the re-encoded %q", l.csr, csr)
		}
	}
	h.pair.mu.Unlock()

	// Expired pairings drop their CSR (and stop counting) at once, and are
	// forgotten a minute later.
	for range maxLinksPerIP - 1 {
		if err := start(csr, "box", "1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := start(csr, "box", "1"); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("over the limit: %v", err)
	}
	expire := func(by time.Duration) {
		h.pair.mu.Lock()
		for _, l := range h.pair.byPoll {
			l.expires = time.Now().Add(-by)
		}
		h.pair.mu.Unlock()
	}
	expire(time.Second)
	if err := start(csr, "box", "1"); err != nil {
		t.Fatalf("expired pairings still count: %v", err)
	}
	expire(2 * time.Minute)
	h.pair.mu.Lock()
	h.pair.sweepLocked(time.Now())
	n := len(h.pair.byPoll)
	h.pair.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d pairings kept after expiry and grace", n)
	}

	alice := newWeb(t, h)
	alice.passwordSignIn(h, "alice-web", "user", "alice")
	approved := func(name string) *device {
		t.Helper()
		d, err := startLink(t, ec, linkWorker, name, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, confirm := alice.lookup(d.st.UserCode)
		if c, body := alice.decide(confirm, "approve"); c != 200 {
			t.Fatalf("approve %s: %d %s", name, c, body)
		}
		return d
	}
	// once
	d := approved("once")
	if r := poll(t, h, ec, d); r.State != linkApproved || len(r.CertPem) == 0 {
		t.Fatalf("first poll: %v", r)
	}
	if _, err := ec.PollLink(tctx(t), &pb.PollLinkRequest{PollToken: d.st.PollToken}); status.Code(err) != codes.NotFound {
		t.Fatalf("second poll of a delivered result: %v", err)
	}
	// revoked before the device collected it: no certificate (nor tailnet key)
	d = approved("gone")
	if _, err := h.Revoke("test", "gone"); err != nil {
		t.Fatal(err)
	}
	if r := poll(t, h, ec, d); r.State != linkDenied || len(r.CertPem) != 0 || r.Tailnet != nil {
		t.Fatalf("revoked before delivery: %v", r)
	}
	// past the expiry and its grace: forgotten
	d = approved("late")
	expire(2 * time.Minute)
	if _, err := ec.PollLink(tctx(t), &pb.PollLinkRequest{PollToken: d.st.PollToken}); status.Code(err) != codes.NotFound {
		t.Fatalf("poll past the expiry: %v", err)
	}
}

// Browsers send "Origin: null" with form posts from a page whose referrer
// policy is no-referrer: the hub's forms would then fail sameSitePost.
func TestReferrerPolicyKeepsOrigin(t *testing.T) {
	rec := httptest.NewRecorder()
	securityHeaders(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", "/link", nil))
	if p := rec.Header().Get("Referrer-Policy"); p == "no-referrer" || p == "" {
		t.Fatalf("Referrer-Policy %q: form posts would carry Origin: null", p)
	}
	h := &Hub{}
	r := httptest.NewRequest("POST", "https://hub.example.com/link", nil)
	r.Header.Set("Origin", "null")
	if h.sameSitePost(r) {
		t.Fatal("Origin: null accepted")
	}
}

// startBoth starts a kind "both" pairing: the client's key (with node key
// nk) and the worker's (with node key wk).
func startBoth(t *testing.T, ec pb.EnrollClient, name, workerName string, nk, wk ed25519.PrivateKey) (*device, []byte, error) {
	t.Helper()
	keyPEM, csrPEM, err := pki.NewKeyAndCSR(name, &pki.CSRTrust{NodeKey: nk})
	if err != nil {
		t.Fatal(err)
	}
	wkeyPEM, wcsrPEM, err := pki.NewKeyAndCSR(workerName, &pki.CSRTrust{NodeKey: wk})
	if err != nil {
		t.Fatal(err)
	}
	st, err := ec.StartLink(tctx(t), &pb.StartLinkRequest{Kind: linkBoth, CsrPem: csrPEM, Name: name, Hostname: "dual.corp.example",
		Os: "windows", Arch: "amd64", Version: "1.2.3", WorkerCsrPem: wcsrPEM, WorkerName: workerName})
	if err != nil {
		return nil, nil, err
	}
	return &device{st: st, keyPEM: keyPEM}, wkeyPEM, nil
}

// TestLinkBoth: one code, one approval enrolls a machine as the
// approver's client device and own worker; the page says both; a failed
// half takes the other back; old kinds and bad requests.
func TestLinkBoth(t *testing.T) {
	f := newFakeEntra(t)
	h := newEntraHub(t, f, nil)
	h.loginLimit, h.joinLimit = newIPLimiter(100, 100), newIPLimiter(100, 100)
	ec := enrollClient(t, h)
	nk := ed25519.NewKeyFromSeed(make([]byte, 32))
	seed := make([]byte, 32)
	seed[0] = 1
	wk := ed25519.NewKeyFromSeed(seed)

	// requests that don't fit together
	_, csrPEM, _ := pki.NewKeyAndCSR("", nil)
	for what, req := range map[string]*pb.StartLinkRequest{
		"both without a worker CSR": {Kind: linkBoth, CsrPem: csrPEM},
		"client with a worker CSR":  {Kind: linkClient, CsrPem: csrPEM, WorkerCsrPem: csrPEM},
		"worker CSR not a CSR":      {Kind: linkBoth, CsrPem: csrPEM, WorkerCsrPem: []byte("nope")},
		"bad worker name":           {Kind: linkBoth, CsrPem: csrPEM, WorkerCsrPem: csrPEM, WorkerName: "no/slash"},
		"one name for both":         {Kind: linkBoth, CsrPem: csrPEM, WorkerCsrPem: csrPEM, Name: "same", WorkerName: "same"},
	} {
		req.Hostname, req.Os, req.Arch, req.Version = "h", "linux", "amd64", "1"
		if _, err := ec.StartLink(tctx(t), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", what, err)
		}
	}

	d, wkeyPEM, err := startBoth(t, ec, "", "", nk, wk)
	if err != nil {
		t.Fatal(err)
	}
	alice := newWeb(t, h)
	alice.entraSignIn(f, "a11ce000-0000-4000-8000-000000000301", "alice@example.com")
	code, confirm := alice.lookup(d.st.UserCode)
	for _, want := range []string{"Approving does <b>two</b> things", "client device (acts as you) + worker (runs jobs)",
		"device name", "worker name", "(default: the host name)", "dual.corp.example", "windows/amd64",
		pki.KeyID(nk.Public().(ed25519.PublicKey)), pki.KeyID(wk.Public().(ed25519.PublicKey)),
		"it gets a certificate that acts as you", "jobs you run may be placed on it"} {
		if !strings.Contains(confirm, want) {
			t.Errorf("confirmation without %q", want)
		}
	}
	if code != 200 || t.Failed() {
		t.Fatalf("confirmation: %d %s", code, confirm)
	}
	if c, body := alice.decide(confirm, "approve"); c != 200 ||
		!strings.Contains(body, "Approved: device alice-dual is signed in as alice (client), and worker dual is yours") {
		t.Fatalf("approve: %d %s", c, body)
	}
	r := poll(t, h, ec, d)
	id, te := identityOf(t, r)
	if r.State != linkApproved || id.Role != pki.RoleClient || id.User != "alice" || id.Name != "alice-dual" || te == nil ||
		!bytes.Equal(te.NodeKey, nk.Public().(ed25519.PublicKey)) {
		t.Fatalf("client half: %v %v %v", r, id, te)
	}
	if r.Worker == nil {
		t.Fatalf("no worker half: %v", r)
	}
	wid, wte := identityOf(t, r.Worker)
	if r.Worker.State != linkApproved || wid.Role != pki.RoleWorker || wid.Name != "dual" || r.Worker.Owner != "alice" ||
		wte == nil || !bytes.Equal(wte.NodeKey, wk.Public().(ed25519.PublicKey)) {
		t.Fatalf("worker half: %v %v %v", r.Worker, wid, wte)
	}
	if _, err := tls.X509KeyPair(r.CertPem, d.keyPEM); err != nil {
		t.Fatal(err)
	}
	if wa := connectWorker(t, h, r.Worker, wkeyPEM); wa.Owner != "alice" {
		t.Fatalf("worker access: %v", wa)
	}
	if _, err := ec.PollLink(tctx(t), &pb.PollLinkRequest{PollToken: d.st.PollToken}); status.Code(err) != codes.NotFound {
		t.Fatalf("result delivered twice: %v", err)
	}

	// The worker name is taken: nothing is enrolled.
	d, _, err = startBoth(t, ec, "", "dual", nk, wk)
	if err != nil {
		t.Fatal(err)
	}
	_, confirm = alice.lookup(d.st.UserCode)
	if c, body := alice.decide(confirm, "approve"); c != http.StatusConflict || !strings.Contains(body, "is taken") {
		t.Fatalf("taken worker name: %d %s", c, body)
	}
	if r := poll(t, h, ec, d); r.State != linkDenied || r.CertPem != nil || r.Worker != nil {
		t.Fatalf("failed approval delivered: %v", r)
	}
	// The device's name is taken (by the worker): the new worker is taken back.
	d, _, err = startBoth(t, ec, "dual", "dual2", nk, wk)
	if err != nil {
		t.Fatal(err)
	}
	_, confirm = alice.lookup(d.st.UserCode)
	if c, _ := alice.decide(confirm, "approve"); c != http.StatusConflict {
		t.Fatalf("taken device name: %d", c)
	}
	h.mu.Lock()
	n := h.nodeLocked("dual2")
	h.mu.Unlock()
	if n == nil || n.rec.State != stateRevoked {
		t.Fatalf("worker of a failed pairing: %+v", n)
	}
}

// TestLinkBothClient: hubclient.Link with kind both, end to end - one
// code, both certificates, each on its own key.
func TestLinkBothClient(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	alice := newWeb(t, h)
	alice.passwordSignIn(h, "alice-web", "user", "alice")
	codeCh := make(chan string, 1)
	done := make(chan error, 1)
	var res *hubclient.JoinResult
	go func() {
		var err error
		res, err = hubclient.Link(context.Background(), h.Addr("grpc"), hubclient.LinkOptions{Kind: linkBoth,
			JoinOptions: hubclient.JoinOptions{Name: "laptop", Fingerprint: pki.Fingerprint(th.ca.Cert)},
			Worker:      hubclient.JoinOptions{Name: "laptop-w"},
			Show:        func(st *pb.StartLinkResponse) { codeCh <- st.UserCode }})
		done <- err
	}()
	var code string
	select {
	case code = <-codeCh:
	case err := <-done:
		t.Fatal(err)
	}
	_, confirm := alice.lookup(code)
	if !strings.Contains(confirm, "<code>laptop-w</code>") {
		t.Fatalf("confirmation: %s", confirm)
	}
	if c, body := alice.decide(confirm, "approve"); c != 200 || !strings.Contains(body, "worker laptop-w is yours") {
		t.Fatalf("approve: %d %s", c, body)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Link did not finish")
	}
	id, _ := identityOf(t, res.Resp)
	if res.Worker == nil {
		t.Fatalf("no worker: %+v", res)
	}
	wid, _ := identityOf(t, res.Worker.Resp)
	if id.Name != "laptop" || id.Role != pki.RoleClient || wid.Name != "laptop-w" || wid.Role != pki.RoleWorker ||
		res.Worker.Resp.Owner != "alice" || !res.Pinned || !res.Worker.Pinned || string(res.KeyPEM) == string(res.Worker.KeyPEM) {
		t.Fatalf("linked: %v %v %+v", id, wid, res)
	}
	if _, err := tls.X509KeyPair(res.Worker.Resp.CertPem, res.Worker.KeyPEM); err != nil {
		t.Fatal(err)
	}
}

// TestEntraLoginBothRollback: az sign-in of both roles is all or nothing
// - a device that fails (its name taken) takes the worker just enrolled
// back (revoked through its own certificate), so a retry may use the
// worker's name again.
func TestEntraLoginBothRollback(t *testing.T) {
	f := newFakeEntra(t)
	h := newEntraHub(t, f, nil)
	h.joinLimit = newIPLimiter(100, 100)
	fp := pki.Fingerprint(h.ca.Cert)
	tok := func(oid, upn string) func(context.Context, *pb.AuthInfoResponse) (string, error) {
		return func(context.Context, *pb.AuthInfoResponse) (string, error) {
			return f.sign(f.accessClaims(oid, upn)), nil
		}
	}
	alice := tok("a11ce000-0000-4000-8000-000000000401", "alice@example.com")
	both := func(device, worker string) (*hubclient.JoinResult, *hubclient.JoinResult, error) {
		return hubclient.LoginBoth(tctx(t), h.Addr("grpc"),
			hubclient.LoginOptions{JoinOptions: hubclient.JoinOptions{Name: device, Fingerprint: fp}, Token: alice},
			hubclient.LoginOptions{JoinOptions: hubclient.JoinOptions{Name: worker, Fingerprint: fp}, Token: alice})
	}
	// bob's worker holds the name alice's device asks for
	if _, err := hubclient.Login(tctx(t), h.Addr("grpc"), hubclient.LoginOptions{Kind: linkWorker,
		JoinOptions: hubclient.JoinOptions{Name: "taken", Fingerprint: fp},
		Token:       tok("b0b00000-0000-4000-8000-000000000402", "bob@example.com")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := both("taken", "aw"); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("device name taken: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.mu.Lock()
		n := h.nodeLocked("aw")
		revoked := n != nil && n.rec.State == stateRevoked
		h.mu.Unlock()
		if revoked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worker of a failed sign-in was not taken back: %+v", n)
		}
		time.Sleep(100 * time.Millisecond)
	}
	cr, wr, err := both("alice-dev", "aw")
	if err != nil {
		t.Fatalf("retry with the same worker name: %v", err)
	}
	if cr.Resp.Name != "alice-dev" || wr.Resp.Name != "aw" || wr.Resp.Owner != "alice" {
		t.Fatalf("retry: %v %v", cr.Resp, wr.Resp)
	}
}
