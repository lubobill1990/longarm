package hub

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// sidecarFake is a Headscale sidecar: the REST API on a TCP port and the
// gRPC API on the unix socket in the shared directory.
type sidecarFake struct {
	mu      sync.Mutex
	keys    map[string]time.Time // API keys -> expiry
	created int
	expired []string // prefixes
	minted  int
	down    bool
}

func (f *sidecarFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path == "/health" {
		w.Write([]byte(`{"status":"pass"}`))
		return
	}
	k := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if exp, ok := f.keys[k]; !ok || time.Now().After(exp) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/apikey":
		w.Write([]byte(`{"apiKeys":[]}`))
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/preauthkey":
		f.minted++
		json.NewEncoder(w).Encode(map[string]any{"preAuthKey": map[string]any{"key": "hskey-auth-minted"}})
	default:
		http.NotFound(w, r)
	}
}

func (f *sidecarFake) grpcHandler(_ any, stream grpc.ServerStream) error {
	method, _ := grpc.MethodFromServerStream(stream)
	var in []byte
	if err := stream.RecvMsg(&in); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []byte
	switch method {
	case "/headscale.v1.HeadscaleService/CreateApiKey":
		// CreateApiKeyRequest{expiration = 1}
		num, _, n := protowire.ConsumeTag(in)
		b, _ := protowire.ConsumeBytes(in[n:])
		var ts timestamppb.Timestamp
		if num != 1 || proto.Unmarshal(b, &ts) != nil {
			return io.ErrUnexpectedEOF
		}
		f.created++
		k := "hskey-api-" + strings.Repeat(string(rune('A'+f.created)), 12) + "-" + strings.Repeat("s", 64)
		f.keys[k] = ts.AsTime()
		out = protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), k)
	case "/headscale.v1.HeadscaleService/ExpireApiKey":
		p, err := pbString(in, 1)
		if err != nil {
			return err
		}
		f.expired = append(f.expired, p)
		for k := range f.keys {
			if headscaleKeyPrefix(k) == p {
				f.keys[k] = time.Now().Add(-time.Second)
			}
		}
	default:
		return io.EOF
	}
	return stream.SendMsg(&out)
}

func sidecarHub(t *testing.T, dataDir, dir, apiURL string) (*Hub, func()) {
	t.Helper()
	cfg := &Config{DataDir: dataDir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "127.0.0.1:0",
		RelaySTUNAdvertise: []string{"self", "stun.example.net:3478", "[2001:db8::1]:3478"}}
	cfg.Tailnet.Headscale.SidecarDir = dir
	cfg.Tailnet.Headscale.URL = "https://hub.example.com"
	cfg.Tailnet.Headscale.APIURL = apiURL
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	h, err := Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Serve(ctx); close(done) }()
	return h, func() { cancel(); <-done; h.Close() }
}

