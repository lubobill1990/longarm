package hub

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

const (
	testTenant = "11111111-2222-3333-4444-555555555555"
	testClient = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// fakeEntra is a Microsoft Entra stand-in: JWKS, token endpoint, and a
// signer for tokens.
type fakeEntra struct {
	t        *testing.T
	key      *rsa.PrivateKey
	kid      string
	srv      *httptest.Server
	jwksHits atomic.Int32

	mu      sync.Mutex
	keys    map[string]*rsa.PrivateKey // published
	codes   map[string]map[string]any  // code -> ID token claims
	forms   []url.Values               // token requests
	tokenFn func(url.Values) (int, map[string]any)
}

func newFakeEntra(t *testing.T) *fakeEntra {
	t.Helper()
	f := &fakeEntra{t: t, key: rsaKey(t), kid: "k1", codes: map[string]map[string]any{}}
	f.keys = map[string]*rsa.PrivateKey{f.kid: f.key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /"+testTenant+"/discovery/v2.0/keys", func(w http.ResponseWriter, _ *http.Request) {
		f.jwksHits.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		var keys []map[string]string
		for kid, k := range f.keys {
			keys = append(keys, map[string]string{"kty": "RSA", "use": "sig", "kid": kid,
				"n": b64url.EncodeToString(k.N.Bytes()), "e": b64url.EncodeToString(big.NewInt(int64(k.E)).Bytes())})
		}
		keys = append(keys, map[string]string{"kty": "EC", "kid": "ec1"}) // ignored
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	mux.HandleFunc("POST /"+testTenant+"/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		f.forms = append(f.forms, r.PostForm)
		fn := f.tokenFn
		claims, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fn != nil {
			code, body := fn(r.PostForm)
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(body)
			return
		}
		if !ok {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_codes": []int{70008}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id_token": f.sign(claims), "token_type": "Bearer"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

var (
	rsaOnce sync.Once
	rsaKeys []*rsa.PrivateKey
	rsaNext atomic.Int32
)

// rsaKey hands out pre-generated 2048-bit keys (generating is slow).
func rsaKey(t *testing.T) *rsa.PrivateKey {
	rsaOnce.Do(func() {
		for range 4 {
			k, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				panic(err)
			}
			rsaKeys = append(rsaKeys, k)
		}
	})
	return rsaKeys[int(rsaNext.Add(1))%len(rsaKeys)]
}

func (f *fakeEntra) issuer() string { return f.srv.URL + "/" + testTenant + "/v2.0" }

// accessClaims are the claims of an az access token for the hub's app.
func (f *fakeEntra) accessClaims(oid, upn string) map[string]any {
	now := time.Now().Unix()
	return map[string]any{"aud": testClient, "iss": f.issuer(), "tid": testTenant, "oid": oid, "scp": "longarm.connect",
		"preferred_username": upn, "name": "Test User", "ver": "2.0", "azp": "04b07795-8ddb-461a-bbee-02f9e1bf7b46",
		"iat": now, "nbf": now, "exp": now + 3600}
}

func (f *fakeEntra) sign(claims map[string]any) string {
	return signJWT(f.t, map[string]any{"alg": "RS256", "typ": "JWT", "kid": f.kid}, claims, f.key)
}

func signJWT(t *testing.T, hdr, claims map[string]any, key *rsa.PrivateKey) string {
	t.Helper()
	h, _ := json.Marshal(hdr)
	c, _ := json.Marshal(claims)
	in := b64url.EncodeToString(h) + "." + b64url.EncodeToString(c)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + b64url.EncodeToString(sig)
}

func (f *fakeEntra) config() EntraConfig {
	return EntraConfig{TenantID: testTenant, ClientID: testClient, Authority: f.srv.URL}
}

func TestEntraVerifyToken(t *testing.T) {
	f := newFakeEntra(t)
	keys := newJWKS(f.srv.URL + "/" + testTenant + "/discovery/v2.0/keys")
	access := tokenCheck{issuer: f.issuer(), tenant: testTenant, audiences: []string{testClient, "api://" + testClient}, scope: "longarm.connect"}
	verify := func(tok string) error {
		_, err := verifyEntraToken(context.Background(), keys, tok, access, time.Now())
		return err
	}
	ok := f.accessClaims("0bd3c9f1-0000-4000-8000-00000000000a", "alice@example.com")
	with := func(k string, v any) map[string]any {
		c := map[string]any{}
		for kk, vv := range ok {
			c[kk] = vv
		}
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
		return c
	}
	if err := verify(f.sign(ok)); err != nil {
		t.Fatalf("good token: %v", err)
	}
	for _, c := range []map[string]any{with("aud", "api://"+testClient), with("aud", strings.ToUpper(testClient)),
		with("scp", "User.Read longarm.connect"), with("nbf", time.Now().Unix()+60)} {
		if err := verify(f.sign(c)); err != nil {
			t.Errorf("token %v: %v", c, err)
		}
	}
	hs := func() string { // HS256 "signed" with the public key, the classic confusion
		h, _ := json.Marshal(map[string]any{"alg": "HS256", "kid": f.kid})
		c, _ := json.Marshal(ok)
		in := b64url.EncodeToString(h) + "." + b64url.EncodeToString(c)
		m := hmac.New(sha256.New, f.key.PublicKey.N.Bytes())
		m.Write([]byte(in))
		return in + "." + b64url.EncodeToString(m.Sum(nil))
	}
	unsigned := func(alg string) string {
		h, _ := json.Marshal(map[string]any{"alg": alg, "kid": f.kid})
		c, _ := json.Marshal(ok)
		return b64url.EncodeToString(h) + "." + b64url.EncodeToString(c) + "."
	}
	tampered := func() string {
		p := strings.Split(f.sign(ok), ".")
		c, _ := json.Marshal(with("oid", "0bd3c9f1-0000-4000-8000-00000000000b"))
		return p[0] + "." + b64url.EncodeToString(c) + "." + p[2]
	}
	otherKey := signJWT(t, map[string]any{"alg": "RS256", "kid": f.kid}, ok, rsaKey(t))
	for name, c := range map[string]struct {
		tok  string
		want string
	}{
		"wrong issuer":    {f.sign(with("iss", "https://login.microsoftonline.com/"+testTenant+"/v2.0")), "issuer"},
		"v1 issuer":       {f.sign(with("iss", "https://sts.windows.net/"+testTenant+"/")), "issuer"},
		"wrong audience":  {f.sign(with("aud", "00000003-0000-0000-c000-000000000000")), "audience"},
		"wrong tenant":    {f.sign(with("tid", "99999999-2222-3333-4444-555555555555")), "tenant"},
		"no scope":        {f.sign(with("scp", nil)), "scope"},
		"other scope":     {f.sign(with("scp", "User.Read longarm.connectx")), "scope"},
		"expired":         {f.sign(with("exp", time.Now().Add(-10*time.Minute).Unix())), "expired"},
		"no expiry":       {f.sign(with("exp", nil)), "expiry"},
		"not yet":         {f.sign(with("nbf", time.Now().Add(10*time.Minute).Unix())), "not valid yet"},
		"no oid":          {f.sign(with("oid", nil)), "oid"},
		"alg none":        {unsigned("none"), "algorithm"},
		"alg RS256 nosig": {unsigned("RS256"), "signature"},
		"HS256":           {hs(), "algorithm"},
		"other key":       {otherKey, "signature"},
		"tampered":        {tampered(), "signature"},
		"garbage":         {"not.a.jwt.at.all", "JWT"},
		"unknown kid":     {signJWT(t, map[string]any{"alg": "RS256", "kid": "nope"}, ok, f.key), "unknown signing key"},
	} {
		if err := verify(c.tok); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an error about %q", name, err, c.want)
		}
	}
	// ID tokens: the nonce must match; the API audience is not an ID token's.
	idc := tokenCheck{issuer: f.issuer(), tenant: testTenant, audiences: []string{testClient}, nonce: "n1"}
	if _, err := verifyEntraToken(context.Background(), keys, f.sign(with("nonce", "n1")), idc, time.Now()); err != nil {
		t.Errorf("ID token: %v", err)
	}
	if _, err := verifyEntraToken(context.Background(), keys, f.sign(with("nonce", "n2")), idc, time.Now()); err == nil {
		t.Error("wrong nonce accepted")
	}
	if _, err := verifyEntraToken(context.Background(), keys, f.sign(with("aud", "api://"+testClient)), idc, time.Now()); err == nil {
		t.Error("ID token with the API audience accepted")
	}
}

// TestEntraJWKSCache: keys are fetched once, again for an unknown kid (key
// rotation) - but not more than once a minute.
func TestEntraJWKSCache(t *testing.T) {
	f := newFakeEntra(t)
	keys := newJWKS(f.srv.URL + "/" + testTenant + "/discovery/v2.0/keys")
	ctx := context.Background()
	for range 5 {
		if _, err := keys.key(ctx, "k1"); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.jwksHits.Load(); n != 1 {
		t.Fatalf("%d fetches, want 1", n)
	}
	for range 5 {
		if _, err := keys.key(ctx, "random"); err == nil {
			t.Fatal("unknown kid")
		}
	}
	if n := f.jwksHits.Load(); n != 1 {
		t.Fatalf("%d fetches: unknown kids refetch at most once a minute", n)
	}
	// rotation: a new key appears; after the minute it is found
	f.mu.Lock()
	f.keys["k2"] = rsaKey(t)
	f.mu.Unlock()
	keys.mu.Lock()
	keys.triedAt = time.Now().Add(-2 * jwksMinRefresh)
	keys.mu.Unlock()
	if _, err := keys.key(ctx, "k2"); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	if n := f.jwksHits.Load(); n != 2 {
		t.Fatalf("%d fetches, want 2", n)
	}
}

func TestEntraEntries(t *testing.T) {
	oid := "0BD3C9F1-0000-4000-8000-00000000000A"
	for _, c := range []struct {
		entry, upn, oid string
		want            bool
	}{
		{"alice@example.com", "Alice@Example.com", "", true},
		{"ALICE@example.com", "alice@example.com", "", true},
		{"alice@example.com", "alice@example.co", "", false},
		{"*@example.com", "bob@EXAMPLE.com", "", true},
		{"*@example.com", "bob@sub.example.com", "", false},
		{"*@example.com", "bob@notexample.com", "", false},
		{"*@example.com", "", "", false},
		{strings.ToLower(oid), "x@y.z", oid, true},
		{oid, "x@y.z", strings.ToLower(oid), true},
		{oid, "x@y.z", "", false},
		{"", "", "", false},
	} {
		if got := entraMatch(c.entry, c.upn, c.oid); got != c.want {
			t.Errorf("entraMatch(%q, %q, %q) = %v", c.entry, c.upn, c.oid, got)
		}
	}
	for e, want := range map[string]bool{"a@b.com": true, "*@b.com": true, oid: true, "*@b": false, "a b@c.com": false,
		"bob": false, "*": false, "a@b@c.com": false, "": false} {
		if validEntraEntry(e) != want {
			t.Errorf("validEntraEntry(%q) != %v", e, want)
		}
	}
	for upn, want := range map[string]string{"bolu1@microsoft.com": "bolu1", "First.Last@x.com": "firstlast",
		"1abc@x.com": "u1abc", "averyveryverylongusername@x.com": "averyveryverylong", "@x.com": "user", "": "user",
		"Zoë_O'Neil-2@x.com": "zo_oneil-2"} {
		if got := entraUserName(upn); got != want || !pki.ValidUser(got) {
			t.Errorf("entraUserName(%q) = %q, want %q", upn, got, want)
		}
	}
}

func TestEntraConfig(t *testing.T) {
	good := func() *EntraConfig { return &EntraConfig{TenantID: strings.ToUpper(testTenant), ClientID: testClient} }
	c := good()
	if err := c.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	if c.TenantID != testTenant || c.Scope != "longarm.connect" || c.Authority != "https://login.microsoftonline.com" ||
		c.FullScope() != "api://"+testClient+"/longarm.connect" || c.issuer() != "https://login.microsoftonline.com/"+testTenant+"/v2.0" {
		t.Fatalf("defaults: %+v", c)
	}
	for name, mut := range map[string]func(*EntraConfig){
		"no client":        func(c *EntraConfig) { c.ClientID = "" },
		"domain tenant":    func(c *EntraConfig) { c.TenantID = "contoso.onmicrosoft.com" },
		"full scope":       func(c *EntraConfig) { c.Scope = "api://x/longarm.connect" },
		"bad admin":        func(c *EntraConfig) { c.Admins = []string{"bob"} },
		"bad allowed":      func(c *EntraConfig) { c.Allowed = []string{"*"} },
		"bad assertion":    func(c *EntraConfig) { c.ClientAssertion = "secret" },
		"both credentials": func(c *EntraConfig) { c.ClientAssertion, c.ClientSecretFile = "managed-identity", "/x" },
		"http authority":   func(c *EntraConfig) { c.Authority = "http://login.example.com" },
		"authority path":   func(c *EntraConfig) { c.Authority = "https://login.example.com/x" },
		"bad web":          func(c *EntraConfig) { c.Web = "public" },
		"bad app id":       func(c *EntraConfig) { c.Apps = []string{"ci=ci"} },
		"bad app user":     func(c *EntraConfig) { c.Apps = []string{testClient + "=Not A User"} },
	} {
		c := good()
		mut(c)
		if err := c.applyDefaults(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if c := good(); c.applyDefaults() != nil || c.Web != EntraWebSPA {
		t.Errorf("default web mode without a credential: %q", c.Web)
	}
	if c := good(); func() error { c.ClientAssertion = "managed-identity"; return c.applyDefaults() }() != nil || c.Web != EntraWebConfidential {
		t.Errorf("default web mode with a credential: %q", c.Web)
	}
	if c := (&EntraConfig{}); c.applyDefaults() != nil || c.Enabled() || c.FullScope() != "" {
		t.Error("empty config")
	}
	// env overrides (containers)
	t.Setenv(EnvEntraTenant, testTenant)
	t.Setenv(EnvEntraClient, testClient)
	t.Setenv(EnvEntraAdmins, "*@example.com, bob@example.org")
	t.Setenv(EnvEntraClientAssertion, "managed-identity")
	t.Setenv(EnvEntraWeb, "off")
	t.Setenv(EnvEntraApps, "c1c1c1c1-0000-4000-8000-000000000001=ci")
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Auth.Entra
	if !e.Enabled() || len(e.Admins) != 2 || e.Admins[1] != "bob@example.org" || e.ClientAssertion != "managed-identity" ||
		e.Web != EntraWebOff || e.apps["c1c1c1c1-0000-4000-8000-000000000001"] != "ci" {
		t.Fatalf("env: %+v", e)
	}
}

// newEntraHub runs a hub with Entra sign-in against f.
func newEntraHub(t *testing.T, f *fakeEntra, mut func(*EntraConfig)) *Hub {
	t.Helper()
	dir := t.TempDir()
	if _, err := InitCA(dir); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: dir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "127.0.0.1:0"}
	cfg.Web.InsecureCookie = true
	cfg.Auth.Entra = f.config()
	if mut != nil {
		mut(&cfg.Auth.Entra)
	}
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
	return h
}

// TestEntraJoin: `longarm login`'s RPC end to end - users created on first
// sign-in, name collisions, admins, the allowed list, disabled users, and
// signing in again on the same device.
func TestEntraJoin(t *testing.T) {
	f := newFakeEntra(t)
	h := newEntraHub(t, f, func(e *EntraConfig) {
		e.Admins = []string{"*@admins.example"}
		e.Allowed = []string{"*@example.com", "carol@other.example"}
	})
	fp := pki.Fingerprint(h.ca.Cert)
	login := func(tok, name string, tr *pki.CSRTrust) (*hubclient.JoinResult, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return hubclient.Login(ctx, h.Addr("grpc"), hubclient.LoginOptions{
			JoinOptions: hubclient.JoinOptions{Name: name, Fingerprint: fp, Trust: tr},
			Token: func(_ context.Context, info *pb.AuthInfoResponse) (string, error) {
				if info.EntraTenantId != testTenant || info.EntraClientId != testClient || info.EntraScope != "api://"+testClient+"/longarm.connect" {
					t.Errorf("auth info %v", info)
				}
				return tok, nil
			},
		})
	}
	const (
		aliceOID = "a11ce000-0000-4000-8000-000000000001"
		alice2   = "a11ce000-0000-4000-8000-000000000002"
		adminOID = "ad000000-0000-4000-8000-000000000003"
	)
	nk := ed25519.NewKeyFromSeed(make([]byte, 32))
	r, err := login(f.sign(f.accessClaims(aliceOID, "Alice@example.com")), "", &pki.CSRTrust{NodeKey: nk})
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := pki.ParseCertPEM(r.Resp.CertPem)
	id, _ := pki.IdentityFromCert(cert)
	if r.Resp.State != stateApproved || id.Role != pki.RoleClient || id.User != "alice" || !strings.HasPrefix(id.Name, "alice-") {
		t.Fatalf("first sign-in: %v %v", r.Resp, id)
	}
	if te, err := pki.TrustOf(cert); err != nil || te == nil {
		t.Fatalf("the CSR's node key binding was not carried over: %v %v", te, err)
	}
	h.mu.Lock()
	u := h.tenants["alice"]
	h.mu.Unlock()
	if u == nil || u.u.Upn != "Alice@example.com" || u.u.CreatedBy != "entra" {
		t.Fatalf("user alice: %v", u)
	}
	// The certificate works, as user alice.
	b := writeTestBundle(t, h, r)
	conn, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	who, err := pb.NewFleetClient(conn).Whoami(tctx(t), &pb.WhoamiRequest{})
	if err != nil || who.User != "alice" || who.Upn != "Alice@example.com" {
		t.Fatalf("whoami: %v %v", who, err)
	}

	// Again on the same device: same name, the old certificate is revoked.
	r2, err := login(f.sign(f.accessClaims(aliceOID, "alice@example.com")), "", nil)
	if err != nil || r2.Resp.Name != id.Name {
		t.Fatalf("second sign-in: %v %v", r2, err)
	}
	if _, err := pb.NewFleetClient(conn).Whoami(tctx(t), &pb.WhoamiRequest{}); err == nil { // refused in the handshake
		t.Errorf("the replaced certificate still works: %v", err)
	}

	// Another person whose UPN starts the same: alice2.
	r3, err := login(f.sign(f.accessClaims(alice2, "alice@other.example.com")), "", nil)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("outside allowed: %v %v", r3, err)
	}
	h.cfg.Auth.Entra.Allowed = append(h.cfg.Auth.Entra.Allowed, "*@other.example.com")
	r3, err = login(f.sign(f.accessClaims(alice2, "alice@other.example.com")), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := pki.ParseCertPEM(r3.Resp.CertPem); c != nil {
		if id, _ := pki.IdentityFromCert(c); id.User != "alice2" {
			t.Fatalf("collision: user %q", id.User)
		}
	}
	// A device name of another user is not taken over.
	if _, err := login(f.sign(f.accessClaims(alice2, "alice@other.example.com")), id.Name, nil); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("other user's device name: %v", err)
	}

	// Admins get admin certificates.
	ra, err := login(f.sign(f.accessClaims(adminOID, "root@admins.example")), "rootdev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ra.Resp.Role != string(pki.RoleAdmin) {
		t.Fatalf("admin: %v", ra.Resp)
	}
	c, _ := pki.ParseCertPEM(ra.Resp.CertPem)
	if aid, _ := pki.IdentityFromCert(c); aid.Role != pki.RoleAdmin || aid.User != "root" {
		t.Fatalf("admin identity %v", aid)
	}

	// Disabled users are refused.
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "alice", SetDisabled: true, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := login(f.sign(f.accessClaims(aliceOID, "alice@example.com")), "", nil); status.Code(err) != codes.PermissionDenied ||
		!strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled user: %v", err)
	}

	// Bad tokens.
	bad := f.accessClaims(aliceOID, "alice@example.com")
	bad["scp"] = "User.Read"
	if _, err := login(f.sign(bad), "", nil); status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("token without the scope: %v", err)
	}
	if _, err := login("garbage", "", nil); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("garbage token: %v", err)
	}
	// Audited.
	res, err := h.Audit(&pb.AuditRequest{Node: "hub", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range res.Records {
		if a.Method == "entra/login" {
			n++
		}
	}
	if n != 4 {
		t.Errorf("%d entra/login audit records, want 4", n)
	}
}

func writeTestBundle(t *testing.T, h *Hub, r *hubclient.JoinResult) *pki.Bundle {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "tls")
	if err := pki.WriteBundle(dir, h.ca.CertPEM, r.Resp.CertPem, r.KeyPEM); err != nil {
		t.Fatal(err)
	}
	b, err := pki.LoadBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestEntraNoConfig: a hub without [auth.entra] says so.
func TestEntraNoConfig(t *testing.T) {
	th := newTenancyHub(t)
	_, err := hubclient.Login(tctx(t), th.h.Addr("grpc"), hubclient.LoginOptions{
		Token: func(context.Context, *pb.AuthInfoResponse) (string, error) {
			t.Fatal("token asked for")
			return "", nil
		}})
	if err != hubclient.ErrNoEntra {
		t.Fatalf("login: %v", err)
	}
	resp, err := http.Get("http://" + th.h.Addr("http") + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/auth/login: %d", resp.StatusCode)
	}
	if _, body := plainGet(t, "http://"+th.h.Addr("http")+"/auth/info"); !strings.Contains(body, `"entra": false`) {
		t.Fatalf("/auth/info without Entra: %s", body)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{"": "/", "/jobs": "/jobs", "/jobs?x=1#y": "/jobs?x=1#y", "//evil.com": "/",
		"/\\evil.com": "/", "https://evil.com/": "/", "jobs": "/", "/%2F%2Fevil": "/", "/a\nb": "/",
		"/" + strings.Repeat("a", 2000): "/"} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEntraWebSignIn: the panel's OpenID Connect sign-in against a fake
// Entra, with the managed identity's token as the client assertion.
func TestEntraWebSignIn(t *testing.T) {
	f := newFakeEntra(t)
	var miCalls atomic.Int32
	mi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.Header.Get("X-IDENTITY-HEADER") != "id-secret" || q.Get("resource") != "api://AzureADTokenExchange" || q.Get("client_id") != "mi-client" {
			http.Error(w, "bad request", 400)
			return
		}
		miCalls.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"access_token": "mi-assertion", "expires_on": strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)})
	}))
	defer mi.Close()
	t.Setenv("IDENTITY_ENDPOINT", mi.URL)
	t.Setenv("IDENTITY_HEADER", "id-secret")
	t.Setenv("AZURE_CLIENT_ID", "mi-client")
	h := newEntraHub(t, f, func(e *EntraConfig) {
		e.Admins = []string{"boss@example.com"}
		e.ClientAssertion = "managed-identity"
	})
	h.loginLimit = newIPLimiter(100, 100) // every request comes from 127.0.0.1
	base := "http://" + h.Addr("http")

	// /auth/info and the server-rendered login page
	if code, body := plainGet(t, base+"/auth/info"); code != 200 || !strings.Contains(body, `"entra": true`) {
		t.Fatalf("/auth/info: %d %s", code, body)
	}
	if code, _ := plainGet(t, base+"/auth/spa-callback?code=x&state=y"); code != http.StatusNotFound {
		t.Fatalf("spa callback in confidential mode: %d", code)
	}
	if _, body := plainGet(t, base+"/status"); !strings.Contains(body, `href="/auth/login?next=/status"`) {
		t.Fatalf("login page without the Microsoft link: %s", body)
	}
	if _, body := plainGet(t, base+"/"); strings.Contains(body, "la-entra") {
		t.Fatalf("panel page with the injected Microsoft link (the login page asks /auth/info): %s", body)
	}

	newBrowser := func() *http.Client {
		jar, _ := cookiejar.New(nil)
		return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	nav := func(c *http.Client, u string, hdr map[string]string) *http.Response {
		req, _ := http.NewRequest("GET", u, nil)
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Dest", "document")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	// start returns the authorize request's parameters.
	start := func(c *http.Client, next string) url.Values {
		resp := nav(c, base+"/auth/login?next="+url.QueryEscape(next), nil)
		resp.Body.Close()
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != http.StatusFound || loc.Path != "/"+testTenant+"/oauth2/v2.0/authorize" {
			t.Fatalf("login: %d %s", resp.StatusCode, loc)
		}
		q := loc.Query()
		if q.Get("client_id") != testClient || q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != base+"/auth/callback" ||
			q.Get("nonce") == "" || q.Get("state") == "" || q.Get("scope") != "openid profile email" {
			t.Fatalf("authorize request: %v", q)
		}
		return q
	}
	idClaims := func(q url.Values, oid, upn string) map[string]any {
		c := f.accessClaims(oid, upn)
		c["nonce"] = q.Get("nonce")
		delete(c, "scp")
		return c
	}
	callback := func(c *http.Client, q url.Values, code string) (int, string) {
		resp := nav(c, base+"/auth/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), nil)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	me := func(c *http.Client) (int, string) {
		resp, err := c.Get(base + "/api/me")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Sub-resource requests don't start a sign-in.
	c := newBrowser()
	resp := nav(c, base+"/auth/login", map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || len(resp.Cookies()) != 0 {
		t.Fatalf("fetch started a sign-in: %d", resp.StatusCode)
	}

	// A user: created, role user, sent on to the target page.
	q := start(c, "/jobs")
	f.mu.Lock()
	f.codes["code1"] = idClaims(q, "dddddddd-0000-4000-8000-000000000001", "dave@example.com")
	f.mu.Unlock()
	code, body := callback(c, q, "code1")
	if code != 200 || !strings.Contains(body, `url=/jobs`) || !strings.Contains(body, "Signed in as dave") {
		t.Fatalf("callback: %d %s", code, body)
	}
	if code, body := me(c); code != 200 || !strings.Contains(body, `"role": "user"`) || !strings.Contains(body, `"tenant": "dave"`) {
		t.Fatalf("me: %d %s", code, body)
	}
	f.mu.Lock()
	form := f.forms[len(f.forms)-1]
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(form.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != q.Get("code_challenge") || form.Get("client_assertion") != "mi-assertion" ||
		form.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" || form.Get("redirect_uri") != base+"/auth/callback" {
		t.Fatalf("token request: %v", form)
	}
	h.mu.Lock()
	dave := h.tenants["dave"]
	h.mu.Unlock()
	if dave == nil || dave.u.Upn != "dave@example.com" {
		t.Fatal("user dave not created")
	}

	// The same state again (a replayed callback with the old cookie): refused.
	c2 := newBrowser()
	q2 := start(c2, "/")
	stateCookie := c2.Jar.Cookies(mustURL(base + "/auth/callback"))
	f.mu.Lock()
	f.codes["code2"] = idClaims(q2, "dddddddd-0000-4000-8000-000000000001", "dave@example.com")
	f.mu.Unlock()
	if code, _ := callback(c2, q2, "code2"); code != 200 {
		t.Fatalf("second sign-in: %d", code)
	}
	c3 := newBrowser()
	c3.Jar.SetCookies(mustURL(base+"/auth/callback"), stateCookie)
	if code, body := callback(c3, q2, "code2"); code != http.StatusBadRequest || !strings.Contains(body, "already used") {
		t.Fatalf("replayed state: %d %s", code, body)
	}

	// A forged cookie, a wrong state, a wrong nonce.
	c4 := newBrowser()
	q4 := start(c4, "/")
	ck := c4.Jar.Cookies(mustURL(base + "/auth/callback"))
	ck[0].Value = strings.Replace(ck[0].Value, ".", "x.", 1)
	c5 := newBrowser()
	c5.Jar.SetCookies(mustURL(base+"/auth/callback"), ck)
	if code, _ := callback(c5, q4, "code4"); code != http.StatusBadRequest {
		t.Fatalf("forged cookie: %d", code)
	}
	c6 := newBrowser()
	q6 := start(c6, "/")
	q6bad := url.Values{"state": {"wrong"}, "nonce": {q6.Get("nonce")}}
	if code, _ := callback(c6, q6bad, "code6"); code != http.StatusBadRequest {
		t.Fatalf("wrong state: %d", code)
	}
	c7 := newBrowser()
	q7 := start(c7, "/")
	bad := idClaims(q7, "dddddddd-0000-4000-8000-000000000001", "dave@example.com")
	bad["nonce"] = "other"
	f.mu.Lock()
	f.codes["code7"] = bad
	f.mu.Unlock()
	if code, body := callback(c7, q7, "code7"); code != http.StatusUnauthorized || !strings.Contains(body, "nonce") {
		t.Fatalf("wrong nonce: %d %s", code, body)
	}
	if code, _ := me(c7); code != http.StatusUnauthorized {
		t.Fatalf("session after a failed sign-in: %d", code)
	}

	// An admin: role admin, sees the user administration.
	c8 := newBrowser()
	q8 := start(c8, "/https://evil.example/")
	f.mu.Lock()
	f.codes["code8"] = idClaims(q8, "bbbbbbbb-0000-4000-8000-000000000001", "Boss@example.com")
	f.mu.Unlock()
	if code, body := callback(c8, q8, "code8"); code != 200 || !strings.Contains(body, `url=/https://evil.example/`) {
		t.Fatalf("admin callback: %d %s", code, body)
	}
	if code, body := me(c8); code != 200 || !strings.Contains(body, `"role": "admin"`) {
		t.Fatalf("admin me: %d %s", code, body)
	}
	if code, _ := plainGetWith(t, c8, base+"/api/tenants"); code != 200 {
		t.Fatalf("admin /api/tenants: %d", code)
	}
	if n := miCalls.Load(); n != 1 {
		t.Errorf("%d managed identity token requests, want 1 (cached)", n)
	}
	// Entra accounts have no password.
	if _, err := h.login(&http.Request{RemoteAddr: "127.0.0.1:1"}, "boss", "", ""); err == nil {
		t.Fatal("password login to an Entra account")
	}
	// Disabling the user ends its panel session.
	if _, err := h.PutUser("test", &pb.PutUserRequest{Name: "dave", SetDisabled: true, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if code, _ := me(c); code == 200 {
		t.Fatal("disabled user's session survives")
	}
	// Token endpoint errors are reported without a session.
	f.mu.Lock()
	f.tokenFn = func(url.Values) (int, map[string]any) {
		return 401, map[string]any{"error": "invalid_client", "error_codes": []int{700016}, "error_description": "AADSTS700016: secret stuff"}
	}
	f.mu.Unlock()
	c9 := newBrowser()
	q9 := start(c9, "/")
	if code, body := callback(c9, q9, "code9"); code != http.StatusBadGateway || !strings.Contains(body, "invalid_client") || strings.Contains(body, "secret stuff") {
		t.Fatalf("token endpoint error: %d %s", code, body)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func plainGet(t *testing.T, u string) (int, string) {
	t.Helper()
	return plainGetWith(t, http.DefaultClient, u)
}

func plainGetWith(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
