package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Sign-in with personal accounts: Google, Apple and Microsoft accounts
// (outlook.com, live.com, ...; [auth.entra] is for work and school
// accounts of one tenant). The web panel signs in with OpenID Connect
// (authorization code, redeemed by the hub with its client credential),
// and the identity - the provider's stable subject - maps to a Longarm
// user, created on first sign-in, exactly like an Entra user. Devices and
// workers link through /link with such a session (`longarm login`,
// `longarmd login`, the tray, MSI LOGIN=1), so the clients and workers one
// account signs in are the same user's: its jobs, its workers.
//
//	GET       /auth/login?provider=<p>&next=/path  to the provider (no provider: a chooser page)
//	GET|POST  /auth/callback/<p>                   code -> ID token -> session (Apple posts: form_post)

// IdPConfig is [auth.google], [auth.microsoft] and [auth.apple]. Off
// unless client_id is set.
type IdPConfig struct {
	// ClientID: Google's OAuth client ID, the Microsoft app registration's
	// application ID (a GUID; supported account types: personal Microsoft
	// accounts), Apple's Services ID.
	ClientID string `toml:"client_id"`
	// ClientSecretFile: the client secret (Google, Microsoft).
	ClientSecretFile string `toml:"client_secret_file"`
	// Apple: the client secret is a JWT the hub signs with the "Sign in
	// with Apple" key (a .p8 file) of the team.
	TeamID         string `toml:"team_id"`
	KeyID          string `toml:"key_id"`
	PrivateKeyFile string `toml:"private_key_file"`
	// Admins get admin certificates and the panel's admin role; Allowed
	// (empty = every account of the provider) may sign in at all. Entries:
	// a verified e-mail address "user@example.com", "*@example.com" or
	// the provider's subject "sub:<id>"; case-insensitive (subjects exact).
	Admins  []string `toml:"admins"`
	Allowed []string `toml:"allowed"`
	// Endpoint overrides (tests, compatible providers): default the
	// provider's own.
	AuthURL  string `toml:"auth_url"`
	TokenURL string `toml:"token_url"`
	JWKSURL  string `toml:"jwks_url"`
	Issuer   string `toml:"issuer"`

	appleKey *ecdsa.PrivateKey
}

// Personal-account providers, in the order the sign-in page lists them.
const (
	idpMicrosoft = "microsoft"
	idpGoogle    = "google"
	idpApple     = "apple"
)

// msaTenant is the tenant ID of every personal Microsoft account.
const msaTenant = "9188040d-6c67-4c5b-b47c-a35d26f2fe1d"

// idpDefaults are a provider's well-known endpoints and its look.
var idpDefaults = map[string]struct {
	label, auth, token, jwks, issuer, scope string
	formPost, pkce, selectAccount           bool
}{
	idpMicrosoft: {label: "Microsoft account", auth: "https://login.microsoftonline.com/consumers/oauth2/v2.0/authorize",
		token: "https://login.microsoftonline.com/consumers/oauth2/v2.0/token", jwks: "https://login.microsoftonline.com/consumers/discovery/v2.0/keys",
		issuer: "https://login.microsoftonline.com/" + msaTenant + "/v2.0", scope: "openid profile email", pkce: true, selectAccount: true},
	idpGoogle: {label: "Google", auth: "https://accounts.google.com/o/oauth2/v2/auth", token: "https://oauth2.googleapis.com/token",
		jwks: "https://www.googleapis.com/oauth2/v3/certs", issuer: "https://accounts.google.com", scope: "openid email profile",
		pkce: true, selectAccount: true},
	// Apple posts the result back (response_mode form_post: required with
	// scopes) and documents no PKCE; state and nonce bind the sign-in.
	idpApple: {label: "Apple", auth: "https://appleid.apple.com/auth/authorize", token: "https://appleid.apple.com/auth/token",
		jwks: "https://appleid.apple.com/auth/keys", issuer: "https://appleid.apple.com", scope: "name email", formPost: true},
}

var idpOrder = []string{idpMicrosoft, idpGoogle, idpApple}

// idps returns the configured personal-account providers' configs by name.
func (a *AuthConfig) idps() map[string]*IdPConfig {
	return map[string]*IdPConfig{idpMicrosoft: &a.Microsoft, idpGoogle: &a.Google, idpApple: &a.Apple}
}

