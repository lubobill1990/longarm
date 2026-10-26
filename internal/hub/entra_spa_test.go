package hub

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

const (
	ciApp   = "c1c1c1c1-0000-4000-8000-000000000001"
	otherA  = "0e0e0e0e-0000-4000-8000-000000000002"
	ciSPOID = "5e5e5e5e-0000-4000-8000-000000000003"
	userOID = "a11ce000-0000-4000-8000-000000000009"
)

// appClaims are the claims of a client-credentials token of app for the
// hub's app (api://<client id>/.default).
func (f *fakeEntra) appClaims(app string) map[string]any {
	now := time.Now().Unix()
	return map[string]any{"aud": testClient, "iss": f.issuer(), "tid": testTenant, "oid": ciSPOID, "sub": ciSPOID,
		"idtyp": "app", "azp": app, "ver": "2.0", "iat": now, "nbf": now, "exp": now + 3600}
}

// TestEntraAppTokens: app-only tokens (CI) count only for listed apps, on
// the CLI path only, as their configured user and never as admins.
func TestEntraAppTokens(t *testing.T) {
	f := newFakeEntra(t)
	cfg := f.config()
	cfg.Apps = []string{ciApp + "=ci"}
	cfg.Admins = []string{ciSPOID} // apps are never admins, whatever the lists say
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	keys := newJWKS(f.srv.URL + "/" + testTenant + "/discovery/v2.0/keys")
	access := tokenCheck{issuer: f.issuer(), tenant: testTenant, audiences: []string{testClient, "api://" + testClient},
		scope: "longarm.connect", apps: cfg.apps, appAud: testClient}
	verify := func(c map[string]any, want tokenCheck) (*entraClaims, error) {
		return verifyEntraToken(context.Background(), keys, f.sign(c), want, time.Now())
	}
	if c, err := verify(f.appClaims(ciApp), access); err != nil || c.App != ciApp {
		t.Fatalf("listed app: %v %v", c, err)
	}
	noIdtyp := f.appClaims(ciApp) // the idtyp optional claim off: no scp is enough
	delete(noIdtyp, "idtyp")
	if c, err := verify(noIdtyp, access); err != nil || c.App != ciApp {
		t.Fatalf("listed app without idtyp: %v %v", c, err)
	}
	mod := func(c map[string]any, k string, v any) map[string]any {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
		return c
	}
	for name, c := range map[string]struct {
		claims map[string]any
		want   string
	}{
		"unlisted app":          {f.appClaims(otherA), "may not sign in"},
		"unlisted, no idtyp":    {mod(f.appClaims(otherA), "idtyp", nil), "lacks the scope"},
		"api:// audience":       {mod(f.appClaims(ciApp), "aud", "api://"+testClient), "audience"},
		"user token, no scp":    {mod(f.accessClaims(userOID, "a@example.com"), "scp", nil), "lacks the scope"},
		"user token, other scp": {mod(f.accessClaims(userOID, "a@example.com"), "scp", "x"), "lacks the scope"},
		"listed app, wrong tid": {mod(f.appClaims(ciApp), "tid", "99999999-2222-3333-4444-555555555555"), "tenant"},
	} {
		if _, err := verify(c.claims, access); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// The web paths pass no apps: refused there.
	idc := tokenCheck{issuer: f.issuer(), tenant: testTenant, audiences: []string{testClient}}
	if _, err := verify(f.appClaims(ciApp), idc); err == nil || !strings.Contains(err.Error(), "not accepted here") {
		t.Fatalf("app token on a web path: %v", err)
	}

	// EntraJoin as user ci, created on first use, a client certificate.
	h := newEntraHub(t, f, func(e *EntraConfig) { e.Apps, e.Admins = cfg.Apps, cfg.Admins })
	r, err := hubclient.Login(tctx(t), h.Addr("grpc"), hubclient.LoginOptions{
		JoinOptions: hubclient.JoinOptions{Name: "runner-1", Fingerprint: pki.Fingerprint(h.ca.Cert)},
		Token:       func(context.Context, *pb.AuthInfoResponse) (string, error) { return f.sign(f.appClaims(ciApp)), nil }})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := pki.ParseCertPEM(r.Resp.CertPem)
	if id, _ := pki.IdentityFromCert(c); id.Role != pki.RoleClient || id.User != "ci" || id.Name != "runner-1" {
		t.Fatalf("app identity %v", id)
	}
	h.mu.Lock()
	ci := h.tenants["ci"]
	h.mu.Unlock()
	if ci == nil || ci.u.CreatedBy != "entra-app" {
		t.Fatalf("user ci: %v", ci)
	}
	_, err = hubclient.Login(tctx(t), h.Addr("grpc"), hubclient.LoginOptions{
		Token: func(context.Context, *pb.AuthInfoResponse) (string, error) { return f.sign(f.appClaims(otherA)), nil }})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unlisted app: %v", err)
	}
	// disabled: refused
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "ci", SetDisabled: true, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	_, err = hubclient.Login(tctx(t), h.Addr("grpc"), hubclient.LoginOptions{
		Token: func(context.Context, *pb.AuthInfoResponse) (string, error) { return f.sign(f.appClaims(ciApp)), nil }})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("disabled app user: %v", err)
	}
}

