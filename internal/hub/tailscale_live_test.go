package hub

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestTailscaleLive (opt-in): mints one real auth key with the OAuth client
// whose secret is in $LONGARM_TAILSCALE_OAUTH_SECRET_FILE (ephemeral,
// pre-authorized, one-time, 5-minute expiry, tag $LONGARM_TAILSCALE_TAG or
// tag:longarm-client), checks the answer's shape and deletes the key again.
// Optional: LONGARM_TAILSCALE_TAILNET (default "-"), LONGARM_TAILSCALE_API_URL.
// Nothing secret is logged.
func TestTailscaleLive(t *testing.T) {
	secretFile := os.Getenv("LONGARM_TAILSCALE_OAUTH_SECRET_FILE")
	if secretFile == "" {
		t.Skip("set LONGARM_TAILSCALE_OAUTH_SECRET_FILE to an OAuth client secret (scope auth_keys) to mint a real key")
	}
	cfg := TailscaleConfig{OAuthSecretFile: secretFile, Tailnet: os.Getenv("LONGARM_TAILSCALE_TAILNET"),
		APIURL: os.Getenv("LONGARM_TAILSCALE_API_URL")}
	if cfg.Tailnet == "" {
		cfg.Tailnet = "-"
	}
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.tailscale.com"
	}
	tag := os.Getenv("LONGARM_TAILSCALE_TAG")
	if tag == "" {
		tag = "tag:longarm-client"
	}
	c, err := newTailscale(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	k, err := c.createKey(ctx, mintRequest{tags: []string{tag}, ephemeral: true, ttl: 5 * time.Minute, node: "live-test"})
	if err != nil {
		t.Fatal(err) // already redacted
	}
	defer func() {
		if err := c.deleteKey(ctx, k.ID); err != nil {
			t.Logf("deleting key %s: %v (it expires in 5 minutes anyway)", k.ID, err)
		} else {
			t.Logf("key %s deleted", k.ID)
		}
	}()
	t.Logf("minted key id=%s created=%s expires=%s tags=%v description=%q", k.ID, k.Created.Format(time.RFC3339),
		k.Expires.Format(time.RFC3339), k.Capabilities.Devices.Create.Tags, k.Description)
	if !strings.HasPrefix(k.Key, "tskey-auth-") {
		t.Errorf("key does not look like an auth key (prefix %q)", RedactKeys(k.Key))
	}
	if k.ID == "" {
		t.Error("no key id")
	}
	if left := time.Until(k.Expires); left < 3*time.Minute || left > 7*time.Minute {
		t.Errorf("expires in %s, want ~5m", left)
	}
	cr := k.Capabilities.Devices.Create
	if cr.Reusable || !cr.Ephemeral || !cr.Preauthorized || !slices.Contains(cr.Tags, tag) {
		t.Errorf("capabilities %+v", cr)
	}
}