var (
	appleIDRE = regexp.MustCompile(`^[A-Z0-9]{10}$`)
	subRE     = regexp.MustCompile(`^sub:[A-Za-z0-9._|:-]{1,255}$`)
)

func (c *IdPConfig) applyDefaults(name string) error {
	c.ClientID = strings.TrimSpace(c.ClientID)
	if c.ClientID == "" {
		return nil
	}
	switch name {
	case idpMicrosoft:
		if !guidRE.MatchString(c.ClientID) {
			return fmt.Errorf("auth.%s: client_id must be the app registration's application ID (a GUID)", name)
		}
		c.ClientID = strings.ToLower(c.ClientID)
		fallthrough
	case idpGoogle:
		if c.ClientSecretFile == "" {
			return fmt.Errorf("auth.%s: client_secret_file is required", name)
		}
	case idpApple:
		if !appleIDRE.MatchString(c.TeamID) || !appleIDRE.MatchString(c.KeyID) || c.PrivateKeyFile == "" {
			return fmt.Errorf("auth.%s: team_id and key_id (10 characters) and private_key_file are required", name)
		}
		k, err := loadAppleKey(c.PrivateKeyFile)
		if err != nil {
			return fmt.Errorf("auth.%s: private_key_file: %w", name, err)
		}
		c.appleKey = k
	}
	for _, l := range [][]string{c.Admins, c.Allowed} {
		for _, x := range l {
			if !validIdPEntry(x) {
				return fmt.Errorf("auth.%s: invalid entry %q (user@domain, *@domain or sub:<subject>)", name, x)
			}
		}
	}
	for _, u := range []string{c.AuthURL, c.TokenURL, c.JWKSURL} {
		if u == "" {
			continue
		}
		p, err := url.Parse(u)
		if err != nil || p.Host == "" || (p.Scheme != "https" && !(p.Scheme == "http" && isLoopbackHost(p.Hostname()))) {
			return fmt.Errorf("auth.%s: endpoint %q: want https://<host>/...", name, u)
		}
	}
	return nil
}

// loadAppleKey reads a "Sign in with Apple" key: PKCS#8 PEM, EC P-256.
func loadAppleKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("no PEM block (want the .p8 file Apple gave)")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok || ek.Curve.Params().BitSize != 256 {
		return nil, errors.New("not an EC P-256 key")
	}
	return ek, nil
}

func validIdPEntry(s string) bool {
	e := strings.TrimSpace(s)
	if strings.HasPrefix(e, "sub:") {
		return subRE.MatchString(e)
	}
	e = strings.ToLower(e)
	if strings.HasPrefix(e, "*@") {
		return domainRE.MatchString(e)
	}
	return upnEntryRE.MatchString(e)
}

// idpMatch: does a verified e-mail address / subject match an entry?
func idpMatch(entry, email, sub string) bool {
	e := strings.TrimSpace(entry)
	if s, ok := strings.CutPrefix(e, "sub:"); ok {
		return sub != "" && s == sub
	}
	e, email = strings.ToLower(e), strings.ToLower(email)
	if email == "" {
		return false
	}
	if strings.HasPrefix(e, "*@") {
		return strings.HasSuffix(email, e[1:])
	}
	return email == e
}

func idpMatchAny(list []string, email, sub string) bool {
	return slices.ContainsFunc(list, func(e string) bool { return idpMatch(e, email, sub) })
}

// idProvider is one configured personal-account provider.
type idProvider struct {
	name, label                      string
	cfg                              *IdPConfig
	authURL, tokenURL, issuer, scope string
	formPost, pkce, selectAccount    bool
	keys                             *jwks
}

