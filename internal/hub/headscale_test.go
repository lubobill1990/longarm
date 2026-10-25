package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHeadscale implements the parts of Headscale's REST API the hub uses.
type fakeHeadscale struct {
	mu    sync.Mutex
	users map[string]string // name -> id
	keys  []map[string]any  // request bodies of created keys
	fail  bool
}

func (f *fakeHeadscale) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer test-api-key" {
		http.Error(w, `{"code":16,"message":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if f.fail {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
		var users []map[string]string
		if id, ok := f.users[r.URL.Query().Get("name")]; ok {
			users = append(users, map[string]string{"id": id, "name": r.URL.Query().Get("name")})
		}
		json.NewEncoder(w).Encode(map[string]any{"users": users})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/user":
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		id := "7"
		f.users[b["name"]] = id
		json.NewEncoder(w).Encode(map[string]any{"user": map[string]string{"id": id, "name": b["name"]}})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/preauthkey":
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		f.keys = append(f.keys, b)
		json.NewEncoder(w).Encode(map[string]any{"preAuthKey": map[string]any{"key": "hskey-auth-" + strings.Repeat("x", 8), "aclTags": b["aclTags"]}})
	default:
		http.NotFound(w, r)
	}
}

func TestHeadscaleMint(t *testing.T) {
	fake := &fakeHeadscale{users: map[string]string{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	keyFile := filepath.Join(t.TempDir(), "hs.key")
	os.WriteFile(keyFile, []byte("test-api-key\n"), 0o600)

	if c, err := newHeadscale(HeadscaleConfig{}); c != nil || err != nil {
		t.Fatal("no url must mean no client")
	}
	if _, err := newHeadscale(HeadscaleConfig{URL: srv.URL}); err == nil {
		t.Fatal("missing api key accepted")
	}
	// Tag-only keys (Headscale >= 0.28): no user.
	c, err := newHeadscale(HeadscaleConfig{URL: "https://hs.example.com", APIURL: srv.URL, APIKeyFile: keyFile})
	if err != nil {
		t.Fatal(err)
	}
	key, err := c.Mint(context.Background(), mintRequest{tags: []string{"tag:longarm-worker"}, ttl: time.Hour})
	if err != nil || !strings.HasPrefix(key, "hskey-auth-") {
		t.Fatalf("mint: %q %v", key, err)
	}
	b := fake.keys[0]
	if _, has := b["user"]; has || b["reusable"] != false || b["ephemeral"] != false || b["aclTags"].([]any)[0] != "tag:longarm-worker" {
		t.Fatalf("key request %v", b)
	}
	exp, err := time.Parse(time.RFC3339, b["expiration"].(string))
	if err != nil || time.Until(exp) < 50*time.Minute || time.Until(exp) > 70*time.Minute {
		t.Fatalf("expiration %v %v", b["expiration"], err)
	}
	// With a user: created once, then reused (by id).
	c, _ = newHeadscale(HeadscaleConfig{URL: srv.URL, APIKeyFile: keyFile, User: "longarm"})
	for i := 0; i < 2; i++ {
		if _, err := c.Mint(context.Background(), mintRequest{tags: []string{"tag:dev"}, ephemeral: true, ttl: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	if len(fake.users) != 1 || fake.keys[2]["user"] != "7" || fake.keys[2]["ephemeral"] != true || fake.keys[2]["aclTags"].([]any)[0] != "tag:dev" {
		t.Fatalf("users %v, key %v", fake.users, fake.keys[2])
	}
	// Errors carry Headscale's answer.
	fake.fail = true
	if _, err := c.Mint(context.Background(), mintRequest{tags: []string{"tag:dev"}, ttl: time.Hour}); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("failure: %v", err)
	}
	os.Setenv("LONGARM_HUB_HEADSCALE_API_KEY", "wrong")
	defer os.Unsetenv("LONGARM_HUB_HEADSCALE_API_KEY")
	fake.fail = false
	c, _ = newHeadscale(HeadscaleConfig{URL: srv.URL, APIKeyFile: keyFile})
	if _, err := c.Mint(context.Background(), mintRequest{tags: []string{"tag:x"}, ttl: time.Hour}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("env key must win (and be wrong here): %v", err)
	}
	if TailnetHostname("worker", "w1") != "longarm-w1" || TailnetHostname("client", "alice") != "longarm-client-alice" {
		t.Fatal("hostnames")
	}
}
