package hub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/hub/store"
)

// Web panel sign-in with Microsoft Entra ID: OpenID Connect authorization
// code flow with PKCE (S256) and a nonce, in one of two modes ([auth.entra]
// web):
//
//   - confidential: Microsoft redirects to /auth/callback and the hub
//     redeems the code with its client credential (its managed identity's
//     token as a federated client assertion, or a client secret).
//   - spa: for app registrations that may have no credential at all.
//     Microsoft redirects to /auth/spa-callback (an SPA platform redirect
//     URI), a page whose script redeems the code at Microsoft's token
//     endpoint (CORS) and posts the ID token to /auth/spa-session.
//
// Either way the hub sets the state, PKCE verifier, nonce and target in a
// short-lived cookie (HMAC-signed with a key kept in the database, so a
// restart in the middle of a sign-in doesn't break it) before sending the
// browser to Microsoft, accepts each state once, and verifies the ID token
// itself (signature, issuer, audience, tenant, expiry, nonce) before it
// opens a panel session.
//
//	GET  /auth/info          {"entra": bool, "mode": ...} (the login page's button)
//	GET  /auth/login         ?next=/path: to Microsoft (top-level navigations only)
//	GET  /auth/callback      confidential: code -> ID token -> session
//	GET  /auth/spa-callback  spa: the page redeeming the code in the browser
//	POST /auth/spa-session   spa: {"id_token"} -> session, {"next"}

const (
	oidcCookie   = "longarm_hub_oidc"
	oidcStateTTL = 10 * time.Minute
	oidcKeyMeta  = "oidc_cookie_key"
	oidcScope    = "openid profile email"
)

type oidcState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
	Next     string `json:"t"`
	Exp      int64  `json:"e"`
}

// entraWeb is the panel's Entra sign-in mode, "" when there is none.
func (h *Hub) entraWeb() string {
	if h.entra == nil || h.cfg.Auth.Entra.Web == EntraWebOff {
		return ""
	}
	return h.cfg.Auth.Entra.Web
}

func (h *Hub) entraRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"entra": h.entraWeb() != "", "mode": h.entraWeb()})
	})
	mux.HandleFunc("GET /auth/login", h.entraLogin)
	mux.HandleFunc("GET /auth/callback", h.entraCallback)
	mux.HandleFunc("GET /auth/spa-callback", h.entraSPACallback)
	mux.HandleFunc("POST /auth/spa-session", h.entraSPASession)
}

// topLevelNavigation: only a page navigation starts a sign-in. Scripts and
// sub-resources get a 401 instead of a new state cookie, which would
// replace the state of a sign-in already under way.
func topLevelNavigation(r *http.Request) bool {
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		dest := r.Header.Get("Sec-Fetch-Dest")
		return mode == "navigate" && (dest == "" || dest == "document")
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html") // no fetch metadata (older browsers)
}

