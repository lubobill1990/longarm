package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
)

// fakeHeadscaleAPI answers POST /api/v1/preauthkey like Headscale.
type fakeHeadscaleAPI struct {
	mu   sync.Mutex
	tags [][]any
}

func (f *fakeHeadscaleAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer fake-api-key" || r.URL.Path != "/api/v1/preauthkey" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	var b map[string]any
	json.NewDecoder(r.Body).Decode(&b)
	f.mu.Lock()
	f.tags = append(f.tags, b["aclTags"].([]any))
	n := len(f.tags)
	f.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"preAuthKey": map[string]any{"key": fmt.Sprintf("hskey-auth-fake%d", n)}})
}

func (f *fakeHeadscaleAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tags)
}

// TestHubHeadscaleEnroll: with [headscale] in hub.toml, longarmd join and longarm join
// receive one-time tagged pre-auth keys (stored next to the tsnet state,
// tailnet recorded in hub.toml); longarmd adds a tailscale listener for that
// tailnet in the background and keeps serving its other legs while the
// control server is unreachable; a worker that lost its key gets a new one
// from the hub (Fleet.TailnetKey).
func TestHubHeadscaleEnroll(t *testing.T) {
	if testing.Short() {
		t.Skip("slow; skipped with -short")
	}
	t.Parallel()
	fake := &fakeHeadscaleAPI{}
	api := httptest.NewServer(fake)
	defer api.Close()
	root := t.TempDir()
	keyFile := filepath.Join(root, "hs-api.key")
	os.WriteFile(keyFile, []byte("fake-api-key"), 0o600)
	// The control URL nodes get is unreachable: tsnet can't come up.
	h := newHubEnv(t, fmt.Sprintf("[headscale]\nurl = \"http://127.0.0.1:1\"\napi_url = %q\napi_key_file = %q\n",
		api.URL, filepath.ToSlash(keyFile)))
	wa := h.newWorkerDir(t, "wa", "")
	h.joinWorker(t, wa, true) // pending -> approved: the key comes with the certificate
	wa.start(t, true)         // finishes the enrollment
	h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	// The old [headscale] section still works; keys wait next to the state
	// dir of their control server.
	key, err := os.ReadFile(filepath.Join(wa.dataDir, "tsnet-127.0.0.1-1.authkey"))
	if err != nil || !strings.HasPrefix(string(key), "hskey-auth-fake") {
		t.Fatalf("worker key: %q %v", key, err)
	}
	hubToml, _ := os.ReadFile(filepath.Join(wa.dataDir, "hub.toml"))
	if !strings.Contains(string(hubToml), `control_url = "http://127.0.0.1:1"`) || !strings.Contains(string(hubToml), `hostname = "longarm-wa"`) {
		t.Fatalf("hub.toml:\n%s", hubToml)
	}
	if fake.tags[0][0] != "tag:longarm-worker" {
		t.Fatalf("worker key tags %v", fake.tags[0])
	}
	// The tailscale leg can't come up, the tcp leg serves anyway.
	if r := h.longarm(h.client, "exec", "wa", "--", bins.helper, "print", "1", "0", "3"); r.code != 3 {
		t.Fatalf("exec while the tailnet is down: %+v", r)
	}
	waitFor(t, "tailscale leg starting", 30*time.Second, func() bool {
		b, _ := os.ReadFile(wa.logPath)
		return bytes.Contains(b, []byte(`tsnet starting with hostname \"longarm-wa\"`))
	})

	// A client enrolls with longarm join: tag:longarm-client key + tailnet.
	code := strings.TrimSpace(h.mustLongarm(h.admin, "admin", "join-code", "--role", "client"))
	c2 := filepath.Join(h.root, "c2")
	out := h.mustLongarm(c2, "join", h.addr(), code, "--ca-fingerprint", h.fp, "--name", "bob")
	if !strings.Contains(out, "tailnet http://127.0.0.1:1: key for longarm-client-bob (tag:longarm-client)") {
		t.Fatalf("longarm join:\n%s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(c2, "tsnet-127.0.0.1-1.authkey")); !strings.HasPrefix(string(b), "hskey-auth-fake") {
		t.Fatalf("client key %q", b)
	}

	// The worker lost its key (and has no tsnet state): the hub mints another.
	before := fake.count()
	wa.kill()
	os.Remove(filepath.Join(wa.dataDir, "tsnet-127.0.0.1-1.authkey"))
	os.RemoveAll(filepath.Join(wa.dataDir, "tsnet-127.0.0.1-1"))
	wa.start(t, true)
	waitFor(t, "new key from the hub", 30*time.Second, func() bool { return fake.count() > before })
}

// TestHeadscaleLive (opt-in: LONGARM_HEADSCALE_BIN=path to a headscale
// binary, which builds and runs on Windows too): a real Headscale; longarmd and
// longarm enroll through the hub, their embedded tsnet nodes join the tailnet
// with the minted keys, and longarm reaches the worker over the tailscale leg.
func TestHeadscaleLive(t *testing.T) {
	hsBin := os.Getenv("LONGARM_HEADSCALE_BIN")
	if hsBin == "" {
		t.Skip("set LONGARM_HEADSCALE_BIN to a headscale binary")
	}
	allowInbound(t, bins.longarmd, bins.longarm)
	root := t.TempDir()
	port, mport, gport := freePort(t), freePort(t), freePort(t)
	hsURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	cfg := fmt.Sprintf(`server_url: %s
listen_addr: 127.0.0.1:%d
metrics_listen_addr: 127.0.0.1:%d
grpc_listen_addr: 127.0.0.1:%d
noise:
  private_key_path: ./noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
derp:
  server:
    enabled: false
  urls: []
  paths: [./derp.yaml]
  auto_update_enabled: false
disable_check_updates: true
database:
  type: sqlite
  sqlite:
    path: ./db.sqlite
policy:
  mode: file
  path: ./policy.hujson
dns:
  magic_dns: false
  override_local_dns: false
  base_domain: longarm.internal
unix_socket: ./hs.sock
unix_socket_permission: "0770"
`, hsURL, port, mport, gport)
	cfg = strings.ReplaceAll(cfg, `\`, "/")
	os.WriteFile(filepath.Join(root, "config.yaml"), []byte(cfg), 0o600)
	// Headscale needs a DERP map; same-host nodes connect directly.
	os.WriteFile(filepath.Join(root, "derp.yaml"), []byte("regions:\n  900:\n    regionid: 900\n    regioncode: none\n    regionname: placeholder\n    nodes:\n      - name: 900a\n        regionid: 900\n        hostname: 127.0.0.1\n        ipv4: 127.0.0.1\n        stunport: -1\n        derpport: 1\n"), 0o600)
	os.WriteFile(filepath.Join(root, "policy.hujson"), must(os.ReadFile("../deploy/headscale/config/policy.hujson")), 0o600)
	hs := exec.Command(hsBin, "serve", "-c", filepath.Join(root, "config.yaml"))
	hs.Dir = root // headscale mangles absolute Windows paths: use relative ones
	hsLog, _ := os.Create(filepath.Join(root, "headscale.log"))
	hs.Stdout, hs.Stderr = hsLog, hsLog
	if err := hs.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		hs.Process.Kill()
		hs.Wait()
		hsLog.Close()
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(root, "headscale.log"))
			t.Logf("headscale log:\n%s", tail(string(b), 4000))
		}
	}()
	waitFor(t, "headscale up", 30*time.Second, func() bool {
		r, err := http.Get(hsURL + "/health")
		if err == nil {
			r.Body.Close()
		}
		return err == nil && r.StatusCode == 200
	})
	hsCmd := func(args ...string) *exec.Cmd {
		c := exec.Command(hsBin, append([]string{"-c", filepath.Join(root, "config.yaml")}, args...)...)
		c.Dir = root
		return c
	}
	apiKey, err := hsCmd("apikeys", "create").Output()
	if err != nil {
		t.Fatalf("apikeys create: %v", err)
	}
	keyFile := filepath.Join(root, "api.key")
	os.WriteFile(keyFile, bytes.TrimSpace(apiKey), 0o600)

	h := newHubEnv(t, fmt.Sprintf("[headscale]\nurl = %q\napi_key_file = %q\n", hsURL, filepath.ToSlash(keyFile)))
	wa := h.newWorkerDir(t, "wa", "")
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	n := h.waitNode(t, "wa", "tailscale endpoint", func(n map[string]any) bool {
		return isTrue(n["online"]) && strings.Contains(fmt.Sprint(n["endpoints"]), "tailscale://100.")
	})
	t.Logf("endpoints %v", n["endpoints"])
	if m, _ := filepath.Glob(filepath.Join(wa.dataDir, "tsnet-*.authkey")); len(m) > 0 {
		t.Fatalf("used key not removed: %v", m)
	}
	code := strings.TrimSpace(h.mustLongarm(h.admin, "admin", "join-code", "--role", "client"))
	c2 := filepath.Join(h.root, "c2")
	h.mustLongarm(c2, "join", h.addr(), code, "--ca-fingerprint", h.fp, "--name", "bob")
	// The first longarm use starts the client's tsnet node, which registers with
	// the minted key (whatever leg wins this time).
	if r := h.longarm(c2, "--transport", "tailscale", "info", "wa"); r.code != 0 {
		t.Fatalf("longarm info: %+v", r)
	}
	if m, _ := filepath.Glob(filepath.Join(c2, "tsnet-*.authkey")); len(m) > 0 {
		t.Fatalf("client key not consumed: %v", m)
	}
	nodes, _ := hsCmd("nodes", "list").CombinedOutput()
	t.Logf("headscale nodes:\n%s", nodes)
	if !bytes.Contains(nodes, []byte("longarm-wa")) || !bytes.Contains(nodes, []byte("longarm-client-bob")) ||
		!bytes.Contains(nodes, []byte("tag:longarm-worker")) || !bytes.Contains(nodes, []byte("tag:longarm-client")) {
		t.Fatalf("nodes:\n%s", nodes)
	}
	if os.Getenv("LONGARM_HEADSCALE_DATAPATH") != "1" {
		// Two tsnet nodes on one Windows host with no reachable DERP did
		// not complete a WireGuard handshake in testing (disco ping/pong
		// works). Opt in to try the data path.
		t.Log("data path over the tailnet not checked (LONGARM_HEADSCALE_DATAPATH=1 to try)")
		return
	}
	var info string
	defer func() {
		if t.Failed() {
			t.Logf("last longarm info: %s", info)
		}
	}()
	os.Setenv("LONGARM_CONNECT_TIMEOUT", "45s")
	defer os.Unsetenv("LONGARM_CONNECT_TIMEOUT")
	waitFor(t, "longarm over the tailscale leg", 90*time.Second, func() bool {
		r := h.longarm(c2, "--transport", "tailscale", "info", "wa")
		info = r.stdout + r.stderr
		return r.code == 0 && strings.Contains(r.stdout, "path: tailscale/")
	})
	if r := h.longarm(c2, "--transport", "tailscale", "exec", "wa", "--", bins.helper, "print", "2", "0", "6"); r.code != 6 {
		t.Fatalf("exec over tailscale: %+v", r)
	}
}

// allowInbound adds temporary inbound firewall rules for the test binaries
// (Windows Firewall drops unsolicited inbound UDP otherwise, so tailscale
// can't go direct between two local nodes); removed at cleanup.
func allowInbound(t *testing.T, programs ...string) {
	if runtime.GOOS != "windows" {
		return
	}
	if !platform.Elevated() {
		t.Skip("needs an elevated process (temporary firewall rules for the test binaries)")
	}
	for i, p := range programs {
		name := fmt.Sprintf("Longarm e2e %d-%d", os.Getpid(), i)
		if err := platform.AddFirewallRules(name, p); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { platform.RemoveFirewallRules(name, p) })
	}
}
