package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Tailscale provider: the hub holds an OAuth client of the official
// Tailscale service (scope auth_keys, allowed to use tag:longarm-worker and
// tag:longarm-client) and mints one-time, pre-authorized, tagged auth keys
// through the API, so new machines need no tailscaled install and no login:
// longarmd / longarm embed tsnet and enrol with the key.
//
// The OAuth client has no devices scope: the hub can't delete devices.
// longarmd leave logs its node out; ephemeral client nodes vanish by themselves;
// stale worker devices are removed in the admin console.

type tailscaleClient struct {
	cfg    TailscaleConfig
	id     string
	secret string
	http   *http.Client

	fetchMu sync.Mutex // one token request at a time
	mu      sync.Mutex // token, expires
	token   string
	expires time.Time
}

func newTailscale(cfg TailscaleConfig) (*tailscaleClient, error) {
	secret := strings.TrimSpace(os.Getenv(EnvTailscaleSecret))
	if secret == "" && cfg.OAuthSecretFile != "" {
		b, err := os.ReadFile(cfg.OAuthSecretFile)
		if err != nil {
			return nil, fmt.Errorf("[tailnet.tailscale] oauth_secret_file: %w", err)
		}
		secret = strings.TrimSpace(string(b))
	}
	if secret == "" {
		return nil, errors.New("[tailnet.tailscale]: the OAuth client secret is empty")
	}
	id := cfg.OAuthClientID
	if id == "" {
		id = clientIDFromSecret(secret)
		if id == "" {
			return nil, errors.New("[tailnet.tailscale] oauth_client_id is needed: the secret is not of the form tskey-client-<id>-...")
		}
	}
	return &tailscaleClient{cfg: cfg, id: id, secret: secret, http: &http.Client{Timeout: 20 * time.Second}}, nil
}

// clientIDFromSecret: OAuth client secrets look like
// tskey-client-<client id>-<random>.
func clientIDFromSecret(s string) string {
	rest, ok := strings.CutPrefix(s, "tskey-client-")
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, "-")
	if !ok {
		return ""
	}
	return id
}

func (c *tailscaleClient) Name() string { return ProviderTailscale }

// accessToken returns a cached OAuth access token, fetching a new one
// (client-credentials grant) shortly before the old one expires.
func (c *tailscaleClient) accessToken(ctx context.Context) (string, error) {
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	c.mu.Lock()
	tok, exp := c.token, c.expires
	c.mu.Unlock()
	if tok != "" && time.Now().Before(exp) {
		return tok, nil
	}
	form := url.Values{"client_id": {c.id}, "client_secret": {c.secret}, "grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.APIURL+"/api/v2/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := c.send(req, "oauth token", &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("tailscale oauth token: no access_token in the answer")
	}
	life := time.Duration(out.ExpiresIn) * time.Second
	if life <= 0 {
		life = time.Hour
	}
	// Renew a minute early (a tenth for very short lifetimes).
	c.mu.Lock()
	c.token, c.expires = out.AccessToken, time.Now().Add(life-min(time.Minute, life/10))
	c.mu.Unlock()
	return out.AccessToken, nil
}

// apiError is a non-2xx answer of the Tailscale API.
type apiError struct {
	op     string
	status int
	msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("tailscale %s: %d %s: %s", e.op, e.status, http.StatusText(e.status), e.msg)
}

func (c *tailscaleClient) send(req *http.Request, op string, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tailscale %s: %s", op, c.redact(err.Error()))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(b))
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			msg = e.Message
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &apiError{op: op, status: resp.StatusCode, msg: c.redact(msg)}
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("tailscale %s: bad answer: %v", op, err)
	}
	return nil
}

// tsKey is the API's answer to a key creation (the parts we look at).
type tsKey struct {
	ID           string    `json:"id"`
	Key          string    `json:"key"`
	Created      time.Time `json:"created"`
	Expires      time.Time `json:"expires"`
	Description  string    `json:"description"`
	Capabilities struct {
		Devices struct {
			Create struct {
				Reusable      bool     `json:"reusable"`
				Ephemeral     bool     `json:"ephemeral"`
				Preauthorized bool     `json:"preauthorized"`
				Tags          []string `json:"tags"`
			} `json:"create"`
		} `json:"devices"`
	} `json:"capabilities"`
}