// safeNext keeps the target after sign-in on this site: a path, never
// another origin ("//evil", "/\evil", "https://...").
func safeNext(s string) string {
	if s == "" || len(s) > 1024 || s[0] != '/' || strings.HasPrefix(s, "//") || strings.ContainsAny(s, "\\\r\n\t") {
		return "/"
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || strings.HasPrefix(u.Path, "//") {
		return "/"
	}
	return s
}

// entraRedirectURI is where Microsoft sends the browser back (registered in
// the app registration: Web platform for confidential, SPA for spa).
func (h *Hub) entraRedirectURI(r *http.Request) string {
	if h.entraWeb() == EntraWebSPA {
		return h.baseURL(r) + "/auth/spa-callback"
	}
	return h.baseURL(r) + "/auth/callback"
}

func (h *Hub) entraLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.entraWeb() == "" {
		http.NotFound(w, r)
		return
	}
	if !topLevelNavigation(r) {
		httpError(w, http.StatusUnauthorized, "sign-in starts with a page navigation")
		return
	}
	st := oidcState{State: randomB64(16), Verifier: randomB64(32), Nonce: randomB64(16),
		Next: safeNext(r.URL.Query().Get("next")), Exp: time.Now().Add(oidcStateTTL).Unix()}
	val, err := h.sealOIDC(st)
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	h.setOIDCCookie(w, val, int(oidcStateTTL.Seconds()))
	ch := sha256.Sum256([]byte(st.Verifier))
	e := &h.cfg.Auth.Entra
	q := url.Values{"client_id": {e.ClientID}, "response_type": {"code"}, "redirect_uri": {h.entraRedirectURI(r)},
		"response_mode": {"query"}, "scope": {oidcScope}, "state": {st.State}, "nonce": {st.Nonce},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(ch[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, e.endpoint("/oauth2/v2.0/authorize")+"?"+q.Encode(), http.StatusFound)
}

// callbackState checks a redirect back from Microsoft against the cookie;
// it writes the error page and returns false when it doesn't fit.
func (h *Hub) callbackState(w http.ResponseWriter, r *http.Request) (oidcState, bool) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		h.setOIDCCookie(w, "", -1)
		h.log.Warn("entra web sign-in: error from Microsoft", "error", e, "remote", h.clientIP(r))
		h.entraPage(w, http.StatusUnauthorized, "Microsoft sign-in failed: "+e+": "+truncate(q.Get("error_description"), 300), "")
		return oidcState{}, false
	}
	st, ok := h.openOIDC(r)
	if !ok || !constantEqual(q.Get("state"), st.State) || q.Get("code") == "" {
		h.setOIDCCookie(w, "", -1)
		h.entraPage(w, http.StatusBadRequest, "This sign-in expired or was not started here. Please try again.", "")
		return oidcState{}, false
	}
	return st, true
}

// entraCallback (confidential): the hub redeems the code.
func (h *Hub) entraCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.entraWeb() != EntraWebConfidential {
		http.NotFound(w, r)
		return
	}
	ip := h.clientIP(r)
	if !h.loginLimit.allow(ip) {
		h.entraPage(w, http.StatusTooManyRequests, errTooMany.Error(), "")
		return
	}
	st, ok := h.callbackState(w, r)
	if !ok {
		return
	}
	h.setOIDCCookie(w, "", -1) // one attempt per cookie
	if !h.entra.consumeState(st.State, time.Unix(st.Exp, 0)) {
		h.entraPage(w, http.StatusBadRequest, "This sign-in was already used. Please try again.", "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	idToken, err := h.redeemCode(ctx, r.URL.Query().Get("code"), st.Verifier, h.entraRedirectURI(r))
	if err != nil {
		h.log.Warn("entra web sign-in: code redemption failed", "err", err, "remote", ip)
		h.entraPage(w, http.StatusBadGateway, "The hub could not complete the sign-in with Microsoft: "+err.Error(), "")
		return
	}
	user, code, err := h.entraSession(ctx, w, ip, idToken, st)
	if err != nil {
		h.entraPage(w, code, err.Error(), "")
		return
	}
	// A page that moves on by itself instead of a redirect: the session
	// cookie is SameSite=Strict, and browsers hold it back on a redirect
	// chain that started at Microsoft.
	h.entraPage(w, http.StatusOK, "Signed in as "+user+".", st.Next)
}

// entraSPACallback (spa): a page whose script redeems the code at
// Microsoft (no client credential: the SPA redirect URI allows that from a
// browser only) and hands the ID token to /auth/spa-session.
func (h *Hub) entraSPACallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.entraWeb() != EntraWebSPA {
		http.NotFound(w, r)
		return
	}
	st, ok := h.callbackState(w, r)
	if !ok {
		return
	}
	e := &h.cfg.Auth.Entra
	nonce := randomB64(16)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'unsafe-inline'; connect-src 'self' "+
		e.Authority+"; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	spaPageTmpl.Execute(w, map[string]any{"Nonce": nonce, "Cfg": map[string]string{
		"token_url": e.endpoint("/oauth2/v2.0/token"), "client_id": e.ClientID, "code": r.URL.Query().Get("code"),
		"redirect_uri": h.entraRedirectURI(r), "code_verifier": st.Verifier, "scope": oidcScope}})
}