func newIdProviders(a *AuthConfig) map[string]*idProvider {
	out := map[string]*idProvider{}
	for name, c := range a.idps() {
		if c.ClientID == "" {
			continue
		}
		d := idpDefaults[name]
		p := &idProvider{name: name, label: d.label, cfg: c, authURL: cmpOr(c.AuthURL, d.auth), tokenURL: cmpOr(c.TokenURL, d.token),
			issuer: cmpOr(c.Issuer, d.issuer), scope: d.scope, formPost: d.formPost, pkce: d.pkce, selectAccount: d.selectAccount,
			keys: newJWKS(cmpOr(c.JWKSURL, d.jwks))}
		out[name] = p
	}
	return out
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// webProvider is a sign-in button: a provider's ID and label.
type webProvider struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// webProviders lists the panel's sign-in providers: Entra first, then the
// personal-account ones.
func (h *Hub) webProviders() []webProvider {
	var out []webProvider
	if h.entraWeb() != "" {
		out = append(out, webProvider{ID: "entra", Label: "Microsoft (work or school)"})
	}
	for _, n := range idpOrder {
		if p := h.idps[n]; p != nil {
			out = append(out, webProvider{ID: n, Label: p.label})
		}
	}
	return out
}

// idpOn: the hub signs people in with an identity provider (Entra or a
// personal-account one). Then /link approves only sessions such a sign-in
// opened.
func (h *Hub) idpOn() bool { return h.entra != nil || len(h.idps) > 0 }

// webSignIn: the panel offers an identity provider's sign-in.
func (h *Hub) webSignIn() bool { return h.entraWeb() != "" || len(h.idps) > 0 }

// identity keys: an Entra user's is its object ID (as always), a personal
// account's "<provider>:<subject>". The key binds the Longarm user (store
// meta, see entraOIDKey) and the panel account (store.User.Entra).
func (id entraIdentity) key() string {
	if id.provider == "" {
		return id.oid
	}
	return id.provider + ":" + id.oid
}

// who names the identity in audit records and logs: "entra/<upn>",
// "google/<e-mail or subject>".
func (id entraIdentity) who() string {
	p := cmpOr(id.provider, "entra")
	return p + "/" + cmpOr(id.upn, id.oid)
}

// identityFromKey is the identity behind a key (the e-mail address is the
// user record's, filled in by the caller).
func identityFromKey(k string) entraIdentity {
	if p, sub, ok := strings.Cut(k, ":"); ok && idpDefaults[p].issuer != "" {
		return entraIdentity{provider: p, oid: sub}
	}
	return entraIdentity{oid: k}
}

// idpPolicy says whether id is an admin and whether it may sign in at all.
func (h *Hub) idpPolicy(id entraIdentity) (admin, allowed bool) {
	if id.provider == "" {
		e := &h.cfg.Auth.Entra
		admin = entraMatchAny(e.Admins, id.upn, id.oid)
		return admin, admin || len(e.Allowed) == 0 || entraMatchAny(e.Allowed, id.upn, id.oid)
	}
	p := h.idps[id.provider]
	if p == nil { // its provider was removed from hub.toml
		return false, false
	}
	admin = idpMatchAny(p.cfg.Admins, id.upn, id.oid)
	return admin, admin || len(p.cfg.Allowed) == 0 || idpMatchAny(p.cfg.Allowed, id.upn, id.oid)
}

// ---------------------------------------------------------------- tokens

// idClaims are the ID token claims of a personal-account provider.
type idClaims struct {
	Iss               string   `json:"iss"`
	Aud               audience `json:"aud"`
	Sub               string   `json:"sub"`
	Tid               string   `json:"tid"`
	Nonce             string   `json:"nonce"`
	Email             string   `json:"email"`
	EmailVerified     any      `json:"email_verified"` // bool (Google), "true" (Apple)
	PreferredUsername string   `json:"preferred_username"`
	Exp               *float64 `json:"exp"`
	Nbf               *float64 `json:"nbf"`
}

// verifyIDToken checks a provider's ID token: signature, issuer, audience
// (the client ID), expiry, nonce, subject; Microsoft: a personal account.
// It returns the identity: the subject and the verified e-mail address
// ("" when the provider doesn't vouch for one).
func (p *idProvider) verifyIDToken(ctx context.Context, token, nonce string, now time.Time) (entraIdentity, error) {
	payload, err := verifyJWT(ctx, p.keys, token)
	if err != nil {
		return entraIdentity{}, err
	}
	var c idClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return entraIdentity{}, errors.New("bad token payload")
	}
	switch {
	case c.Iss != p.issuer && !(p.name == idpGoogle && p.cfg.Issuer == "" && c.Iss == "accounts.google.com"):
		return entraIdentity{}, fmt.Errorf("token issuer %q, want %q", c.Iss, p.issuer)
	case !slices.Contains(c.Aud, p.cfg.ClientID):
		return entraIdentity{}, fmt.Errorf("token audience %q is not this hub's client", strings.Join(c.Aud, ","))
	case c.Exp == nil:
		return entraIdentity{}, errors.New("token without expiry")
	case now.After(time.Unix(int64(*c.Exp), 0).Add(jwtSkew)):
		return entraIdentity{}, errors.New("token expired")
	case c.Nbf != nil && now.Add(jwtSkew).Before(time.Unix(int64(*c.Nbf), 0)):
		return entraIdentity{}, errors.New("token not valid yet")
	case c.Sub == "" || len(c.Sub) > 255 || strings.ContainsAny(c.Sub, " \t\r\n"):
		return entraIdentity{}, errors.New("token without a usable subject (sub)")
	case !constantEqual(c.Nonce, nonce) || nonce == "":
		return entraIdentity{}, errors.New("token nonce mismatch")
	case p.name == idpMicrosoft && !strings.EqualFold(c.Tid, msaTenant):
		return entraIdentity{}, errors.New("not a personal Microsoft account (work and school accounts sign in with [auth.entra])")
	}
	id := entraIdentity{provider: p.name, oid: c.Sub}
	switch {
	case p.name == idpMicrosoft:
		// The account's sign-in name: Microsoft owns its verification.
		if n := cmpOr(c.PreferredUsername, c.Email); upnEntryRE.MatchString(strings.ToLower(n)) {
			id.upn = strings.ToLower(n)
		}
	case c.EmailVerified == true || c.EmailVerified == "true":
		if upnEntryRE.MatchString(strings.ToLower(c.Email)) {
			id.upn = strings.ToLower(c.Email)
		}
	}
	return id, nil
}