// TestEntraSPASignIn: the credential-less mode - the browser redeems the
// code and posts the ID token; the hub checks it against the cookie.
func TestEntraSPASignIn(t *testing.T) {
	f := newFakeEntra(t)
	h := newEntraHub(t, f, func(e *EntraConfig) { e.Apps = []string{ciApp + "=ci"} })
	h.loginLimit = newIPLimiter(100, 100) // every request comes from 127.0.0.1
	if h.cfg.Auth.Entra.Web != EntraWebSPA {
		t.Fatalf("default web mode without a credential: %q", h.cfg.Auth.Entra.Web)
	}
	base := "http://" + h.Addr("http")
	if code, body := plainGet(t, base+"/auth/info"); code != 200 || !strings.Contains(body, `"mode": "spa"`) {
		t.Fatalf("/auth/info: %d %s", code, body)
	}
	if code, _ := plainGet(t, base+"/auth/callback?code=x&state=y"); code != http.StatusNotFound {
		t.Fatalf("confidential callback in spa mode: %d", code)
	}
	browser := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	do := func(c *http.Client, method, u string, hdr map[string]string, body string) (*http.Response, string) {
		req, _ := http.NewRequest(method, u, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	navHdr := map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}
	postHdr := map[string]string{"Origin": base, "Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"}
	// signIn plays the browser up to the ID token (what the callback page's
	// script does); it returns the token and the sign-in cookie.
	signIn := func(c *http.Client, oid, upn string, nonce func(string) string) (string, []*http.Cookie) {
		resp, _ := do(c, "GET", base+"/auth/login?next=/me", navHdr, "")
		loc, _ := url.Parse(resp.Header.Get("Location"))
		q := loc.Query()
		if q.Get("redirect_uri") != base+"/auth/spa-callback" || q.Get("code_challenge_method") != "S256" {
			t.Fatalf("authorize request: %v", q)
		}
		claims := f.accessClaims(oid, upn)
		delete(claims, "scp")
		claims["nonce"] = nonce(q.Get("nonce"))
		f.mu.Lock()
		f.codes["c-"+oid] = claims
		f.mu.Unlock()
		resp, page := do(c, "GET", base+"/auth/spa-callback?code=c-"+oid+"&state="+url.QueryEscape(q.Get("state")), navHdr, "")
		csp := resp.Header.Get("Content-Security-Policy")
		if resp.StatusCode != 200 || !strings.Contains(csp, "script-src 'nonce-") || !strings.Contains(csp, "connect-src 'self' "+f.srv.URL) {
			t.Fatalf("spa callback: %d %s\n%s", resp.StatusCode, csp, page)
		}
		_, js, _ := strings.Cut(page, "const cfg = ")
		js, _, _ = strings.Cut(js, ";\n")
		var cfg map[string]string
		if err := json.Unmarshal([]byte(js), &cfg); err != nil {
			t.Fatalf("page config %q: %v", js, err)
		}
		sum := sha256.Sum256([]byte(cfg["code_verifier"]))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") || cfg["redirect_uri"] != base+"/auth/spa-callback" {
			t.Fatalf("page config %v", cfg)
		}
		form := url.Values{"client_id": {cfg["client_id"]}, "grant_type": {"authorization_code"}, "code": {cfg["code"]},
			"redirect_uri": {cfg["redirect_uri"]}, "code_verifier": {cfg["code_verifier"]}, "scope": {cfg["scope"]}}
		tr, err := http.PostForm(cfg["token_url"], form)
		if err != nil {
			t.Fatal(err)
		}
		defer tr.Body.Close()
		var out struct {
			IDToken string `json:"id_token"`
		}
		json.NewDecoder(tr.Body).Decode(&out)
		if out.IDToken == "" {
			t.Fatalf("token endpoint: %s", tr.Status)
		}
		return out.IDToken, c.Jar.Cookies(mustURL(base + "/auth/spa-session"))
	}
	same := func(n string) string { return n }
	session := func(c *http.Client, tok string, hdr map[string]string) (int, string) {
		resp, body := do(c, "POST", base+"/auth/spa-session", hdr, `{"id_token":"`+tok+`"}`)
		return resp.StatusCode, body
	}

	c := browser()
	tok, ck := signIn(c, "eeeeeeee-0000-4000-8000-000000000001", "erin@example.com", same)
	// cross-origin posts are refused (and don't use up the state)
	if code, _ := session(c, tok, map[string]string{"Origin": "https://evil.example", "Content-Type": "application/json"}); code != http.StatusForbidden {
		t.Fatalf("cross-origin post: %d", code)
	}
	if code, _ := session(c, tok, map[string]string{"Content-Type": "application/json"}); code != http.StatusForbidden {
		t.Fatalf("post without Origin: %d", code)
	}
	if code, _ := session(c, tok, map[string]string{"Origin": base, "Sec-Fetch-Site": "cross-site", "Content-Type": "application/json"}); code != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", code)
	}
	if code, body := session(c, tok, postHdr); code != 200 || !strings.Contains(body, `"next": "/me"`) || !strings.Contains(body, `"user": "erin"`) {
		t.Fatalf("spa session: %d %s", code, body)
	}
	if resp, body := do(c, "GET", base+"/api/me", nil, ""); resp.StatusCode != 200 || !strings.Contains(body, `"tenant": "erin"`) {
		t.Fatalf("me: %d %s", resp.StatusCode, body)
	}
	// the same cookie again: used
	c2 := browser()
	c2.Jar.SetCookies(mustURL(base+"/auth/spa-session"), ck)
	if code, body := session(c2, tok, postHdr); code != http.StatusBadRequest || !strings.Contains(body, "already used") {
		t.Fatalf("replayed state: %d %s", code, body)
	}
	// an ID token of another sign-in (its nonce)
	c3 := browser()
	tok3, _ := signIn(c3, "eeeeeeee-0000-4000-8000-000000000001", "erin@example.com", func(string) string { return "other" })
	if code, body := session(c3, tok3, postHdr); code != http.StatusUnauthorized || !strings.Contains(body, "nonce") {
		t.Fatalf("wrong nonce: %d %s", code, body)
	}
	// an app-only token never opens a panel session
	c4 := browser()
	var n4 string
	signIn(c4, "eeeeeeee-0000-4000-8000-000000000002", "x@example.com", func(n string) string { n4 = n; return n })
	appTok := f.appClaims(ciApp)
	appTok["nonce"] = n4 // even with the right nonce
	if code, body := session(c4, f.sign(appTok), postHdr); code != http.StatusUnauthorized || !strings.Contains(body, "not accepted here") {
		t.Fatalf("app token for a panel session: %d %s", code, body)
	}
	// no sign-in cookie at all
	if code, _ := session(browser(), tok, postHdr); code != http.StatusBadRequest {
		t.Fatalf("post without the sign-in cookie: %d", code)
	}
	// a state that doesn't match the cookie never gets the page
	c5 := browser()
	do(c5, "GET", base+"/auth/login", navHdr, "")
	if resp, _ := do(c5, "GET", base+"/auth/spa-callback?code=x&state=forged", navHdr, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged state: %d", resp.StatusCode)
	}
}