// entraSPASession (spa): the ID token the page got from Microsoft, checked
// against the cookie's nonce, becomes a panel session.
func (h *Hub) entraSPASession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.entraWeb() != EntraWebSPA {
		http.NotFound(w, r)
		return
	}
	// Only the hub's own page may post here.
	if site := r.Header.Get("Sec-Fetch-Site"); r.Header.Get("Origin") == "" || !h.sameOrigin(r) || site != "" && site != "same-origin" {
		httpError(w, http.StatusForbidden, "cross-origin request refused")
		return
	}
	ip := h.clientIP(r)
	if !h.loginLimit.allow(ip) {
		httpError(w, http.StatusTooManyRequests, errTooMany.Error())
		return
	}
	var body struct {
		IDToken string `json:"id_token"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	st, ok := h.openOIDC(r)
	h.setOIDCCookie(w, "", -1) // one attempt per cookie
	if !ok {
		httpError(w, http.StatusBadRequest, "this sign-in expired or was not started here; please try again")
		return
	}
	if !h.entra.consumeState(st.State, time.Unix(st.Exp, 0)) {
		httpError(w, http.StatusBadRequest, "this sign-in was already used; please try again")
		return
	}
	user, code, err := h.entraSession(r.Context(), w, ip, body.IDToken, st)
	if err != nil {
		httpError(w, code, err.Error())
		return
	}
	writeJSON(w, map[string]string{"user": user, "next": st.Next})
}

// entraSession verifies an ID token for the sign-in st, maps it to its
// Longarm user and opens a panel session. On error: an HTTP status code.
func (h *Hub) entraSession(ctx context.Context, w http.ResponseWriter, ip, idToken string, st oidcState) (string, int, error) {
	e := &h.cfg.Auth.Entra
	// No apps here: app-only tokens never open panel sessions.
	claims, err := verifyEntraToken(ctx, h.entra.keys, idToken, tokenCheck{issuer: e.issuer(), tenant: e.TenantID,
		audiences: []string{e.ClientID}, nonce: st.Nonce}, time.Now())
	if err != nil {
		h.log.Warn("entra web sign-in: ID token rejected", "err", err, "remote", ip)
		return "", http.StatusUnauthorized, fmt.Errorf("the ID token was rejected: %w", err)
	}
	id := claims.identity()
	user, admin, err := h.entraSignIn(id, "web")
	if err != nil {
		return "", http.StatusForbidden, errors.New(errMessage(err))
	}
	if err := h.entraPanelLogin(user, id.oid, admin); err != nil {
		h.log.Warn("entra web sign-in: panel account", "user", user, "err", err)
		return "", http.StatusConflict, err
	}
	tok := randomToken()
	if err := h.store.AddWebSession(sessionHash(tok), user, time.Now().Add(h.cfg.Web.SessionTTL.Duration)); err != nil {
		return "", http.StatusInternalServerError, err
	}
	h.setSession(w, tok)
	role := map[bool]string{true: "admin", false: "user"}[admin]
	h.log.Info("panel login (entra)", "user", user, "upn", id.upn, "role", role, "mode", e.Web, "remote", ip)
	h.auditHub("entra/"+id.upn, "entra/panel-login", map[string]any{"user": user, "upn": id.upn, "oid": id.oid,
		"role": role, "mode": e.Web, "remote": ip})
	return user, http.StatusOK, nil
}

// entraPanelLogin creates or updates the panel account of an Entra user:
// named like its Longarm user, role admin for the hub's Entra admins, else
// user. The user's own password login (longarm admin user add --panel)
// becomes reachable by Entra sign-in too; anyone else's is left alone.
func (h *Hub) entraPanelLogin(user, oid string, admin bool) error {
	h.loginMu.Lock() // password logins rewrite the record too (TOTP step)
	defer h.loginMu.Unlock()
	u, err := h.store.GetUser(user)
	switch {
	case errors.Is(err, store.ErrNotFound):
		u = &store.User{Name: user, CreatedAt: time.Now().UTC()}
	case err != nil:
		return err
	case u.Entra == oid, u.Entra == "" && u.Role == "user" && u.Tenant == user:
	default:
		return fmt.Errorf("the panel account %s belongs to someone else; ask an administrator", user)
	}
	u.Role, u.Tenant, u.Entra = "user", user, oid
	if admin {
		u.Role = "admin"
	}
	return h.store.PutUser(u)
}

func (h *Hub) setOIDCCookie(w http.ResponseWriter, val string, maxAge int) {
	// Lax, not Strict: the callback is a top-level navigation from Microsoft.
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: val, Path: "/auth/", HttpOnly: true,
		Secure: !h.cfg.Web.InsecureCookie, SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

// oidcKey is the cookie MAC key, made once and kept in the database (the
// hub's state, replicated with it).
func (h *Hub) oidcKey() ([]byte, error) {
	h.entra.mu.Lock()
	defer h.entra.mu.Unlock()
	if h.entra.cookieKey != nil {
		return h.entra.cookieKey, nil
	}
	v, err := h.store.Meta(oidcKeyMeta)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(v)
	if err != nil || len(key) != 32 {
		key = make([]byte, 32)
		rand.Read(key)
		if err := h.store.SetMeta(oidcKeyMeta, hex.EncodeToString(key)); err != nil {
			return nil, err
		}
	}
	h.entra.cookieKey = key
	return key, nil
}

func oidcMAC(key []byte, body string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("longarm-hub oidc state:" + body))
	return m.Sum(nil)
}

func (h *Hub) sealOIDC(st oidcState) (string, error) {
	key, err := h.oidcKey()
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(st)
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(oidcMAC(key, body)), nil
}

// openOIDC returns the sign-in state of r's cookie if its MAC is right and
// it hasn't expired.
func (h *Hub) openOIDC(r *http.Request) (oidcState, bool) {
	var st oidcState
	c, err := r.Cookie(oidcCookie)
	if err != nil {
		return st, false
	}
	body, mac, ok := strings.Cut(c.Value, ".")
	key, err := h.oidcKey()
	if !ok || err != nil {
		return st, false
	}
	got, err := base64.RawURLEncoding.DecodeString(mac)
	if err != nil || !hmac.Equal(got, oidcMAC(key, body)) {
		return st, false
	}
	b, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || json.Unmarshal(b, &st) != nil || st.State == "" || time.Now().Unix() > st.Exp {
		return st, false
	}
	return st, true
}

// consumeState marks a state used; false if it was already.
func (e *entraState) consumeState(state string, exp time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for s, t := range e.usedState {
		if now.After(t) {
			delete(e.usedState, s)
		}
	}
	if _, used := e.usedState[state]; used {
		return false
	}
	e.usedState[state] = exp
	return true
}

func randomB64(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

var entraHTTP = &http.Client{Timeout: 20 * time.Second}

// redeemCode exchanges an authorization code for the ID token
// (confidential mode).
func (h *Hub) redeemCode(ctx context.Context, code, verifier, redirectURI string) (string, error) {
	e := &h.cfg.Auth.Entra
	form := url.Values{"client_id": {e.ClientID}, "grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {redirectURI}, "code_verifier": {verifier}, "scope": {oidcScope}}
	switch {
	case e.ClientAssertion == "managed-identity":
		a, err := h.managedIdentityToken(ctx)
		if err != nil {
			return "", fmt.Errorf("managed identity token (client assertion): %w", err)
		}
		form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
		form.Set("client_assertion", a)
	case e.ClientSecretFile != "":
		b, err := os.ReadFile(e.ClientSecretFile)
		if err != nil {
			return "", fmt.Errorf("client secret: %w", err)
		}
		form.Set("client_secret", strings.TrimSpace(string(b)))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint("/oauth2/v2.0/token"), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := entraHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		IDToken    string `json:"id_token"`
		Error      string `json:"error"`
		ErrorCodes []int  `json:"error_codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("token endpoint: %s: %w", resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK || out.IDToken == "" {
		// The error code (AADSTS...) only: the description can echo request data.
		return "", fmt.Errorf("token endpoint: %s %s %v", resp.Status, out.Error, out.ErrorCodes)
	}
	return out.IDToken, nil
}