func waitHeadscale(t *testing.T, h *Hub, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, detail := h.headscaleHealth()
		if st == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("headscale state %q (%s), want %q", st, detail, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHeadscaleSidecar: the hub writes the sidecar's files with keys from
// its database, creates its API key over the socket once Headscale answers,
// mints with it, keeps both across restarts and replaces a key Headscale no
// longer knows (expiring the old one).
func TestHeadscaleSidecar(t *testing.T) {
	dataDir, dir := t.TempDir(), t.TempDir()
	if _, err := InitCA(dataDir); err != nil {
		t.Fatal(err)
	}
	fake := &sidecarFake{keys: map[string]time.Time{}, down: true}
	api := httptest.NewServer(fake)
	defer api.Close()
	sock := filepath.Join(dir, headscaleSocketFile)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	gs := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(fake.grpcHandler))
	go gs.Serve(ln)
	defer gs.Stop()

	h, stop := sidecarHub(t, dataDir, dir, api.URL)
	// Headscale not up yet: starting, nothing minted, files and ready written
	waitHeadscale(t, h, hsStateStarting)
	for _, f := range []string{"ready", "config.yaml", "derp.yaml", "policy.hujson", "noise_private.key", "derp_server_private.key"} {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s not written", f)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if _, err := h.tailnet.Mint(context.Background(), mintRequest{tags: []string{"tag:longarm-worker"}, ttl: time.Hour}); err == nil {
		t.Fatal("minted without an API key")
	}
	read := func(name string) string { b, _ := os.ReadFile(filepath.Join(dir, name)); return string(b) }
	cfgY, derpY, pol := read("config.yaml"), read("derp.yaml"), read("policy.hujson")
	for _, want := range []string{`server_url: "https://hub.example.com"`, `listen_addr: "` + strings.TrimPrefix(api.URL, "http://") + `"`,
		`automatically_add_embedded_derp_region: false`, `urls: []`, `stun_listen_addr: "127.0.0.1:3478"`,
		`path: "` + filepath.ToSlash(filepath.Join(dir, "db.sqlite")) + `"`, `unix_socket: "` + filepath.ToSlash(sock) + `"`, `magic_dns: false`} {
		if !strings.Contains(cfgY, want) {
			t.Errorf("config.yaml lacks %s:\n%s", want, cfgY)
		}
	}
	for _, want := range []string{`hostname: "hub.example.com"`, "derpport: 443", "stunport: -1",
		`hostname: "stun.example.net"`, "stunport: 3478", "stunonly: true", `hostname: "2001:db8::1"`, `regioncode: "longarm-hub"`} {
		if !strings.Contains(derpY, want) {
			t.Errorf("derp.yaml lacks %s:\n%s", want, derpY)
		}
	}
	for _, want := range []string{`"src": ["tag:longarm-client"], "dst": ["tag:longarm-worker:7701"]`,
		`"src": ["tag:longarm-worker"], "dst": ["tag:longarm-worker:7701"]`} {
		if !strings.Contains(pol, want) {
			t.Errorf("policy lacks %s:\n%s", want, pol)
		}
	}
	noise := read("noise_private.key")
	if !strings.HasPrefix(noise, "privkey:") || noise == read("derp_server_private.key") {
		t.Fatalf("keys %q", noise)
	}

	// Headscale comes up: the hub creates its key and mints with it
	fake.mu.Lock()
	fake.down = false
	fake.mu.Unlock()
	waitHeadscale(t, h, hsStateOK)
	if k, err := h.tailnet.Mint(context.Background(), mintRequest{tags: []string{"tag:longarm-worker"}, ttl: time.Hour}); err != nil || k != "hskey-auth-minted" {
		t.Fatalf("mint: %q %v", k, err)
	}
	kb, _ := os.ReadFile(h.cfg.Tailnet.Headscale.APIKeyFile)
	key1 := strings.TrimSpace(string(kb))
	fake.mu.Lock()
	exp1, created := fake.keys[key1], fake.created
	fake.mu.Unlock()
	if created != 1 || time.Until(exp1) < 300*24*time.Hour {
		t.Fatalf("created %d keys, expiry %s", created, exp1)
	}
	stop()

	// a restart (a new container group: the same database) keeps the keys
	os.Remove(filepath.Join(dir, "ready"))
	h, stop = sidecarHub(t, dataDir, dir, api.URL)
	waitHeadscale(t, h, hsStateOK)
	fake.mu.Lock()
	created = fake.created
	fake.mu.Unlock()
	if created != 1 || read("noise_private.key") != noise {
		t.Fatalf("restart: %d keys created, noise key kept %v", created, read("noise_private.key") == noise)
	}
	stop()

	// Headscale lost its database: a new key, the old one expired
	fake.mu.Lock()
	delete(fake.keys, key1)
	fake.mu.Unlock()
	h, stop = sidecarHub(t, dataDir, dir, api.URL)
	defer stop()
	waitHeadscale(t, h, hsStateOK)
	fake.mu.Lock()
	created, expired := fake.created, fake.expired
	fake.mu.Unlock()
	if created != 2 || len(expired) != 1 || expired[0] != headscaleKeyPrefix(key1) {
		t.Fatalf("reset: created %d, expired %v", created, expired)
	}
	if _, err := h.tailnet.Mint(context.Background(), mintRequest{tags: []string{"tag:longarm-client"}, ttl: time.Hour}); err != nil {
		t.Fatal(err)
	}
}

// TestHeadscaleSidecarRestoreFailed: Headscale stays stopped (no "ready").
func TestHeadscaleSidecarRestoreFailed(t *testing.T) {
	dataDir, dir := t.TempDir(), t.TempDir()
	if _, err := InitCA(dataDir); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvHeadscaleRestore, "litestream restore: boom")
	h, stop := sidecarHub(t, dataDir, dir, "http://127.0.0.1:1")
	defer stop()
	waitHeadscale(t, h, hsStateDown)
	if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
		t.Fatal("ready written after a failed restore")
	}
	if _, detail := h.headscaleHealth(); !strings.Contains(detail, "boom") {
		t.Fatalf("detail %q", detail)
	}
}

// TestHeadscaleSidecarConfigEnv: the container's environment
// (deploy/azure) configures the sidecar; defaults fill the rest.
func TestHeadscaleSidecarConfigEnv(t *testing.T) {
	data, side := t.TempDir(), t.TempDir()
	t.Setenv("LONGARM_HUB_DATA_DIR", data)
	t.Setenv(EnvPublicURL, "https://hub.example.com/")
	t.Setenv(EnvTailnetProvider, "headscale")
	t.Setenv(EnvHeadscaleDir, side)
	t.Setenv(EnvHeadscalePublicDERP, "1")
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Tailnet.Headscale
	if cfg.Tailnet.Provider != ProviderHeadscale || h.URL != "https://hub.example.com" || h.APIURL != "http://127.0.0.1:8080" ||
		!h.Proxy || !h.PublicDERP || h.APIKeyFile != filepath.Join(data, "headscale-api.key") || h.SidecarDir != side {
		t.Fatalf("%+v %+v", cfg.Tailnet.Provider, h)
	}
	if cfg.Tailnet.ControlURL() != "https://hub.example.com" {
		t.Fatal(cfg.Tailnet.ControlURL())
	}
	t.Setenv(EnvHeadscaleURL, "https://hs.example.com")
	t.Setenv(EnvHeadscaleAPIURL, "http://127.0.0.1:9999")
	t.Setenv(EnvHeadscaleAPIKeyFile, "/run/key")
	if cfg, err = LoadConfig(""); err != nil {
		t.Fatal(err)
	}
	if h := cfg.Tailnet.Headscale; h.URL != "https://hs.example.com" || h.APIURL != "http://127.0.0.1:9999" || h.APIKeyFile != "/run/key" {
		t.Fatalf("%+v", h)
	}
	t.Setenv(EnvHeadscaleDir, "relative")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("relative sidecar_dir accepted")
	}
}

func TestHeadscaleKeyPrefix(t *testing.T) {
	for k, want := range map[string]string{
		"hskey-api-abcdefghijkl-" + strings.Repeat("s", 64): "abcdefghijkl",
		"abcdefg.secret": "abcdefg",
		"nonsense":       "",
	} {
		if got := headscaleKeyPrefix(k); got != want {
			t.Errorf("%q: %q, want %q", k, got, want)
		}
	}
}
