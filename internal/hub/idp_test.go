package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
)

// fakeIdP is a personal-account provider stand-in (Google, Apple,
// Microsoft): JWKS, token endpoint, and a signer for ID tokens.
type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	codes map[string]map[string]any // code -> ID token claims
	forms []url.Values              // token requests
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	f := &fakeIdP{t: t, codes: map[string]map[string]any{}}
	key := rsaKey(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "use": "sig", "kid": "g1",
			"n": b64url.EncodeToString(key.N.Bytes()), "e": b64url.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		f.forms = append(f.forms, r.PostForm)
		claims, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_description": "secret stuff"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id_token": signJWT(t, map[string]any{"alg": "RS256", "kid": "g1"}, claims, key)})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// config points a provider's endpoints at f.
func (f *fakeIdP) config(clientID string) IdPConfig {
	secret := filepath.Join(f.t.TempDir(), "secret")
	os.WriteFile(secret, []byte("s3cret\n"), 0o600)
	return IdPConfig{ClientID: clientID, ClientSecretFile: secret, AuthURL: f.srv.URL + "/authorize",
		TokenURL: f.srv.URL + "/token", JWKSURL: f.srv.URL + "/jwks"}
}

func (f *fakeIdP) lastForm() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.forms[len(f.forms)-1]
}

const (
	googleClient = "1234-abc.apps.googleusercontent.com"
	msaClient    = "cccccccc-0000-4000-8000-00000000000c"
	appleClient  = "com.example.longarm.signin"
)

// appleKeyFile writes a "Sign in with Apple" key (.p8) and returns it.
func appleKeyFile(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	p := filepath.Join(t.TempDir(), "AuthKey_ABCDE12345.p8")
	os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	return p, k
}

func newIdPHub(t *testing.T, mut func(*AuthConfig)) *Hub {
	t.Helper()
	dir := t.TempDir()
	if _, err := InitCA(dir); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: dir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "127.0.0.1:0"}
	cfg.Web.InsecureCookie = true
	mut(&cfg.Auth)
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	h, err := Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	h.loginLimit, h.joinLimit = newIPLimiter(100, 100), newIPLimiter(100, 100) // every request comes from 127.0.0.1
	return h
}