// cachedToken is a managed identity token until shortly before it expires.
type cachedToken struct {
	tok string
	exp time.Time
}

// imdsEndpoint is Azure's instance metadata identity endpoint (Container
// Instances, VMs); App Service / Container Apps set IDENTITY_ENDPOINT.
var imdsEndpoint = "http://169.254.169.254/metadata/identity/oauth2/token"

// managedIdentityToken is a token of the hub's managed identity (the
// user-assigned one named by AZURE_CLIENT_ID, else the system-assigned
// one) for api://AzureADTokenExchange: the client assertion an app
// registration accepts through a federated credential.
func (h *Hub) managedIdentityToken(ctx context.Context) (string, error) {
	h.entra.mu.Lock()
	if c := h.entra.mi; c.tok != "" && time.Until(c.exp) > 5*time.Minute {
		h.entra.mu.Unlock()
		return c.tok, nil
	}
	h.entra.mu.Unlock()
	q := url.Values{"resource": {"api://AzureADTokenExchange"}}
	if id := os.Getenv("AZURE_CLIENT_ID"); id != "" {
		q.Set("client_id", id)
	}
	var req *http.Request
	var err error
	if ep, secret := os.Getenv("IDENTITY_ENDPOINT"), os.Getenv("IDENTITY_HEADER"); ep != "" && secret != "" {
		q.Set("api-version", "2019-08-01")
		if req, err = http.NewRequestWithContext(ctx, http.MethodGet, ep+"?"+q.Encode(), nil); err != nil {
			return "", err
		}
		req.Header.Set("X-IDENTITY-HEADER", secret)
	} else {
		q.Set("api-version", "2018-02-01")
		if req, err = http.NewRequestWithContext(ctx, http.MethodGet, imdsEndpoint+"?"+q.Encode(), nil); err != nil {
			return "", err
		}
		req.Header.Set("Metadata", "true")
	}
	resp, err := entraHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string          `json:"access_token"`
		ExpiresOn   json.RawMessage `json:"expires_on"` // "1700000000" or 1700000000
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		return "", fmt.Errorf("identity endpoint: %s", resp.Status)
	}
	exp := time.Now().Add(10 * time.Minute)
	if sec, err := strconv.ParseInt(strings.Trim(string(out.ExpiresOn), `"`), 10, 64); err == nil {
		exp = time.Unix(sec, 0)
	}
	h.entra.mu.Lock()
	h.entra.mi = cachedToken{tok: out.AccessToken, exp: exp}
	h.entra.mu.Unlock()
	return out.AccessToken, nil
}