// TagsHint explains Tailscale's "requested tags are invalid or not
// permitted": a trust credential (OAuth client) may only mint keys that
// carry exactly its own tags, or tags owned by them.
const TagsHint = "the Tailscale trust credential (OAuth client) may only mint keys with exactly its own tags or tags its tags own: " +
	"give it only tag:longarm-hub (scope auth_keys: write) and list tag:longarm-hub among the owners of the worker and client tags " +
	"in the policy's tagOwners; see deploy/hub/README.md, \"Tailnet provider\""

// tagsRejected reports whether err is Tailscale's answer to tags the
// credential may not put on a key.
func tagsRejected(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.status == http.StatusBadRequest || ae.status == http.StatusForbidden) &&
		strings.Contains(ae.msg, "requested tags")
}

// createKey mints a one-time, pre-authorized auth key carrying tags.
func (c *tailscaleClient) createKey(ctx context.Context, req mintRequest) (*tsKey, error) {
	body := map[string]any{
		"capabilities": map[string]any{"devices": map[string]any{"create": map[string]any{
			"reusable": false, "ephemeral": req.ephemeral, "preauthorized": true, "tags": req.tags,
		}}},
		"expirySeconds": int64(req.ttl / time.Second),
		"description":   keyDescription(req.node),
	}
	b, _ := json.Marshal(body)
	for attempt := 0; ; attempt++ {
		tok, err := c.accessToken(ctx)
		if err != nil {
			return nil, err
		}
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.cfg.APIURL+"/api/v2/tailnet/"+url.PathEscape(c.cfg.Tailnet)+"/keys", bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		hreq.Header.Set("Authorization", "Bearer "+tok)
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("Accept", "application/json")
		var k tsKey
		err = c.send(hreq, "create key", &k)
		var ae *apiError
		if errors.As(err, &ae) && ae.status == http.StatusUnauthorized && attempt == 0 {
			// The token was revoked or expired early: get a new one, once.
			c.mu.Lock()
			c.token = ""
			c.mu.Unlock()
			continue
		}
		if tagsRejected(err) {
			return nil, fmt.Errorf("%w (tags %s; %s)", err, strings.Join(req.tags, ","), TagsHint)
		}
		if err != nil {
			return nil, err
		}
		if k.Key == "" {
			return nil, errors.New("tailscale create key: no key in the answer")
		}
		return &k, nil
	}
}

func (c *tailscaleClient) Mint(ctx context.Context, req mintRequest) (string, error) {
	k, err := c.createKey(ctx, req)
	if err != nil {
		return "", err
	}
	return k.Key, nil
}

// deleteKey revokes an (unused) key (the live test cleans up with it; the
// auth_keys scope allows it).
func (c *tailscaleClient) deleteKey(ctx context.Context, id string) error {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.cfg.APIURL+"/api/v2/tailnet/"+url.PathEscape(c.cfg.Tailnet)+"/keys/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	hreq.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http.Do(hreq)
	if err != nil {
		return fmt.Errorf("tailscale delete key: %s", c.redact(err.Error()))
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("tailscale delete key: %s", resp.Status)
	}
	return nil
}

// redact removes the client secret, the access token and anything that
// looks like a Tailscale key from s.
func (c *tailscaleClient) redact(s string) string {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	for _, v := range []string{c.secret, tok} {
		if len(v) >= 8 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return RedactKeys(s)
}

var keyRE = regexp.MustCompile(`(tskey|hskey)-[A-Za-z0-9_-]+`)

// RedactKeys hides Tailscale / Headscale keys and secrets
// (tskey-..., hskey-...) in s, keeping their kind: "tskey-auth-[redacted]".
func RedactKeys(s string) string {
	return keyRE.ReplaceAllStringFunc(s, func(k string) string {
		parts := strings.SplitN(k, "-", 3)
		if len(parts) == 3 {
			return parts[0] + "-" + parts[1] + "-[redacted]"
		}
		return parts[0] + "-[redacted]"
	})
}

// keyDescription: Tailscale allows up to 50 alphanumerics, spaces and
// hyphens.
func keyDescription(node string) string {
	var b strings.Builder
	b.WriteString("longarm ")
	for _, r := range node {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := b.String()
	if len(s) > 50 {
		s = s[:50]
	}
	return strings.TrimSpace(s)
}