func idClaimsFor(iss, aud, sub, nonce string, extra map[string]any) map[string]any {
	now := time.Now().Unix()
	c := map[string]any{"iss": iss, "aud": aud, "sub": sub, "nonce": nonce, "iat": now, "exp": now + 3600}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

// idpStart starts a sign-in with provider at w and returns the authorize
// request's parameters.
func (w *web) idpStart(provider, next string) url.Values {
	w.t.Helper()
	resp, _ := w.nav("/auth/login?provider=" + provider + "&next=" + url.QueryEscape(next))
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Path != "/authorize" {
		w.t.Fatalf("login with %s: %d %s", provider, resp.StatusCode, loc)
	}
	return loc.Query()
}

// idpCallback returns from the provider: a navigation (query) or, for
// Apple, a cross-site form post.
func (w *web) idpCallback(provider string, q url.Values, code string) (int, string) {
	w.t.Helper()
	if provider == idpApple {
		resp, body := w.do("POST", "/auth/callback/apple", strings.NewReader(url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "https://appleid.apple.com", "Sec-Fetch-Site": "cross-site"})
		return resp.StatusCode, body
	}
	resp, body := w.nav("/auth/callback/" + provider + "?code=" + code + "&state=" + url.QueryEscape(q.Get("state")))
	return resp.StatusCode, body
}

// googleSignIn signs w in with a Google account, back to next.
func (w *web) googleSignIn(f *fakeIdP, sub, email, next string) {
	w.t.Helper()
	q := w.idpStart(idpGoogle, next)
	f.mu.Lock()
	f.codes["c-"+sub] = idClaimsFor("https://accounts.google.com", googleClient, sub, q.Get("nonce"),
		map[string]any{"email": email, "email_verified": true})
	f.mu.Unlock()
	if code, body := w.idpCallback(idpGoogle, q, "c-"+sub); code != 200 || !strings.Contains(body, "url="+next) {
		w.t.Fatalf("google sign-in of %s: %d %s", email, code, body)
	}
}

func TestIdPConfig(t *testing.T) {
	keyFile, _ := appleKeyFile(t)
	secret := filepath.Join(t.TempDir(), "s")
	os.WriteFile(secret, []byte("x"), 0o600)
	for _, c := range []struct {
		name string
		cfg  AuthConfig
		ok   bool
	}{
		{"none", AuthConfig{}, true},
		{"google", AuthConfig{Google: IdPConfig{ClientID: googleClient, ClientSecretFile: secret, Admins: []string{"me@example.com", "*@example.com", "sub:1234"}}}, true},
		{"google without a secret", AuthConfig{Google: IdPConfig{ClientID: googleClient}}, false},
		{"microsoft not a GUID", AuthConfig{Microsoft: IdPConfig{ClientID: "longarm", ClientSecretFile: secret}}, false},
		{"microsoft", AuthConfig{Microsoft: IdPConfig{ClientID: strings.ToUpper(msaClient), ClientSecretFile: secret}}, true},
		{"apple", AuthConfig{Apple: IdPConfig{ClientID: appleClient, TeamID: "ABCDE12345", KeyID: "KEY1234567", PrivateKeyFile: keyFile}}, true},
		{"apple without a key", AuthConfig{Apple: IdPConfig{ClientID: appleClient, TeamID: "ABCDE12345", KeyID: "KEY1234567"}}, false},
		{"apple with a bad key", AuthConfig{Apple: IdPConfig{ClientID: appleClient, TeamID: "ABCDE12345", KeyID: "KEY1234567", PrivateKeyFile: secret}}, false},
		{"bad entry", AuthConfig{Google: IdPConfig{ClientID: googleClient, ClientSecretFile: secret, Allowed: []string{"not an address"}}}, false},
		{"plain http endpoint", AuthConfig{Google: IdPConfig{ClientID: googleClient, ClientSecretFile: secret, TokenURL: "http://idp.example/token"}}, false},
	} {
		err := c.cfg.applyDefaults()
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	for _, m := range []struct {
		entry, email, sub string
		want              bool
	}{
		{"me@example.com", "Me@Example.com", "", true}, {"*@example.com", "a@example.com", "", true}, {"*@example.com", "a@notexample.com", "", false},
		{"sub:AbC", "", "AbC", true}, {"sub:AbC", "", "abc", false}, {"me@example.com", "", "me@example.com", false},
	} {
		if got := idpMatch(m.entry, m.email, m.sub); got != m.want {
			t.Errorf("idpMatch(%q, %q, %q) = %v", m.entry, m.email, m.sub, got)
		}
	}
}

// TestIdPWebSignIn: Google, personal Microsoft accounts and Apple sign in
// to the panel: users created on first sign-in (named after the verified
// e-mail address), admins, the allowed list, a provider's state never
// accepted by another, work accounts refused at [auth.microsoft], Apple's
// form post and client secret.
func TestIdPWebSignIn(t *testing.T) {
	g, ms, ap := newFakeIdP(t), newFakeIdP(t), newFakeIdP(t)
	keyFile, appleKey := appleKeyFile(t)
	h := newIdPHub(t, func(a *AuthConfig) {
		a.Google = g.config(googleClient)
		a.Google.Admins = []string{"boss@example.com"}
		a.Google.Allowed = []string{"*@example.com"}
		a.Microsoft = ms.config(msaClient)
		a.Apple = ap.config(appleClient)
		a.Apple.ClientSecretFile, a.Apple.TeamID, a.Apple.KeyID, a.Apple.PrivateKeyFile = "", "ABCDE12345", "KEY1234567", keyFile
	})
	base := "http://" + h.Addr("http")
	if code, body := plainGet(t, base+"/auth/info"); code != 200 || !strings.Contains(body, `"entra": false`) ||
		!strings.Contains(body, `"id": "google"`) || !strings.Contains(body, `"label": "Apple"`) || !strings.Contains(body, `"id": "microsoft"`) {
		t.Fatalf("/auth/info: %d %s", code, body)
	}
	if _, body := plainGet(t, base+"/status"); !strings.Contains(body, `href="/auth/login?provider=google&amp;next=/status"`) {
		t.Fatalf("login page without the Google link: %s", body)
	}
	// Several providers: /auth/login without one is a chooser.
	w := newWeb(t, h)
	if resp, body := w.nav("/auth/login?next=/jobs"); resp.StatusCode != 200 || !strings.Contains(body, `/auth/login?provider=apple&amp;next=%2fjobs`) {
		t.Fatalf("chooser: %d %s", resp.StatusCode, body)
	}
	if resp, _ := w.nav("/auth/login?provider=entra"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("entra without [auth.entra]: %d", resp.StatusCode)
	}

	// Google: the authorize request, the token request, the user.
	q := w.idpStart(idpGoogle, "/jobs")
	if q.Get("client_id") != googleClient || q.Get("redirect_uri") != base+"/auth/callback/google" || q.Get("code_challenge_method") != "S256" ||
		q.Get("scope") != "openid email profile" || q.Get("nonce") == "" || q.Get("prompt") != "select_account" || q.Get("response_mode") != "" {
		t.Fatalf("google authorize: %v", q)
	}
	g.codes["g1"] = idClaimsFor("https://accounts.google.com", googleClient, "1001", q.Get("nonce"), map[string]any{"email": "Alice@example.com", "email_verified": true})
	if code, body := w.idpCallback(idpGoogle, q, "g1"); code != 200 || !strings.Contains(body, "Signed in as alice") || !strings.Contains(body, "url=/jobs") {
		t.Fatalf("google callback: %d %s", code, body)
	}
	form := g.lastForm()
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	if form.Get("client_secret") != "s3cret" || base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") ||
		form.Get("redirect_uri") != base+"/auth/callback/google" {
		t.Fatalf("google token request: %v", form)
	}
	if code, body := plainGetWith(t, w.c, base+"/api/me"); code != 200 || !strings.Contains(body, `"tenant": "alice"`) || !strings.Contains(body, `"role": "user"`) {
		t.Fatalf("me: %d %s", code, body)
	}
	h.mu.Lock()
	alice := h.tenants["alice"]
	h.mu.Unlock()
	if alice == nil || alice.u.Upn != "alice@example.com" || alice.u.CreatedBy != "google" {
		t.Fatalf("alice: %+v", alice)
	}
	// The same account again: the same user, whatever its address is now.
	w2 := newWeb(t, h)
	q = w2.idpStart(idpGoogle, "/")
	g.codes["g2"] = idClaimsFor("accounts.google.com", googleClient, "1001", q.Get("nonce"), map[string]any{"email": "alice.new@example.com", "email_verified": true})
	if code, body := w2.idpCallback(idpGoogle, q, "g2"); code != 200 || !strings.Contains(body, "Signed in as alice.") {
		t.Fatalf("google again: %d %s", code, body)
	}
	// Refused: an address outside allowed, an unverified one (no address: not allowed either), a wrong nonce.
	for i, c := range []map[string]any{
		{"email": "eve@evil.example.net", "email_verified": true},
		{"email": "eve@example.com", "email_verified": false},
	} {
		w := newWeb(t, h)
		q := w.idpStart(idpGoogle, "/")
		g.codes["bad"] = idClaimsFor("https://accounts.google.com", googleClient, "2000", q.Get("nonce"), c)
		if code, body := w.idpCallback(idpGoogle, q, "bad"); code != http.StatusForbidden || !strings.Contains(body, "may not sign in") {
			t.Fatalf("refused %d: %d %s", i, code, body)
		}
	}
	w3 := newWeb(t, h)
	q = w3.idpStart(idpGoogle, "/")
	g.codes["g3"] = idClaimsFor("https://accounts.google.com", googleClient, "1001", "other", map[string]any{"email": "alice@example.com", "email_verified": true})
	if code, body := w3.idpCallback(idpGoogle, q, "g3"); code != http.StatusUnauthorized || !strings.Contains(body, "nonce") {
		t.Fatalf("wrong nonce: %d %s", code, body)
	}
	// An admin.
	w4 := newWeb(t, h)
	w4.googleSignIn(g, "1002", "boss@example.com", "/")
	if code, body := plainGetWith(t, w4.c, base+"/api/me"); code != 200 || !strings.Contains(body, `"role": "admin"`) {
		t.Fatalf("admin me: %d %s", code, body)
	}
	// Token endpoint errors: the code only, never the description.
	w5 := newWeb(t, h)
	q = w5.idpStart(idpGoogle, "/")
	if code, body := w5.idpCallback(idpGoogle, q, "unknown"); code != http.StatusBadGateway || !strings.Contains(body, "invalid_grant") || strings.Contains(body, "secret stuff") {
		t.Fatalf("token error: %d %s", code, body)
	}

	// A Google sign-in's state at Microsoft's callback: refused.
	w6 := newWeb(t, h)
	q = w6.idpStart(idpGoogle, "/")
	ms.codes["m0"] = idClaimsFor("https://login.microsoftonline.com/"+msaTenant+"/v2.0", msaClient, "m-sub", q.Get("nonce"), map[string]any{"tid": msaTenant})
	if code, _ := w6.idpCallback(idpMicrosoft, q, "m0"); code != http.StatusBadRequest {
		t.Fatalf("state of another provider: %d", code)
	}

	// Microsoft: personal accounts only.
	msIss := "https://login.microsoftonline.com/" + msaTenant + "/v2.0"
	w7 := newWeb(t, h)
	q = w7.idpStart(idpMicrosoft, "/")
	if q.Get("client_id") != msaClient || q.Get("redirect_uri") != base+"/auth/callback/microsoft" || q.Get("code_challenge") == "" {
		t.Fatalf("microsoft authorize: %v", q)
	}
	ms.codes["m1"] = idClaimsFor(msIss, msaClient, "AAAAmsaSub", q.Get("nonce"), map[string]any{"tid": testTenant, "preferred_username": "worker@contoso.example"})
	if code, body := w7.idpCallback(idpMicrosoft, q, "m1"); code != http.StatusUnauthorized || !strings.Contains(body, "not a personal Microsoft account") {
		t.Fatalf("work account at [auth.microsoft]: %d %s", code, body)
	}
	w8 := newWeb(t, h)
	q = w8.idpStart(idpMicrosoft, "/")
	ms.codes["m2"] = idClaimsFor(msIss, msaClient, "AAAAmsaSub", q.Get("nonce"), map[string]any{"tid": msaTenant, "preferred_username": "Carol@Example.org"})
	if code, body := w8.idpCallback(idpMicrosoft, q, "m2"); code != 200 || !strings.Contains(body, "Signed in as carol") {
		t.Fatalf("microsoft account: %d %s", code, body)
	}
	if f := ms.lastForm(); f.Get("client_secret") != "s3cret" || f.Get("code_verifier") == "" {
		t.Fatalf("microsoft token request: %v", f)
	}
	// A subject differing only in case is another account.
	w9 := newWeb(t, h)
	q = w9.idpStart(idpMicrosoft, "/")
	ms.codes["m3"] = idClaimsFor(msIss, msaClient, "aaaaMSASUB", q.Get("nonce"), map[string]any{"tid": msaTenant, "preferred_username": "carol@example.org"})
	if code, body := w9.idpCallback(idpMicrosoft, q, "m3"); code != 200 || !strings.Contains(body, "Signed in as carol2") {
		t.Fatalf("second microsoft account: %d %s", code, body)
	}

	// Apple: form_post, a state cookie that survives the cross-site post, an ES256 client secret, no PKCE.
	wa := newWeb(t, h)
	q = wa.idpStart(idpApple, "/me")
	if q.Get("response_mode") != "form_post" || q.Get("scope") != "name email" || q.Get("code_challenge") != "" || q.Get("redirect_uri") != base+"/auth/callback/apple" {
		t.Fatalf("apple authorize: %v", q)
	}
	if resp, _ := wa.nav("/auth/callback/apple?code=x&state=" + q.Get("state")); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("apple callback by GET: %d", resp.StatusCode)
	}
	ap.codes["a1"] = idClaimsFor("https://appleid.apple.com", appleClient, "000123.abcdef.0456", q.Get("nonce"),
		map[string]any{"email": "x7f2k@privaterelay.example", "email_verified": "true", "is_private_email": "true"})
	if code, body := wa.idpCallback(idpApple, q, "a1"); code != 200 || !strings.Contains(body, "Signed in as x7f2k") || !strings.Contains(body, "url=/me") {
		t.Fatalf("apple callback: %d %s", code, body)
	}
	af := ap.lastForm()
	if af.Get("code_verifier") != "" || af.Get("client_id") != appleClient || !ecdsaVerifyJWT(&appleKey.PublicKey, af.Get("client_secret")) {
		t.Fatalf("apple token request: %v", af)
	}
	parts := strings.Split(af.Get("client_secret"), ".")
	var hdr, cl map[string]any
	b, _ := b64url.DecodeString(parts[0])
	json.Unmarshal(b, &hdr)
	b, _ = b64url.DecodeString(parts[1])
	json.Unmarshal(b, &cl)
	if hdr["alg"] != "ES256" || hdr["kid"] != "KEY1234567" || cl["iss"] != "ABCDE12345" || cl["sub"] != appleClient || cl["aud"] != "https://appleid.apple.com" {
		t.Fatalf("apple client secret: %v %v", hdr, cl)
	}
	// An Apple account without an e-mail address (not shared): named after the provider.
	wb := newWeb(t, h)
	q = wb.idpStart(idpApple, "/")
	ap.codes["a2"] = idClaimsFor("https://appleid.apple.com", appleClient, "000999.ffff.0001", q.Get("nonce"), nil)
	if code, body := wb.idpCallback(idpApple, q, "a2"); code != 200 || !strings.Contains(body, "Signed in as apple") {
		t.Fatalf("apple without e-mail: %d %s", code, body)
	}
}

func ecdsaVerifyJWT(pub *ecdsa.PublicKey, jwt string) bool {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := b64url.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}

// TestIdPLink: the clients and workers one account links are one user's:
// a Google user approves a machine as client + worker, then a second
// machine as a worker - all alice's; another account's are its own; a
// password login can't approve on a hub with identity providers.
func TestIdPLink(t *testing.T) {
	g := newFakeIdP(t)
	h := newIdPHub(t, func(a *AuthConfig) { a.Google = g.config(googleClient) })
	ec := enrollClient(t, h)
	nk := ed25519.NewKeyFromSeed(make([]byte, 32))
	seed := make([]byte, 32)
	seed[0] = 1
	wk := ed25519.NewKeyFromSeed(seed)

	// /link without a session: off to the (only) provider.
	alice := newWeb(t, h)
	if resp, _ := alice.nav("/link"); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/auth/login?next=/link" {
		t.Fatalf("/link without a session: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	alice.googleSignIn(g, "1001", "alice@example.com", "/link")

	d, wkeyPEM, err := startBoth(t, ec, "", "", nk, wk)
	if err != nil {
		t.Fatal(err)
	}
	code, confirm := alice.lookup(d.st.UserCode)
	if code != 200 || !strings.Contains(confirm, "alice@example.com") {
		t.Fatalf("confirmation: %d %s", code, confirm)
	}
	if c, body := alice.decide(confirm, "approve"); c != 200 || !strings.Contains(body, "signed in as alice (client), and worker dual is yours") {
		t.Fatalf("approve: %d %s", c, body)
	}
	r := poll(t, h, ec, d)
	id, _ := identityOf(t, r)
	if r.State != linkApproved || id.Role != pki.RoleClient || id.User != "alice" || r.Worker == nil || r.Worker.Owner != "alice" {
		t.Fatalf("both: %v %v", r, id)
	}
	if wa := connectWorker(t, h, r.Worker, wkeyPEM); wa.Owner != "alice" {
		t.Fatalf("worker access: %v", wa)
	}

	// A second machine, a worker only, signed in again later in another browser: alice's.
	d2, err := startLink(t, ec, linkWorker, "box2", nil)
	if err != nil {
		t.Fatal(err)
	}
	again := newWeb(t, h)
	again.googleSignIn(g, "1001", "alice@example.com", "/link")
	_, confirm = again.lookup(d2.st.UserCode)
	if c, body := again.decide(confirm, "approve"); c != 200 {
		t.Fatalf("approve worker: %d %s", c, body)
	}
	if r := poll(t, h, ec, d2); r.State != linkApproved || r.Owner != "alice" || r.Role != string(pki.RoleWorker) {
		t.Fatalf("second worker: %v", r)
	}

	// Another Google account: another user, its own device.
	bob := newWeb(t, h)
	bob.googleSignIn(g, "2002", "bob@example.com", "/link")
	d3, err := startLink(t, ec, linkClient, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, confirm = bob.lookup(d3.st.UserCode)
	if c, body := bob.decide(confirm, "approve"); c != 200 {
		t.Fatalf("bob approves: %d %s", c, body)
	}
	if r := poll(t, h, ec, d3); r.State != linkApproved {
		t.Fatalf("bob's device: %v", r)
	} else if id, _ := identityOf(t, r); id.User != "bob" || id.Role != pki.RoleClient {
		t.Fatalf("bob's device identity: %v", id)
	}
	h.mu.Lock()
	var owners []string
	for _, n := range h.nodes {
		if n.rec.Role == string(pki.RoleWorker) {
			owners = append(owners, n.rec.Name+"="+n.rec.Owner)
		}
	}
	h.mu.Unlock()
	if len(owners) != 2 || !strings.Contains(strings.Join(owners, ","), "dual=alice") || !strings.Contains(strings.Join(owners, ","), "box2=alice") {
		t.Fatalf("workers: %v", owners)
	}

	// A password login can't approve here.
	pw := newWeb(t, h)
	pw.passwordSignIn(h, "ops", "admin", "")
	if resp, _ := pw.nav("/link"); resp.StatusCode != http.StatusFound {
		t.Fatalf("password session at /link: %d", resp.StatusCode)
	}
}