var entraPageTmpl = template.Must(template.New("entra").Parse(`<!doctype html><html><head><meta charset="utf-8">
{{if .Next}}<meta http-equiv="refresh" content="0;url={{.Next}}">{{end}}<title>longarm-hub sign-in</title>
<style>body{font:14px system-ui,sans-serif;margin:2em}</style></head><body><h1>longarm-hub</h1><p>{{.Msg}}</p>
{{if .Next}}<p><a href="{{.Next}}">Continue</a></p>{{else}}<p><a href="/auth/login">Try again</a> or <a href="/">sign in with a password</a>.</p>{{end}}
</body></html>`))

func (h *Hub) entraPage(w http.ResponseWriter, code int, msg, next string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	entraPageTmpl.Execute(w, map[string]string{"Msg": msg, "Next": next})
}

// spaPageTmpl redeems the code in the browser (spa mode). The code and
// verifier never leave this page except to Microsoft; the ID token goes to
// the hub, which checks it like any other.
var spaPageTmpl = template.Must(template.New("spa").Parse(`<!doctype html><html><head><meta charset="utf-8">
<title>longarm-hub sign-in</title><style>body{font:14px system-ui,sans-serif;margin:2em}</style></head><body>
<h1>longarm-hub</h1><p id="msg">Signing in…</p><p id="retry" hidden><a href="/auth/login">Try again</a> or <a href="/">sign in with a password</a>.</p>
<script nonce="{{.Nonce}}">
(async () => {
  const cfg = {{.Cfg}};
  history.replaceState(null, '', '/auth/spa-callback');
  const msg = document.getElementById('msg');
  try {
    const r = await fetch(cfg.token_url, {method: 'POST', headers: {'Content-Type': 'application/x-www-form-urlencoded'},
      body: new URLSearchParams({client_id: cfg.client_id, grant_type: 'authorization_code', code: cfg.code,
        redirect_uri: cfg.redirect_uri, code_verifier: cfg.code_verifier, scope: cfg.scope})});
    const t = await r.json().catch(() => ({}));
    if (!r.ok || !t.id_token) throw new Error('Microsoft: ' + (t.error || r.status) + (t.error_codes ? ' ' + t.error_codes.join(',') : ''));
    const s = await fetch('/auth/spa-session', {method: 'POST', credentials: 'same-origin',
      headers: {'Content-Type': 'application/json'}, body: JSON.stringify({id_token: t.id_token})});
    const j = await s.json().catch(() => ({}));
    if (!s.ok) throw new Error(j.error || s.status);
    msg.textContent = 'Signed in as ' + j.user + '.';
    location.replace(j.next || '/');
  } catch (e) {
    msg.textContent = 'Sign-in failed: ' + (e && e.message ? e.message : e);
    document.getElementById('retry').hidden = false;
  }
})();
</script></body></html>`))