// TestEntraStaleMapping: a removed user's name may go to someone else; the
// first person's next sign-in must not land on the new owner's account.
func TestEntraStaleMapping(t *testing.T) {
	f := newFakeEntra(t)
	h := newEntraHub(t, f, nil)
	const (
		oidA = "aaaaaaaa-0000-4000-8000-00000000000a"
		oidB = "bbbbbbbb-0000-4000-8000-00000000000b"
	)
	a := entraIdentity{oid: oidA, upn: "alice@a.example"}
	b := entraIdentity{oid: oidB, upn: "alice@b.example"}
	if u, _, err := h.entraSignIn(a, "web"); err != nil || u != "alice" {
		t.Fatalf("A: %q %v", u, err)
	}
	if err := h.entraPanelLogin("alice", oidA, false); err != nil {
		t.Fatal(err)
	}
	if _, err := h.RemoveUser("test", &pb.RemoveUserRequest{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	if u, _, err := h.entraSignIn(b, "web"); err != nil || u != "alice" {
		t.Fatalf("B after alice's removal: %q %v (the name is free: no devices, panel account removed)", u, err)
	}
	if err := h.entraPanelLogin("alice", oidB, false); err != nil {
		t.Fatalf("B's panel account: %v", err)
	}
	u, _, err := h.entraSignIn(a, "web")
	if err != nil || u == "alice" {
		t.Fatalf("A again: %q %v - must not become B's alice", u, err)
	}
	h.mu.Lock()
	upn := h.tenants["alice"].u.Upn
	h.mu.Unlock()
	if upn != "alice@b.example" {
		t.Fatalf("B's UPN overwritten: %q", upn)
	}
	if u2, _, err := h.entraSignIn(b, "web"); err != nil || u2 != "alice" {
		t.Fatalf("B again: %q %v", u2, err)
	}
	if u3, _, err := h.entraSignIn(a, "web"); err != nil || u3 != u {
		t.Fatalf("A's new user is stable: %q, want %q (%v)", u3, u, err)
	}

	// A removed user with devices: the name keeps that history.
	r, err := hubclient.Login(tctx(t), h.Addr("grpc"), hubclient.LoginOptions{
		JoinOptions: hubclient.JoinOptions{Name: "bobdev"},
		Token: func(context.Context, *pb.AuthInfoResponse) (string, error) {
			return f.sign(f.accessClaims("cccccccc-0000-4000-8000-00000000000c", "bob@c.example")), nil
		}})
	if err != nil || r.Resp.Name != "bobdev" {
		t.Fatalf("bob: %v %v", r, err)
	}
	if _, err := h.RemoveUser("test", &pb.RemoveUserRequest{Name: "bob"}); err != nil {
		t.Fatal(err)
	}
	if u, _, err := h.entraSignIn(entraIdentity{oid: "dddddddd-0000-4000-8000-00000000000d", upn: "bob@d.example"}, "web"); err != nil || u != "bob2" {
		t.Fatalf("a new bob: %q %v, want bob2", u, err)
	}
}
