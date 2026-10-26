package hub

import (
	"cmp"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Microsoft Entra ID tokens (JWTs signed RS256 with the tenant's published
// keys). A small verifier instead of a JOSE library: Entra only uses RS256
// with RSA keys from one JWKS, and everything else is refused - in
// particular "none" and HMAC algorithms, whose "key" would be public.

const (
	jwtSkew        = 2 * time.Minute // clock skew allowed on exp / nbf
	maxTokenLen    = 16 << 10
	jwksMaxAge     = 24 * time.Hour // keys are refetched daily
	jwksMinRefresh = time.Minute    // an unknown kid refetches at most this often
	jwksMaxKeys    = 64             // bounded cache
	jwksMaxBody    = 1 << 20
)

// jwks caches a tenant's signing keys by key ID.
type jwks struct {
	url    string
	client *http.Client

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time // last successful fetch
	triedAt   time.Time // last attempt
}

func newJWKS(url string) *jwks {
	return &jwks{url: url, client: &http.Client{Timeout: 15 * time.Second}}
}

// key returns the key kid, fetching the set when it is old or doesn't know
// kid (rotation) - an unknown kid at most once a minute, so made-up key IDs
// cannot turn the hub into a load generator.
func (j *jwks) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	k := j.keys[kid]
	stale := now.Sub(j.fetchedAt) > jwksMaxAge
	if (k == nil || stale) && now.Sub(j.triedAt) >= jwksMinRefresh {
		j.triedAt = now
		keys, err := j.fetch(ctx)
		if err != nil {
			if k != nil { // keep using a known key while Entra is unreachable
				return k, nil
			}
			return nil, fmt.Errorf("signing keys: %w", err)
		}
		j.keys, j.fetchedAt = keys, now
		k = keys[kid]
	}
	if k == nil {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	return k, nil
}

func (j *jwks) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := j.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", j.url, resp.Status)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Use string `json:"use"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, jwksMaxBody)).Decode(&set); err != nil {
		return nil, fmt.Errorf("%s: %w", j.url, err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || k.Kid == "" || len(keys) >= jwksMaxKeys {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 || pub.E < 3 {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s: no RSA signing keys", j.url)
	}
	return keys, nil
}

// entraClaims are the claims the hub uses.
type entraClaims struct {
	Iss               string   `json:"iss"`
	Aud               audience `json:"aud"`
	Tid               string   `json:"tid"`
	Oid               string   `json:"oid"`
	Scp               string   `json:"scp"`
	Nonce             string   `json:"nonce"`
	PreferredUsername string   `json:"preferred_username"`
	UPN               string   `json:"upn"`
	Email             string   `json:"email"`
	Name              string   `json:"name"`
	Exp               *float64 `json:"exp"`
	Nbf               *float64 `json:"nbf"`
	Idtyp             string   `json:"idtyp"` // "app": app-only (optional claim)
	Azp               string   `json:"azp"`   // the calling app (v2)
	Appid             string   `json:"appid"` // the calling app (v1)

	// App is set for an accepted app-only token: the calling app's ID.
	App string `json:"-"`
}

// SignInName is the user's sign-in name (UPN), "" if the token has none.
func (c *entraClaims) SignInName() string {
	for _, s := range []string{c.PreferredUsername, c.UPN, c.Email} {
		if s != "" {
			return s
		}
	}
	return ""
}

// audience is "aud": a string, or (other issuers) a list.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*a = audience{s}
		return nil
	}
	var l []string
	if err := json.Unmarshal(b, &l); err != nil {
		return errors.New("bad aud")
	}
	*a = l
	return nil
}

// tokenCheck is what a token must satisfy besides its signature.
type tokenCheck struct {
	issuer    string
	tenant    string
	audiences []string // any of
	scope     string   // access tokens: required in scp
	nonce     string   // ID tokens: must match
	// apps: app-only tokens are accepted from these app IDs (nil: none),
	// with audience appAud and no scope.
	apps   map[string]string
	appAud string
}

var b64url = base64.RawURLEncoding

// verifyEntraToken checks token's RS256 signature against keys and its
// claims against want. Errors say why, never echo the token.
func verifyEntraToken(ctx context.Context, keys *jwks, token string, want tokenCheck, now time.Time) (*entraClaims, error) {
	if len(token) > maxTokenLen {
		return nil, errors.New("token too long")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a signed JWT")
	}
	hb, err := b64url.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("bad token header")
	}
	var hdr struct {
		Alg  string `json:"alg"`
		Kid  string `json:"kid"`
		Crit any    `json:"crit"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return nil, errors.New("bad token header")
	}
	if hdr.Alg != "RS256" {
		return nil, fmt.Errorf("token algorithm %q not accepted (RS256 only)", hdr.Alg)
	}
	if hdr.Kid == "" || hdr.Crit != nil {
		return nil, errors.New("token header without key ID, or with critical extensions")
	}
	sig, err := b64url.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("bad token signature")
	}
	pub, err := keys.key(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return nil, errors.New("bad token signature")
	}
	payload, err := b64url.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("bad token payload")
	}
	var c entraClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, errors.New("bad token payload")
	}
	switch {
	case c.Iss != want.issuer:
		return nil, fmt.Errorf("token issuer %q, want %q", c.Iss, want.issuer)
	case !strings.EqualFold(c.Tid, want.tenant):
		return nil, fmt.Errorf("token of tenant %q, want %q", c.Tid, want.tenant)
	case !slices.ContainsFunc(c.Aud, func(a string) bool { return slices.Contains(want.audiences, strings.ToLower(a)) }):
		return nil, fmt.Errorf("token audience %q is not this hub's app", strings.Join(c.Aud, ","))
	case c.Exp == nil:
		return nil, errors.New("token without expiry")
	case now.After(time.Unix(int64(*c.Exp), 0).Add(jwtSkew)):
		return nil, errors.New("token expired")
	case c.Nbf != nil && now.Add(jwtSkew).Before(time.Unix(int64(*c.Nbf), 0)):
		return nil, errors.New("token not valid yet")
	case c.Oid == "":
		return nil, errors.New("token without object ID (oid)")
	case want.nonce != "" && !constantEqual(c.Nonce, want.nonce):
		return nil, errors.New("token nonce mismatch")
	}
	// App-only tokens (client credentials) carry no scp; idtyp says so
	// where that optional claim is on. They only count for listed apps.
	if c.Idtyp == "app" || want.scope != "" && strings.TrimSpace(c.Scp) == "" {
		app := strings.ToLower(cmp.Or(c.Azp, c.Appid))
		switch {
		case c.Idtyp != "app" && want.apps[app] == "":
			return nil, fmt.Errorf("token lacks the scope %s", want.scope)
		case want.apps == nil:
			return nil, errors.New("app-only tokens are not accepted here")
		case want.apps[app] == "":
			return nil, fmt.Errorf("app %q may not sign in (auth.entra apps)", app)
		case !slices.ContainsFunc(c.Aud, func(a string) bool { return strings.EqualFold(a, want.appAud) }):
			return nil, fmt.Errorf("app token audience %q, want %q", strings.Join(c.Aud, ","), want.appAud)
		}
		c.App = app
	} else if want.scope != "" && !slices.Contains(strings.Fields(c.Scp), want.scope) {
		return nil, fmt.Errorf("token lacks the scope %s", want.scope)
	}
	c.Oid = strings.ToLower(c.Oid)
	return &c, nil
}