// entraLinkHead / entraLinkBody add "Sign in with Microsoft" to the
// panel's page, visible only while its login form is (CSS :has, no
// script, so the panel's CSP holds) - the built frontend (internal/webui)
// has no such button yet.
const (
	entraLinkHead = `<style>#la-entra{display:none}body:has(form.login) #la-entra{display:block;position:fixed;left:50%;bottom:12vh;` +
		`transform:translateX(-50%);padding:8px 18px;border:1px solid #8888;border-radius:6px;color:inherit;text-decoration:none;font:14px system-ui,sans-serif}</style>`
	entraLinkBody = `<a id="la-entra" href="/auth/login">Sign in with Microsoft</a>`
)

// withEntraLink serves the panel with the Entra sign-in link when the hub
// has one.
func (h *Hub) withEntraLink(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.entraWeb() == "" {
			next.ServeHTTP(w, r)
			return
		}
		bw := &bufferedWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(bw, r)
		b := bw.buf.Bytes()
		if strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			s := strings.Replace(string(b), "</head>", entraLinkHead+"</head>", 1)
			b = []byte(strings.Replace(s, "</body>", entraLinkBody+"</body>", 1))
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(bw.code)
		w.Write(b)
	})
}

type bufferedWriter struct {
	http.ResponseWriter
	buf  bytes.Buffer
	code int
}

func (b *bufferedWriter) WriteHeader(code int)        { b.code = code }
func (b *bufferedWriter) Write(p []byte) (int, error) { return b.buf.Write(p) }