// appleClientSecret is the client secret Apple wants: a short-lived ES256
// JWT of the team, for the Services ID.
func (p *idProvider) appleClientSecret(now time.Time) (string, error) {
	c := p.cfg
	hdr, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": c.KeyID, "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": c.TeamID, "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"aud": "https://appleid.apple.com", "sub": c.ClientID})
	body := b64url.EncodeToString(hdr) + "." + b64url.EncodeToString(claims)
	sum := sha256.Sum256([]byte(body))
	r, s, err := ecdsa.Sign(rand.Reader, c.appleKey, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return body + "." + b64url.EncodeToString(sig), nil
}

// redeem exchanges an authorization code for the ID token.
func (p *idProvider) redeem(ctx context.Context, code, verifier, redirectURI string) (string, error) {
	form := url.Values{"client_id": {p.cfg.ClientID}, "grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}}
	if p.pkce {
		form.Set("code_verifier", verifier)
	}
	if p.name == idpApple {
		s, err := p.appleClientSecret(time.Now())
		if err != nil {
			return "", fmt.Errorf("client secret: %w", err)
		}
		form.Set("client_secret", s)
	} else {
		b, err := os.ReadFile(p.cfg.ClientSecretFile)
		if err != nil {
			return "", fmt.Errorf("client secret: %w", err)
		}
		form.Set("client_secret", strings.TrimSpace(string(b)))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := entraHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("token endpoint: %s: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK || out.IDToken == "" {
		// The error code only: descriptions can echo request data.
		return "", fmt.Errorf("token endpoint: %s %s", resp.Status, out.Error)
	}
	return out.IDToken, nil
}

// ---------------------------------------------------------------- web

func (h *Hub) idpRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/callback/{provider}", h.idpCallback)
	mux.HandleFunc("POST /auth/callback/{provider}", h.idpCallback)
}

func (h *Hub) idpRedirectURI(r *http.Request, p *idProvider) string {
	return h.baseURL(r) + "/auth/callback/" + p.name
}

// signInStart is GET /auth/login: to the requested provider; without one,
// to the only one, or a page to choose.
func (h *Hub) signInStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name := r.URL.Query().Get("provider")
	if name == "" {
		ps := h.webProviders()
		switch len(ps) {
		case 0:
			http.NotFound(w, r)
			return
		case 1:
			name = ps[0].ID
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			chooserTmpl.Execute(w, map[string]any{"Providers": ps, "Next": safeNext(r.URL.Query().Get("next"))})
			return
		}
	}
	if name == "entra" {
		h.entraLogin(w, r)
		return
	}
	p := h.idps[name]
	if p == nil {
		http.NotFound(w, r)
		return
	}
	if !topLevelNavigation(r) {
		httpError(w, http.StatusUnauthorized, "sign-in starts with a page navigation")
		return
	}
	st := oidcState{Provider: p.name, State: randomB64(16), Verifier: randomB64(32), Nonce: randomB64(16),
		Next: safeNext(r.URL.Query().Get("next")), Exp: time.Now().Add(oidcStateTTL).Unix()}
	val, err := h.sealOIDC(st)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	h.setOIDCCookieFor(w, val, int(oidcStateTTL.Seconds()), p.formPost)
	q := url.Values{"client_id": {p.cfg.ClientID}, "response_type": {"code"}, "redirect_uri": {h.idpRedirectURI(r, p)},
		"scope": {p.scope}, "state": {st.State}, "nonce": {st.Nonce}}
	if p.formPost {
		q.Set("response_mode", "form_post")
	}
	if p.pkce {
		ch := sha256.Sum256([]byte(st.Verifier))
		q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(ch[:]))
		q.Set("code_challenge_method", "S256")
	}
	if p.selectAccount {
		q.Set("prompt", "select_account")
	}
	http.Redirect(w, r, p.authURL+"?"+q.Encode(), http.StatusFound)
}

