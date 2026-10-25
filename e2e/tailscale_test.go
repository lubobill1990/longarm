package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/testutil/fakets"
)

// TestHubTailscaleProvider: [tailnet] provider = "tailscale" against a fake
// Tailscale API (OAuth token + key creation, checking the request body and
// the tags the OAuth client may use). longarmd join and longarm join get one-time,
// pre-authorized, tagged keys (workers never ephemeral, clients ephemeral)
// written next to the per-control tsnet state dir; longarm admin tailnet status
// shows the provider and each node; longarm admin tailnet rekey makes the online
// worker enrol again; switching the hub to Headscale and back moves the
// worker (and the client, on its next hub contact) to the other control
// server's state dir, minting only where no key or state exists; provider
// "none" stops the leg. The OAuth secret, access tokens and minted keys
// never show up in any log or output.
func TestHubTailscaleProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("slow; skipped with -short")
	}
	t.Parallel()
	secret := fakets.SecretFor("kE2eTestCNTRL")
	fake := fakets.Start("kE2eTestCNTRL", secret)
	defer fake.Close()
	hs := &fakeHeadscaleAPI{}
	hsAPI := httptest.NewServer(hs)
	defer hsAPI.Close()
	root := t.TempDir()
	secretFile := filepath.Join(root, "tailscale-oauth.secret")
	os.WriteFile(secretFile, []byte(secret+"\n"), 0o600)
	hsKeyFile := filepath.Join(root, "hs-api.key")
	os.WriteFile(hsKeyFile, []byte("fake-api-key"), 0o600)

	// Control servers nobody answers: tsnet never comes up, so minted keys
	// stay in their files (where the test can see them).
	const tsControl, hsControl = "http://127.0.0.1:1", "http://127.0.0.1:2"
	tailscaleCfg := fmt.Sprintf("[tailnet]\nprovider = \"tailscale\"\nkey_ttl = \"30m\"\n[tailnet.tailscale]\noauth_secret_file = %q\napi_url = %q\ncontrol_url = %q\n",
		filepath.ToSlash(secretFile), fake.URL, tsControl)
	headscaleCfg := fmt.Sprintf("[tailnet]\nprovider = \"headscale\"\n[tailnet.headscale]\nurl = %q\napi_url = %q\napi_key_file = %q\n",
		hsControl, hsAPI.URL, filepath.ToSlash(hsKeyFile))
	h := newHubEnv(t, tailscaleCfg)
	baseCfg, _ := os.ReadFile(h.cfgPath)
	base := strings.TrimSuffix(string(baseCfg), tailscaleCfg)
	var outputs []string // every longarm/longarmd output, checked for secrets at the end
	longarm := func(dir string, args ...string) string {
		t.Helper()
		r := h.longarm(dir, args...)
		outputs = append(outputs, r.stdout, r.stderr)
		if r.code != 0 {
			t.Fatalf("longarm %v: %+v", args, r)
		}
		return r.stdout + r.stderr
	}

	wa := h.newWorkerDir(t, "wa", "")
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	tsKey := filepath.Join(wa.dataDir, "tsnet-127.0.0.1-1.authkey")
	if b, err := os.ReadFile(tsKey); err != nil || !strings.HasPrefix(string(b), "tskey-auth-") {
		t.Fatalf("worker key file: %q %v", b, err)
	}
	hubToml, _ := os.ReadFile(filepath.Join(wa.dataDir, "hub.toml"))
	for _, want := range []string{`provider = "tailscale"`, `control_url = "` + tsControl + `"`, `hostname = "longarm-wa"`} {
		if !strings.Contains(string(hubToml), want) {
			t.Fatalf("worker hub.toml lacks %s:\n%s", want, hubToml)
		}
	}
	keys := fake.Keys()
	if len(keys) != 1 {
		t.Fatalf("keys minted: %d", len(keys))
	}
	if k := keys[0]; k.Reusable || k.Ephemeral || !k.Preauthorized || len(k.Tags) != 1 || k.Tags[0] != "tag:longarm-worker" ||
		k.ExpirySeconds != 1800 || k.Description != "longarm wa" || k.Tailnet != "-" {
		t.Fatalf("worker key request %+v", k)
	}
	// The tailscale leg can't come up; tcp serves anyway.
	if r := h.longarm(h.client, "exec", "wa", "--", bins.helper, "print", "1", "0", "3"); r.code != 3 {
		t.Fatalf("exec while the tailnet is down: %+v", r)
	}

	// A client: ephemeral tag:longarm-client key.
	code := strings.TrimSpace(h.mustLongarm(h.admin, "admin", "join-code", "--role", "client"))
	c2 := filepath.Join(h.root, "c2")
	out := longarm(c2, "join", h.addr(), code, "--ca-fingerprint", h.fp, "--name", "bob")
	if !strings.Contains(out, "tailnet "+tsControl+": key for longarm-client-bob (tag:longarm-client)") {
		t.Fatalf("longarm join:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(c2, "tsnet-127.0.0.1-1.authkey")); !strings.HasPrefix(string(b), "tskey-auth-") {
		t.Fatalf("client key %q", b)
	}
	if k := fake.Keys()[1]; !k.Ephemeral || k.Tags[0] != "tag:longarm-client" || k.Reusable || !k.Preauthorized {
		t.Fatalf("client key request %+v", k)
	}

	// longarm admin tailnet status.
	status := func() map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(longarm(h.admin, "admin", "tailnet", "status", "--json")), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	nodeStatus := func(m map[string]any, name string) map[string]any {
		for _, n := range m["nodes"].([]any) {
			if n := n.(map[string]any); n["name"] == name {
				return n
			}
		}
		return nil
	}
	var st map[string]any
	waitFor(t, "wa's tailnet state at the hub", 20*time.Second, func() bool {
		st = status()
		n := nodeStatus(st, "wa")
		s, _ := n["state"].(map[string]any)
		return s != nil && s["control_url"] == tsControl && s["hostname"] == "longarm-wa"
	})
	if st["provider"] != "tailscale" || st["control_url"] != tsControl || st["tailnet"] != "-" || st["ephemeral_clients"] != true {
		t.Fatalf("status %v", st)
	}
	if n := nodeStatus(st, "wa"); n["key_reason"] != "enrol" || n["key_control_url"] != tsControl {
		t.Fatalf("wa status %v", n)
	}
	if n := nodeStatus(st, "bob"); n == nil || n["key_reason"] != "enrol" || n["role"] != "client" {
		t.Fatalf("bob status %v", n)
	}
	table := longarm(h.admin, "admin", "tailnet", "status")
	if !strings.Contains(table, "provider  tailscale") || !strings.Contains(table, "wa ") || !strings.Contains(table, "(enrol)") {
		t.Fatalf("status table:\n%s", table)
	}

	// Re-key the online worker: it drops its key/state and gets a new one.
	// The old key is read before the re-key: the worker may have its new
	// key already when longarm admin returns (the hub sends the command before
	// it answers), and comparing against that one waited forever.
	old, _ := os.ReadFile(tsKey)
	out = longarm(h.admin, "admin", "tailnet", "rekey", "wa")
	if !strings.Contains(out, "re-enrols now") {
		t.Fatalf("rekey: %s", out)
	}
	waitFor(t, "re-keyed worker", 30*time.Second, func() bool {
		b, _ := os.ReadFile(tsKey)
		n := nodeStatus(status(), "wa")
		return len(fake.Keys()) == 3 && len(b) > 0 && string(b) != string(old) && n["key_reason"] == "rekey" && n["rekey_pending"] != true
	})
	if k := fake.Keys()[2]; k.Ephemeral || k.Tags[0] != "tag:longarm-worker" {
		t.Fatalf("re-key request %+v", k)
	}

	// The real-world credential mistake (tagged with both node tags):
	// Tailscale refuses a key with only tag:longarm-worker. The status shows the
	// error and how to set the tags up.
	fake.SetTags([]string{"tag:longarm-worker", "tag:longarm-client"}, map[string][]string{})
	longarm(h.admin, "admin", "tailnet", "rekey", "wa")
	waitFor(t, "key error in the status", 30*time.Second, func() bool {
		return strings.Contains(fmt.Sprint(status()["last_mint_error"]), "requested tags")
	})
	table = longarm(h.admin, "admin", "tailnet", "status")
	for _, want := range []string{"key error", "for wa", "400", "requested tags [tag:longarm-worker] are invalid or not permitted",
		`"tag:longarm-hub": ["autogroup:admin"]`, "deploy/hub/README.md"} {
		if !strings.Contains(table, want) {
			t.Fatalf("status table lacks %q:\n%s", want, table)
		}
	}
	t.Logf("status with the tags error:\n%s", table)
	hubLog, _ := os.ReadFile(filepath.Join(h.root, "hub.log"))
	if !strings.Contains(string(hubLog), "tag:longarm-hub") {
		t.Fatalf("hub log without the tags hint:\n%s", tail(string(hubLog), 3000))
	}
	// Fixed credential: the re-key (asked for again) goes through, the error
	// is gone.
	fake.SetTags(nil, nil)
	longarm(h.admin, "admin", "tailnet", "rekey", "wa")
	waitFor(t, "re-key after the credential fix", 90*time.Second, func() bool {
		st := status()
		n := nodeStatus(st, "wa")
		return st["last_mint_error"] == nil && n["rekey_pending"] != true && len(fake.Keys()) == 4
	})

	// The hub switches to Headscale: the worker follows on its next session.
	restartHub := func(cfg string) {
		t.Helper()
		h.killHub()
		os.WriteFile(h.cfgPath, []byte(base+cfg), 0o600)
		h.startHub(t)
	}
	restartHub(headscaleCfg)
	hsKey := filepath.Join(wa.dataDir, "tsnet-127.0.0.1-2.authkey")
	waitFor(t, "worker on the headscale tailnet", 45*time.Second, func() bool {
		b, _ := os.ReadFile(hsKey)
		hubToml, _ := os.ReadFile(filepath.Join(wa.dataDir, "hub.toml"))
		return strings.HasPrefix(string(b), "hskey-auth-fake") && strings.Contains(string(hubToml), `control_url = "`+hsControl+`"`) &&
			strings.Contains(string(hubToml), `provider = "headscale"`)
	})
	if hs.tags[0][0] != "tag:longarm-worker" {
		t.Fatalf("headscale key tags %v", hs.tags[0])
	}
	if _, err := os.Stat(tsKey); err != nil {
		t.Fatal("the tailscale key must stay for switching back")
	}
	st = status()
	if st["provider"] != "headscale" || st["control_url"] != hsControl {
		t.Fatalf("status after the switch %v", st)
	}
	// The client follows on its next hub contact (lazily: no key yet).
	out = longarm(c2, "ls")
	if !strings.Contains(out, "the hub's tailnet is now headscale ("+hsControl+")") {
		t.Fatalf("longarm ls after the switch:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(c2, "hub.toml")); !strings.Contains(string(b), `control_url = "`+hsControl+`"`) ||
		!strings.Contains(string(b), `hostname = "longarm-client-bob"`) {
		t.Fatalf("client hub.toml:\n%s", b)
	}

	// An admin re-key of a client applies on its next hub contact.
	if out := longarm(h.admin, "admin", "tailnet", "rekey", "bob"); !strings.Contains(out, "on its next hub contact") {
		t.Fatalf("rekey bob: %s", out)
	}
	if out := longarm(c2, "ls"); !strings.Contains(out, "tailnet re-keyed") {
		t.Fatalf("longarm ls after the client re-key:\n%s", out)
	}
	c2Key := filepath.Join(c2, "tsnet-127.0.0.1-2.authkey")
	if b, _ := os.ReadFile(c2Key); !strings.HasPrefix(string(b), "hskey-auth-fake") {
		t.Fatalf("client key after the re-key: %q", b)
	}
	if n := nodeStatus(status(), "bob"); n["key_reason"] != "rekey" || n["rekey_pending"] == true || n["key_control_url"] != hsControl {
		t.Fatalf("bob after the re-key %v", n)
	}
	// A client without key or state: its embedded node asks the hub when a
	// tailscale leg needs it (here a worker only reachable over tailscale).
	os.Remove(c2Key)
	os.WriteFile(filepath.Join(c2, "workers.toml"), []byte("[[worker]]\nname = \"wt\"\ntransport = \"tailscale\"\naddress = \"100.64.0.9:7701\"\n"), 0o600)
	before := hs.count()
	r := longarmEnv(t, c2, []string{"LONGARM_CONNECT_TIMEOUT=4s"}, "--transport", "tailscale", "info", "wt")
	outputs = append(outputs, r.stdout, r.stderr)
	if r.code == 0 {
		t.Fatalf("longarm info over an unreachable tailnet: %+v", r)
	}
	if hs.count() != before+1 {
		t.Fatalf("the client's node fetched %d keys", hs.count()-before)
	}
	if b, _ := os.ReadFile(c2Key); !strings.HasPrefix(string(b), "hskey-auth-fake") {
		t.Fatalf("fetched key not kept for the next try: %q", b)
	}
	if n := nodeStatus(status(), "bob"); n["key_reason"] != "no state" {
		t.Fatalf("bob after the on-demand key %v", n)
	}
	os.Remove(filepath.Join(c2, "workers.toml"))

	// And back: the worker's tailscale key is still there, no new mint.
	nTS := len(fake.Keys())
	restartHub(tailscaleCfg)
	waitFor(t, "worker back on tailscale", 45*time.Second, func() bool {
		hubToml, _ := os.ReadFile(filepath.Join(wa.dataDir, "hub.toml"))
		return strings.Contains(string(hubToml), `control_url = "`+tsControl+`"`)
	})
	h.waitNode(t, "wa", "tailnet state on tailscale", func(n map[string]any) bool {
		s, _ := n["tailnet"].(map[string]any)
		return s != nil && s["control_url"] == tsControl
	})
	time.Sleep(2 * time.Second)
	if n := len(fake.Keys()); n != nTS {
		t.Fatalf("switching back minted %d new keys", n-nTS)
	}

	// Provider none: the leg stops, hub.toml forgets the tailnet.
	restartHub("")
	waitFor(t, "tailnet gone from the worker", 45*time.Second, func() bool {
		hubToml, _ := os.ReadFile(filepath.Join(wa.dataDir, "hub.toml"))
		return !strings.Contains(string(hubToml), "control_url")
	})
	if r := h.longarm(h.client, "exec", "wa", "--", bins.helper, "print", "1", "0", "4"); r.code != 4 {
		t.Fatalf("exec after the tailnet is gone: %+v", r)
	}
	if st := status(); st["provider"] != "none" {
		t.Fatalf("status without a provider: %v", st)
	}

	// Secrets: never in a log or an output.
	logs := append([]string(nil), outputs...)
	for _, p := range []string{filepath.Join(h.root, "hub.log"), wa.logPath} {
		b, _ := os.ReadFile(p)
		logs = append(logs, string(b))
	}
	secrets := []string{secret, "tskey-api-fake", "fake-api-key"}
	for _, k := range fake.Keys() {
		secrets = append(secrets, k.Key)
	}
	for i := 1; i <= hs.count(); i++ {
		secrets = append(secrets, fmt.Sprintf("hskey-auth-fake%d", i))
	}
	for _, l := range logs {
		for _, s := range secrets {
			if strings.Contains(l, s) {
				t.Fatalf("secret %q leaked into a log/output:\n%s", s[:min(len(s), 16)], tail(l, 2000))
			}
		}
	}
	if !strings.Contains(logs[len(logs)-2], "tailnet key minted") {
		t.Fatal("hub log has no mint record")
	}
}

// longarmEnv runs longarm with extra environment variables (no os.Setenv: tests run
// in parallel).
func longarmEnv(t *testing.T, cfgDir string, env []string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bins.longarm, args...)
	cmd.Env = append(append(os.Environ(), "LONGARM_CONFIG_DIR="+cfgDir), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("longarm %v: %v", args, err)
	}
	return result{out.String(), errb.String(), code}
}
