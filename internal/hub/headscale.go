package hub

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Headscale provider (DESIGN §4.2, §4.4): the hub mints one-time, tagged
// pre-auth keys over Headscale's REST API (>= 0.28: tag-only keys need no
// user; with [tailnet.headscale] user set, the key also carries that user,
// created if missing). The Headscale policy decides who may talk to whom
// (deploy/headscale).

type headscaleClient struct {
	cfg  HeadscaleConfig
	base string
	key  string
	http *http.Client

	mu     sync.Mutex
	userID string
}

func newHeadscale(cfg HeadscaleConfig) (*headscaleClient, error) {
	if cfg.URL == "" {
		return nil, nil
	}
	key := strings.TrimSpace(os.Getenv("LONGARM_HUB_HEADSCALE_API_KEY"))
	if key == "" && cfg.APIKeyFile != "" {
		b, err := os.ReadFile(cfg.APIKeyFile)
		if err != nil {
			return nil, fmt.Errorf("[tailnet.headscale] api_key_file: %w", err)
		}
		key = strings.TrimSpace(string(b))
	}
	if key == "" {
		return nil, errors.New("[tailnet.headscale] needs an API key (api_key_file or LONGARM_HUB_HEADSCALE_API_KEY)")
	}
	base := strings.TrimSuffix(cmp.Or(cfg.APIURL, cfg.URL), "/")
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("[tailnet.headscale] url: %w", err)
	}
	return &headscaleClient{cfg: cfg, base: base, key: key, http: &http.Client{Timeout: 20 * time.Second}}, nil
}

func (c *headscaleClient) Name() string { return ProviderHeadscale }

func (c *headscaleClient) redact(s string) string {
	if len(c.key) >= 8 {
		s = strings.ReplaceAll(s, c.key, "[redacted]")
	}
	return RedactKeys(s)
}

func (c *headscaleClient) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("headscale: %s", c.redact(err.Error()))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("headscale %s %s: %s: %s", method, path, resp.Status, c.redact(msg))
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("headscale %s %s: %w", method, path, err)
		}
	}
	return nil
}

type hsUser struct {
	ID   json.Number `json:"id"` // uint64, sent as a string by grpc-gateway
	Name string      `json:"name"`
}

// user resolves (creating if needed) the configured user's id.
func (c *headscaleClient) user(ctx context.Context) (string, error) {
	if c.cfg.User == "" {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.userID != "" {
		return c.userID, nil
	}
	var list struct {
		Users []hsUser `json:"users"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/user?name="+url.QueryEscape(c.cfg.User), nil, &list); err != nil {
		return "", err
	}
	for _, u := range list.Users {
		if u.Name == c.cfg.User {
			c.userID = u.ID.String()
			return c.userID, nil
		}
	}
	var created struct {
		User hsUser `json:"user"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/user", map[string]string{"name": c.cfg.User}, &created); err != nil {
		return "", err
	}
	c.userID = created.User.ID.String()
	return c.userID, nil
}

// Mint creates a one-time pre-auth key carrying tags.
func (c *headscaleClient) Mint(ctx context.Context, req mintRequest) (string, error) {
	body := map[string]any{
		"reusable":   false,
		"ephemeral":  req.ephemeral,
		"expiration": time.Now().Add(req.ttl).UTC().Format(time.RFC3339),
		"aclTags":    req.tags,
	}
	uid, err := c.user(ctx)
	if err != nil {
		return "", err
	}
	if uid != "" {
		body["user"] = uid
	}
	var out struct {
		PreAuthKey struct {
			Key string `json:"key"`
		} `json:"preAuthKey"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v1/preauthkey", body, &out); err != nil {
		return "", err
	}
	if out.PreAuthKey.Key == "" {
		return "", errors.New("headscale returned no key")
	}
	return out.PreAuthKey.Key, nil
}