// idpCallback: the provider sends the browser back (query, or Apple's
// form post) with a code; the hub redeems it and opens a session.
func (h *Hub) idpCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p := h.idps[r.PathValue("provider")]
	if p == nil || (r.Method == http.MethodPost) != p.formPost {
		http.NotFound(w, r)
		return
	}
	ip := h.clientIP(r)
	if !h.loginLimit.allow(ip) {
		h.entraPage(w, http.StatusTooManyRequests, errTooMany.Error(), "")
		return
	}
	params := r.URL.Query()
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			h.entraPage(w, http.StatusBadRequest, "Bad sign-in response.", "")
			return
		}
		params = r.PostForm
	}
	if e := params.Get("error"); e != "" {
		h.setOIDCCookieFor(w, "", -1, p.formPost)
		h.log.Warn("web sign-in: error from the provider", "provider", p.name, "error", truncate(e, 100), "remote", ip)
		h.entraPage(w, http.StatusUnauthorized, p.label+" sign-in failed: "+truncate(e, 100), "")
		return
	}
	st, ok := h.openOIDC(r)
	h.setOIDCCookieFor(w, "", -1, p.formPost) // one attempt per cookie
	if !ok || st.Provider != p.name || !constantEqual(params.Get("state"), st.State) || params.Get("code") == "" {
		h.entraPage(w, http.StatusBadRequest, "This sign-in expired or was not started here. Please try again.", "")
		return
	}
	if !h.oidc.consumeState(st.State, time.Unix(st.Exp, 0)) {
		h.entraPage(w, http.StatusBadRequest, "This sign-in was already used. Please try again.", "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	idToken, err := p.redeem(ctx, params.Get("code"), st.Verifier, h.idpRedirectURI(r, p))
	if err != nil {
		h.log.Warn("web sign-in: code redemption failed", "provider", p.name, "err", err, "remote", ip)
		h.entraPage(w, http.StatusBadGateway, "The hub could not complete the sign-in with "+p.label+": "+err.Error(), "")
		return
	}
	id, err := p.verifyIDToken(ctx, idToken, st.Nonce, time.Now())
	if err != nil {
		h.log.Warn("web sign-in: ID token rejected", "provider", p.name, "err", err, "remote", ip)
		h.entraPage(w, http.StatusUnauthorized, "The ID token was rejected: "+err.Error(), "")
		return
	}
	user, code, err := h.identitySession(w, ip, id, p.name)
	if err != nil {
		h.entraPage(w, code, err.Error(), "")
		return
	}
	// A page that moves on by itself (the session cookie is SameSite=Strict).
	h.entraPage(w, http.StatusOK, "Signed in as "+user+".", st.Next)
}

var chooserTmpl = template.Must(template.New("chooser").Parse(`<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>longarm-hub sign-in</title>
<style>body{font:15px system-ui,sans-serif;margin:2em;max-width:26em}a.b{display:block;margin:.6em 0;padding:.7em 1em;border:1px solid #bbb;border-radius:6px;text-decoration:none;color:inherit}a.b:hover{background:#f3f3f3}</style>
</head><body><h1>longarm-hub</h1><p>Sign in with</p>
{{range .Providers}}<a class="b" href="/auth/login?provider={{.ID}}&amp;next={{$.Next}}">{{.Label}}</a>{{end}}
<p><a href="/">Or sign in with a password</a></p></body></html>`))
