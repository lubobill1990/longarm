package e2e

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestHubEntraLogin: `longarm login` with a fake Azure CLI against a hub
// whose Entra authority is a fake (JWKS): a user's first sign-in creates
// the user and enrolls the device; a service principal (CI) signs in as
// its configured user; a missing az login tells the user what to run.
func TestHubEntraLogin(t *testing.T) {
	const (
		tenant = "11111111-2222-3333-4444-555555555555"
		client = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		ciApp  = "c1c1c1c1-0000-4000-8000-000000000001"
	)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding
	mux := http.NewServeMux()
	mux.HandleFunc("GET /"+tenant+"/discovery/v2.0/keys", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "use": "sig", "kid": "k1",
			"n": b64.EncodeToString(key.N.Bytes()), "e": b64.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	})
	entra := httptest.NewServer(mux)
	defer entra.Close()
	sign := func(claims map[string]any) string {
		now := time.Now().Unix()
		for k, v := range map[string]any{"aud": client, "iss": entra.URL + "/" + tenant + "/v2.0", "tid": tenant, "iat": now, "nbf": now, "exp": now + 3600} {
			claims[k] = v
		}
		h, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "k1"})
		c, _ := json.Marshal(claims)
		in := b64.EncodeToString(h) + "." + b64.EncodeToString(c)
		sum := sha256.Sum256([]byte(in))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		return in + "." + b64.EncodeToString(sig)
	}

	h := newHubEnv(t, fmt.Sprintf(`[auth.entra]
tenant_id = %q
client_id = %q
authority = %q
admins = ["boss@example.com"]
apps = ["%s=ci"]
`, tenant, client, entra.URL, ciApp))

	// the fake az: account show prints type.txt, get-access-token token.txt
	// (or fail.txt to stderr, exit 1)
	azDir := t.TempDir()
	azPath, script := filepath.Join(azDir, "az"), "#!/bin/sh\nd=$(dirname \"$0\")\n"+
		"if [ \"$2\" = show ]; then cat \"$d/type.txt\"; exit 0; fi\n"+
		"if [ -f \"$d/fail.txt\" ]; then cat \"$d/fail.txt\" >&2; exit 1; fi\ncat \"$d/token.txt\"\n"
	if runtime.GOOS == "windows" {
		azPath, script = filepath.Join(azDir, "az.cmd"), "@echo off\r\n"+
			"if \"%2\"==\"show\" (type \"%~dp0type.txt\" & exit /b 0)\r\n"+
			"if exist \"%~dp0fail.txt\" (type \"%~dp0fail.txt\" 1>&2 & exit /b 1)\r\n"+
			"type \"%~dp0token.txt\"\r\n"
	}
	set := func(name, content string) {
		if err := os.WriteFile(filepath.Join(azDir, name), []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(azPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	azEnv := []string{"LONGARM_AZ=" + azPath}

	// a user
	userTok := sign(map[string]any{"oid": "eeeeeeee-0000-4000-8000-000000000001", "scp": "longarm.connect",
		"preferred_username": "erin@example.com", "azp": "04b07795-8ddb-461a-bbee-02f9e1bf7b46"})
	set("type.txt", "user")
	set("token.txt", userTok)
	erin := filepath.Join(h.root, "erin")
	r := h.longarmWith(azEnv, erin, "login", h.addr(), "--ca-fingerprint", h.fp)
	if r.code != 0 || !strings.Contains(r.stdout, "signed in to") || !strings.Contains(r.stdout, "client/erin-") {
		t.Fatalf("longarm login: %+v", r)
	}
	if r := h.longarmWith(nil, erin, "whoami"); r.code != 0 || !strings.Contains(r.stdout, "user erin ") {
		t.Fatalf("whoami: %+v", r)
	}
	// again on the same machine: no --force needed, same device
	if r := h.longarmWith(azEnv, erin, "login", h.addr(), "--ca-fingerprint", h.fp); r.code != 0 || !strings.Contains(r.stdout, "client/erin-") {
		t.Fatalf("second login: %+v", r)
	}

	// CI: a service principal's app-only token
	set("type.txt", "servicePrincipal")
	set("token.txt", sign(map[string]any{"oid": "5e5e5e5e-0000-4000-8000-000000000003", "idtyp": "app", "azp": ciApp}))
	ci := filepath.Join(h.root, "ci")
	if r := h.longarmWith(azEnv, ci, "login", h.addr(), "--ca-fingerprint", h.fp, "--name", "runner-1"); r.code != 0 || !strings.Contains(r.stdout, "client/runner-1") {
		t.Fatalf("service principal login: %+v", r)
	}
	if r := h.longarmWith(nil, ci, "whoami"); r.code != 0 || !strings.Contains(r.stdout, "user ci ") {
		t.Fatalf("ci whoami: %+v", r)
	}

	// not signed in to az
	set("type.txt", "user")
	set("fail.txt", "ERROR: AADSTS70043: The refresh token has expired. Please run 'az login' to setup account.")
	r = h.longarmWith(azEnv, filepath.Join(h.root, "frank"), "login", h.addr(), "--ca-fingerprint", h.fp)
	if want := "az login --tenant " + tenant + " --scope api://" + client + "/longarm.connect"; r.code == 0 || !strings.Contains(r.stderr, want) {
		t.Fatalf("not signed in: %+v, want %q", r, want)
	}
	// the token never shows up in output or the hub's log
	logb, _ := os.ReadFile(filepath.Join(h.root, "hub.log"))
	if strings.Contains(string(logb), userTok) {
		t.Fatal("the access token is in the hub log")
	}
	if !strings.Contains(string(logb), "node joined by entra sign-in") {
		t.Fatalf("hub log without the sign-in:\n%s", logb)
	}
}
