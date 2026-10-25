package hub

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/testutil/fakets"
)

func writeSecret(t *testing.T, secret string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ts.secret")
	if err := os.WriteFile(p, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTailscaleClientID(t *testing.T) {
	for in, want := range map[string]string{
		"tskey-client-kAbc123CNTRL-SECRETSECRET": "kAbc123CNTRL",
		"tskey-client-k1-a-b":                    "k1",
		"tskey-auth-k1-xyz":                      "",
		"tskey-client-nodash":                    "",
		"plain":                                  "",
	} {
		if got := clientIDFromSecret(in); got != want {
			t.Errorf("clientIDFromSecret(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTailscaleMint(t *testing.T) {
	secret := fakets.SecretFor("kTest1CNTRL")
	fake := fakets.Start("kTest1CNTRL", secret)
	defer fake.Close()
	cfg := TailscaleConfig{OAuthSecretFile: writeSecret(t, secret), Tailnet: "-", APIURL: fake.URL}

	if _, err := newTailscale(TailscaleConfig{OAuthSecretFile: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing secret file accepted")
	}
	if _, err := newTailscale(TailscaleConfig{OAuthSecretFile: writeSecret(t, "not-a-tskey")}); err == nil || !strings.Contains(err.Error(), "oauth_client_id") {
		t.Fatalf("secret without id and no oauth_client_id: %v", err)
	}
	c, err := newTailscale(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k, err := c.createKey(ctx, mintRequest{tags: []string{"tag:longarm-worker"}, ttl: time.Hour, node: "w1.example_x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Key, "tskey-auth-") || k.ID == "" || time.Until(k.Expires) < 55*time.Minute {
		t.Fatalf("key %+v", k)
	}
	if _, err := c.Mint(ctx, mintRequest{tags: []string{"tag:longarm-client"}, ephemeral: true, ttl: 10 * time.Minute, node: "alice"}); err != nil {
		t.Fatal(err)
	}
	keys := fake.Keys()
	w, cl := keys[0], keys[1]
	if w.Reusable || w.Ephemeral || !w.Preauthorized || w.Tags[0] != "tag:longarm-worker" || w.ExpirySeconds != 3600 ||
		w.Description != "longarm w1-example-x" || w.Tailnet != "-" {
		t.Fatalf("worker key request %+v", w)
	}
	if cl.Reusable || !cl.Ephemeral || !cl.Preauthorized || cl.Tags[0] != "tag:longarm-client" || cl.ExpirySeconds != 600 {
		t.Fatalf("client key request %+v", cl)
	}
	if n := fake.TokenCount(); n != 1 {
		t.Fatalf("access token not cached: %d tokens", n)
	}

	// A revoked token: one retry with a fresh token.
	fake.RevokeTokens()
	if _, err := c.Mint(ctx, mintRequest{tags: []string{"tag:longarm-worker"}, ttl: time.Hour}); err != nil {
		t.Fatalf("after token revocation: %v", err)
	}
	if n := fake.TokenCount(); n != 2 {
		t.Fatalf("tokens %d, want 2", n)
	}
	// A tag the OAuth client may not use: the API's 400, with the secret,
	// the token and keys redacted, and the hint how to set the tags up.
	fake.EchoSecret = true
	_, err = c.Mint(ctx, mintRequest{tags: []string{"tag:other"}, ttl: time.Hour})
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "not permitted") ||
		!strings.Contains(err.Error(), "tags tag:other; ") || !strings.Contains(err.Error(), TagsHint) {
		t.Fatalf("bad tag: %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "tskey-api-fake") || strings.Contains(err.Error(), "tskey-client-kTest1CNTRL-") {
		t.Fatalf("secret leaked: %v", err)
	}
	fake.EchoSecret = false
	// The real-world mistake: a credential tagged with both node tags (and
	// no tagOwners entry for it) can mint keys with both tags, but not with
	// one of them.
	fake.Tags, fake.TagOwners = []string{"tag:longarm-worker", "tag:longarm-client"}, map[string][]string{}
	if _, err := c.Mint(ctx, mintRequest{tags: []string{"tag:longarm-client", "tag:longarm-worker"}, ttl: time.Hour}); err != nil {
		t.Fatalf("exactly the credential's tags: %v", err)
	}
	if _, err := c.Mint(ctx, mintRequest{tags: []string{"tag:longarm-worker"}, ttl: time.Hour}); err == nil || !tagsRejected(errors.Unwrap(err)) ||
		!strings.Contains(err.Error(), "tag:longarm-hub") {
		t.Fatalf("one of the credential's two tags: %v", err)
	}
	fake.Tags, fake.TagOwners = nil, nil
	// Server errors are reported.
	fake.FailNext(http.StatusInternalServerError)
	if _, err := c.Mint(ctx, mintRequest{tags: []string{"tag:longarm-worker"}, ttl: time.Hour}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("injected failure: %v", err)
	}
	// Short-lived tokens are renewed before they expire.
	fake.TokenTTL = 2 * time.Second
	fake.RevokeTokens()
	c.Mint(ctx, mintRequest{tags: []string{"tag:longarm-worker"}, ttl: time.Hour})
	before := fake.TokenCount()
	time.Sleep(2 * time.Second)
	if _, err := c.Mint(ctx, mintRequest{tags: []string{"tag:longarm-worker"}, ttl: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if fake.TokenCount() != before+1 {
		t.Fatalf("expired token not renewed (%d -> %d)", before, fake.TokenCount())
	}

	// Wrong credentials: 401 on the token, secret not in the error.
	bad := writeSecret(t, fakets.SecretFor("kTest1CNTRL"))
	c2, _ := newTailscale(TailscaleConfig{OAuthSecretFile: bad, Tailnet: "-", APIURL: fake.URL})
	if _, err := c2.Mint(ctx, mintRequest{tags: []string{"tag:longarm-worker"}, ttl: time.Hour}); err == nil || !strings.Contains(err.Error(), "401") ||
		strings.Contains(err.Error(), c2.secret) {
		t.Fatalf("bad credentials: %v", err)
	}
	// No auth_keys scope.
	fake.Scopes = []string{"devices:core:read"}
	fake.RevokeTokens()
	if _, err := c.Mint(ctx, mintRequest{tags: []string{"tag:longarm-worker"}, ttl: time.Hour}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("missing scope: %v", err)
	}
	// Another tailnet than the client's.
	fake.Scopes = nil
	c3, _ := newTailscale(TailscaleConfig{OAuthSecretFile: cfg.OAuthSecretFile, Tailnet: "other.org", APIURL: fake.URL})
	if _, err := c3.Mint(ctx, mintRequest{tags: []string{"tag:longarm-worker"}, ttl: time.Hour}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("other tailnet: %v", err)
	}
	// The environment wins over the file.
	t.Setenv(EnvTailscaleSecret, "tskey-client-kEnv-zzz")
	c4, _ := newTailscale(cfg)
	if c4.id != "kEnv" {
		t.Fatalf("env secret: id %q", c4.id)
	}
}

func TestRedactKeys(t *testing.T) {
	in := "key tskey-auth-k123CNTRL-abcdef and hskey-auth-xyz_9 and tskey-client-kA-B, ok"
	got := RedactKeys(in)
	want := "key tskey-auth-[redacted] and hskey-auth-[redacted] and tskey-client-[redacted], ok"
	if got != want {
		t.Fatalf("got %q", got)
	}
	if got := cleanReason("login neededé\x00\n" + strings.Repeat("世", 30) + strings.Repeat("x", 50)); got != "login needed"+strings.Repeat("x", 28) {
		t.Fatalf("cleanReason %q", got)
	}
	if keyDescription(strings.Repeat("n", 80)) != "longarm "+strings.Repeat("n", 42) {
		t.Fatalf("description %q", keyDescription(strings.Repeat("n", 80)))
	}
}

func TestTailnetConfig(t *testing.T) {
	load := func(toml string) (*Config, error) {
		p := filepath.Join(t.TempDir(), "hub.toml")
		os.WriteFile(p, []byte(toml), 0o600)
		return LoadConfig(p)
	}
	t.Setenv(EnvTailscaleSecret, "")
	c, err := load("")
	if err != nil || c.Tailnet.Provider != ProviderNone || c.Tailnet.ControlURL() != "" {
		t.Fatalf("default: %+v %v", c.Tailnet, err)
	}
	// The old [headscale] section means provider headscale.
	c, err = load("[headscale]\nurl = \"https://hs.example.com/\"\napi_key_file = \"k\"\nkey_ttl = \"30m\"\nworker_tags = [\"tag:w\"]\n")
	if err != nil || c.Tailnet.Provider != ProviderHeadscale || c.Tailnet.ControlURL() != "https://hs.example.com" ||
		c.Tailnet.KeyTTL.Duration != 30*time.Minute || c.Tailnet.WorkerTags[0] != "tag:w" || c.Tailnet.ClientTags[0] != "tag:longarm-client" ||
		!*c.Tailnet.EphemeralClients {
		t.Fatalf("legacy headscale: %+v %v", c.Tailnet, err)
	}
	if _, err := load("[headscale]\nurl = \"https://a\"\n[tailnet.headscale]\nurl = \"https://b\"\n"); err == nil {
		t.Fatal("both [headscale] and [tailnet.headscale] accepted")
	}
	c, err = load("[tailnet]\nprovider = \"tailscale\"\nephemeral_clients = false\n[tailnet.tailscale]\noauth_secret_file = \"/etc/longarm-hub/tailscale-oauth.secret\"\n")
	if err != nil || c.Tailnet.ControlURL() != DefaultTailscaleControlURL || c.Tailnet.Tailscale.Tailnet != "-" ||
		c.Tailnet.Tailscale.APIURL != "https://api.tailscale.com" || *c.Tailnet.EphemeralClients || c.Tailnet.KeyTTL.Duration != time.Hour {
		t.Fatalf("tailscale: %+v %v", c.Tailnet, err)
	}
	// An OAuth secret alone selects tailscale; headscale url wins over it.
	c, err = load("[tailnet.tailscale]\noauth_secret_file = \"x\"\ncontrol_url = \"https://ctl.example.com/\"\n")
	if err != nil || c.Tailnet.Provider != ProviderTailscale || c.Tailnet.ControlURL() != "https://ctl.example.com" {
		t.Fatalf("implicit tailscale: %+v %v", c.Tailnet, err)
	}
	c, err = load("[tailnet]\nprovider = \"none\"\n[tailnet.headscale]\nurl = \"https://hs\"\n")
	if err != nil || c.Tailnet.Provider != ProviderNone {
		t.Fatalf("explicit none: %+v %v", c.Tailnet, err)
	}
	for _, bad := range []string{
		"[tailnet]\nprovider = \"tailscale\"\n",
		"[tailnet]\nprovider = \"headscale\"\n",
		"[tailnet]\nprovider = \"zerotier\"\n",
		"[tailnet]\nprovider = \"headscale\"\n[tailnet.headscale]\nurl = \"hs.example.com\"\n",
		"[tailnet]\nprovider = \"none\"\nworker_tags = [\"longarm-worker\"]\n",
		"[tailnet]\nkey_ttl = \"5s\"\n",
	} {
		if _, err := load(bad); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}
