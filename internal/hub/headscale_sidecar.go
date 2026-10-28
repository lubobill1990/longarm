package hub

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"tailscale.com/types/key"

	"github.com/lubobill1990/longarm/internal/relay"
)

// A Headscale sidecar ([tailnet.headscale] sidecar_dir; deploy/azure
// enableHeadscale): Headscale runs in its own container next to the hub,
// sharing this directory and localhost with it, and nothing of it is
// published but what the hub proxies (headscale_proxy.go). The hub owns its
// setup, so no one ever runs a headscale command:
//
//   - every start writes config.yaml, policy.hujson and derp.yaml, and the
//     Noise and DERP private keys, which live in the hub's database (meta):
//     replicated with it, they outlive the containers; losing them would
//     make every node register again
//   - then "ready": the sidecar's start script waits for it (and the
//     container entrypoint restores Headscale's database before that:
//     cmd/longarm-hub container.go)
//   - once Headscale listens, the hub checks its API key (also in its
//     database) and creates a new one over Headscale's unix socket in this
//     directory when there is none, it stopped working or expires within
//     30 days (the previous one is expired). The key is never in the
//     environment or a log; api_key_file gets a copy (0600).

const (
	// where the sidecar's HTTP (API, Noise, DERP) listens: the hub's localhost
	headscaleSidecarListen = "127.0.0.1:8080"
	// the files in sidecar_dir
	headscaleReadyFile  = "ready"
	headscaleSocketFile = "headscale.sock"
	headscaleDBFile     = "db.sqlite"
	// the embedded DERP region
	headscaleDERPRegion     = 900
	headscaleDERPRegionCode = "longarm-hub"
	// meta keys
	metaHeadscaleNoise     = "headscale/noise_private_key"
	metaHeadscaleDERP      = "headscale/derp_private_key"
	metaHeadscaleAPIKey    = "headscale/api_key"
	metaHeadscaleAPIKeyExp = "headscale/api_key_expires"
	// API keys the hub creates live this long and are renewed when they
	// expire within headscaleKeyRenew
	headscaleKeyLife  = 365 * 24 * time.Hour
	headscaleKeyRenew = 30 * 24 * time.Hour
)

// HeadscaleDB is the database of a Headscale sidecar in dir (the container
// entrypoint restores and replicates it with Litestream).
func HeadscaleDB(dir string) string { return filepath.Join(dir, headscaleDBFile) }

// EnvHeadscaleRestore is set by the container entrypoint when the restore
// of the sidecar's database failed: the hub then keeps Headscale stopped
// (no "ready"), so it can't start empty next to a replica it could not read.
const EnvHeadscaleRestore = "LONGARM_HUB_HEADSCALE_RESTORE"

// headscaleSidecarFiles renders the sidecar's files (name -> content).
func headscaleSidecarFiles(cfg *Config, noiseKey, derpKey string) (map[string]string, error) {
	h := cfg.Tailnet.Headscale
	dir := h.SidecarDir
	u, err := url.Parse(h.URL)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("[tailnet.headscale] url %q: %v", h.URL, err)
	}
	derpPort := 443
	if u.Scheme == "http" {
		derpPort = 80
	}
	if p := u.Port(); p != "" {
		if derpPort, err = strconv.Atoi(p); err != nil {
			return nil, fmt.Errorf("[tailnet.headscale] url %q: bad port", h.URL)
		}
	}
	listen := headscaleSidecarListen
	if a, err := url.Parse(h.APIURL); err == nil && a.Host != "" {
		listen = a.Host
	}
	q := strconv.Quote
	p := func(name string) string { return q(filepath.ToSlash(filepath.Join(dir, name))) }
	urls := "[]"
	if h.PublicDERP {
		urls = "[" + q("https://controlplane.tailscale.com/derpmap/default") + "]"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `# Written by longarm-hub on every start (internal/hub/headscale_sidecar.go):
# edits are lost. Headscale %s behind the hub's https listener.
server_url: %s
listen_addr: %s
metrics_listen_addr: "127.0.0.1:9090"
# the remote gRPC API stays off (no TLS, not insecure); the hub uses the
# REST API and the unix socket
grpc_listen_addr: "127.0.0.1:50443"
grpc_allow_insecure: false
# the hub's reverse proxy sets X-Forwarded-For
trusted_proxies: ["127.0.0.1/32", "::1/128"]
noise:
  private_key_path: %s
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
  allocation: sequential
derp:
  server:
    # embedded DERP at <server_url>/derp (proxied by the hub); its region is
    # derp.yaml's (STUN: see there)
    enabled: true
    region_id: %d
    region_code: %s
    region_name: "Longarm hub"
    verify_clients: true
    stun_listen_addr: "127.0.0.1:3478"
    private_key_path: %s
    automatically_add_embedded_derp_region: false
  urls: %s
  paths: [%s]
  auto_update_enabled: %t
  update_frequency: 24h
disable_check_updates: true
database:
  type: sqlite
  sqlite:
    path: %s
    write_ahead_log: true
log:
  level: info
  format: text
policy:
  mode: file
  path: %s
dns:
  # nodes reach workers by the tailnet IPs the hub advertises: no MagicDNS,
  # the hosts' resolvers stay untouched
  magic_dns: false
  override_local_dns: false
  base_domain: longarm.internal
  nameservers:
    global: []
# the hub (another container, uid 65532) creates its API key over this
# socket; the directory is shared with the hub only (mode 0750)
unix_socket: %s
unix_socket_permission: "0777"
logtail:
  enabled: false
node:
  ephemeral:
    inactivity_timeout: 30m
`, "v0.29", q(strings.TrimRight(h.URL, "/")), q(listen), p("noise_private.key"), headscaleDERPRegion, q(headscaleDERPRegionCode),
		p("derp_server_private.key"), urls, p("derp.yaml"), h.PublicDERP, p(headscaleDBFile), p("policy.hujson"), p(headscaleSocketFile))

	// The hub's region: DERP over https through the hub. No STUN of its
	// own: ACI publishes at most 5 ports per IP (the hub uses all of them)
	// and source-NATs inbound UDP, so STUN answers from inside the group
	// would show nodes a private address. The region's STUN comes from the
	// external servers the hub advertises for longnet (relay_stun_advertise,
	// the template's stunServers), as STUN-only nodes of the same region
	// (a region without a DERP node could become a node's home).
	var d strings.Builder
	fmt.Fprintf(&d, `# Written by longarm-hub on every start (internal/hub/headscale_sidecar.go).
regions:
  %d:
    regionid: %d
    regioncode: %s
    regionname: "Longarm hub"
    nodes:
      - name: "%da"
        regionid: %d
        hostname: %s
        derpport: %d
        stunport: -1
`, headscaleDERPRegion, headscaleDERPRegion, q(headscaleDERPRegionCode), headscaleDERPRegion, headscaleDERPRegion, q(u.Hostname()), derpPort)
	for i, s := range sidecarSTUN(cfg.RelaySTUNAdvertise) {
		host, port, _ := net.SplitHostPort(s)
		fmt.Fprintf(&d, `      - name: "%ds%d"
        regionid: %d
        hostname: %s
        stunport: %s
        stunonly: true
`, headscaleDERPRegion, i+1, headscaleDERPRegion, q(host), port)
	}

	tags := func(t []string) string {
		qs := make([]string, len(t))
		for i, s := range t {
			qs[i] = q(s)
		}
		return strings.Join(qs, ", ")
	}
	dsts := func(t []string) string {
		qs := make([]string, len(t))
		for i, s := range t {
			qs[i] = q(s + ":7701")
		}
		return strings.Join(qs, ", ")
	}
	t := cfg.Tailnet
	owners := ""
	for _, tag := range append(append([]string(nil), t.WorkerTags...), t.ClientTags...) {
		if !strings.Contains(owners, q(tag)+":") {
			owners += fmt.Sprintf("    %s: [],\n", q(tag))
		}
	}
	policy := fmt.Sprintf(`// Written by longarm-hub on every start (internal/hub/headscale_sidecar.go).
// Nodes join with one-time pre-auth keys the hub mints: workers (longarmd)
// %s, clients (longarm) %s. Clients reach workers on longarmd's port 7701
// (ssh, forwards and files all go through it, inside Longarm's mTLS), and
// workers reach workers there for port links (docs/usage.md "Port links").
// Nothing else.
{
  "tagOwners": {
%s  },
  "acls": [
    {"action": "accept", "src": [%s], "dst": [%s]},
    {"action": "accept", "src": [%s], "dst": [%s]},
  ],
}
`, strings.Join(t.WorkerTags, ","), strings.Join(t.ClientTags, ","), owners,
		tags(t.ClientTags), dsts(t.WorkerTags), tags(t.WorkerTags), dsts(t.WorkerTags))

	return map[string]string{
		"config.yaml":             b.String(),
		"derp.yaml":               d.String(),
		"policy.hujson":           policy,
		"noise_private.key":       noiseKey,
		"derp_server_private.key": derpKey,
	}, nil
}

// sidecarSTUN: the external STUN servers of relay_stun_advertise (host:port).
func sidecarSTUN(list []string) []string {
	var out []string
	for _, s := range list {
		s = strings.TrimSpace(s)
		if relay.STUNKeyword(s) || strings.HasPrefix(s, ":") {
			continue
		}
		if host, port, err := net.SplitHostPort(s); err == nil && host != "" && port != "" {
			out = append(out, s)
		}
	}
	return out
}

// headscaleKeyMeta returns a private key kept in the database, creating it.
func (h *Hub) headscaleKeyMeta(k string) (string, error) {
	v, err := h.store.Meta(k)
	if err != nil || v != "" {
		return v, err
	}
	b, err := key.NewMachine().MarshalText() // "privkey:<hex>", what Headscale reads
	if err != nil {
		return "", err
	}
	if err := h.store.SetMeta(k, string(b)); err != nil {
		return "", err
	}
	h.log.Info("headscale: new private key", "which", k)
	return string(b), nil
}

// prepareHeadscaleSidecar writes the sidecar's files and "ready".
func (h *Hub) prepareHeadscaleSidecar() error {
	dir := h.cfg.Tailnet.Headscale.SidecarDir
	if v := os.Getenv(EnvHeadscaleRestore); v != "" {
		return fmt.Errorf("Headscale stays stopped: its database could not be restored (%s); see the log above", v)
	}
	noise, err := h.headscaleKeyMeta(metaHeadscaleNoise)
	if err != nil {
		return err
	}
	derp, err := h.headscaleKeyMeta(metaHeadscaleDERP)
	if err != nil {
		return err
	}
	files, err := headscaleSidecarFiles(h.cfg, noise, derp)
	if err != nil {
		return err
	}
	for name, content := range files {
		if err := writeFileAtomic(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(dir, headscaleReadyFile), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// ---------------------------------------------------------------- API key

// headscaleSocket calls Headscale's gRPC API on its unix socket (no
// authentication: whoever can open the socket is an admin). The messages
// are encoded by hand (headscale/v1 apikey.proto) to stay clear of
// Headscale's generated code.
type headscaleSocket struct {
	conn *grpc.ClientConn
}

func dialHeadscaleSocket(path string) (*headscaleSocket, error) {
	c, err := grpc.NewClient("passthrough:///headscale",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})))
	if err != nil {
		return nil, err
	}
	return &headscaleSocket{conn: c}, nil
}

func (s *headscaleSocket) Close() error { return s.conn.Close() }

func (s *headscaleSocket) call(ctx context.Context, method string, req []byte) ([]byte, error) {
	var resp []byte
	if err := s.conn.Invoke(ctx, "/headscale.v1.HeadscaleService/"+method, &req, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// CreateAPIKey: CreateApiKeyRequest{expiration = 1} -> CreateApiKeyResponse{api_key = 1}.
func (s *headscaleSocket) CreateAPIKey(ctx context.Context, exp time.Time) (string, error) {
	ts, err := proto.Marshal(timestamppb.New(exp))
	if err != nil {
		return "", err
	}
	req := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), ts)
	resp, err := s.call(ctx, "CreateApiKey", req)
	if err != nil {
		return "", err
	}
	k, err := pbString(resp, 1)
	if err != nil {
		return "", err
	}
	if k == "" {
		return "", errors.New("headscale CreateApiKey: no key")
	}
	return k, nil
}

// ExpireAPIKey: ExpireApiKeyRequest{prefix = 1}.
func (s *headscaleSocket) ExpireAPIKey(ctx context.Context, prefix string) error {
	req := protowire.AppendString(protowire.AppendTag(nil, 1, protowire.BytesType), prefix)
	_, err := s.call(ctx, "ExpireApiKey", req)
	return err
}

// pbString is the last value of string field n of a message.
func pbString(b []byte, n protowire.Number) (string, error) {
	var v string
	for len(b) > 0 {
		num, typ, l := protowire.ConsumeTag(b)
		if l < 0 {
			return "", protowire.ParseError(l)
		}
		b = b[l:]
		if num == n && typ == protowire.BytesType {
			s, l := protowire.ConsumeString(b)
			if l < 0 {
				return "", protowire.ParseError(l)
			}
			v, b = s, b[l:]
			continue
		}
		l = protowire.ConsumeFieldValue(num, typ, b)
		if l < 0 {
			return "", protowire.ParseError(l)
		}
		b = b[l:]
	}
	return v, nil
}

// headscaleKeyPrefix is the public part of an API key
// (hskey-api-<12 characters>-<secret>), what ExpireApiKey takes.
func headscaleKeyPrefix(k string) string {
	const p = "hskey-api-"
	if strings.HasPrefix(k, p) && len(k) > len(p)+12 {
		return k[len(p) : len(p)+12]
	}
	if i := strings.IndexByte(k, '.'); i > 0 { // older keys: prefix.secret
		return k[:i]
	}
	return ""
}

// rawCodec passes already encoded protobuf messages (*[]byte) through.
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	b, ok := v.(*[]byte)
	if !ok {
		return nil, fmt.Errorf("rawCodec: %T", v)
	}
	return *b, nil
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	b, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("rawCodec: %T", v)
	}
	*b = append((*b)[:0], data...)
	return nil
}

func (rawCodec) Name() string { return "proto" }

// ensureHeadscaleKey makes sure the provider has a working API key that is
// not about to expire: the one in the database, else a new one.
func (h *Hub) ensureHeadscaleKey(ctx context.Context, c *headscaleClient) error {
	cfg := c.cfg
	old, err := h.store.Meta(metaHeadscaleAPIKey)
	if err != nil {
		return err
	}
	exp, _ := h.store.Meta(metaHeadscaleAPIKeyExp)
	expires, _ := time.Parse(time.RFC3339, exp)
	if old != "" && time.Until(expires) > headscaleKeyRenew {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := c.doKey(cctx, old, "GET", "/api/v1/apikey", nil, nil)
		cancel()
		if err == nil {
			if c.apiKey() != old {
				c.setKey(old)
				h.writeHeadscaleKeyFile(old)
				h.log.Info("headscale: API key ok", "prefix", headscaleKeyPrefix(old), "expires", expires.Format(time.RFC3339))
			}
			return nil
		}
		if !errors.Is(err, errHeadscaleUnauthorized) {
			return err // Headscale not answering (yet): try again
		}
		h.log.Warn("headscale: the stored API key no longer works (Headscale's database was reset?): creating a new one",
			"prefix", headscaleKeyPrefix(old))
	}
	sock, err := dialHeadscaleSocket(filepath.Join(cfg.SidecarDir, headscaleSocketFile))
	if err != nil {
		return err
	}
	defer sock.Close()
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	newExp := time.Now().Add(headscaleKeyLife).UTC().Truncate(time.Second)
	k, err := sock.CreateAPIKey(cctx, newExp)
	if err != nil {
		return fmt.Errorf("headscale socket: create API key: %w", err)
	}
	if err := h.store.SetMeta(metaHeadscaleAPIKey, k); err != nil {
		return err
	}
	if err := h.store.SetMeta(metaHeadscaleAPIKeyExp, newExp.Format(time.RFC3339)); err != nil {
		return err
	}
	c.setKey(k)
	h.writeHeadscaleKeyFile(k)
	h.log.Info("headscale: new API key", "prefix", headscaleKeyPrefix(k), "expires", newExp.Format(time.RFC3339))
	if p := headscaleKeyPrefix(old); p != "" && p != headscaleKeyPrefix(k) {
		if err := sock.ExpireAPIKey(cctx, p); err != nil {
			h.log.Info("headscale: previous API key not expired", "prefix", p, "err", err)
		}
	}
	return nil
}

func (h *Hub) writeHeadscaleKeyFile(k string) {
	f := h.cfg.Tailnet.Headscale.APIKeyFile
	if f == "" {
		return
	}
	err := os.MkdirAll(filepath.Dir(f), 0o700)
	if err == nil {
		err = writeFileAtomic(f, []byte(k+"\n"), 0o600)
	}
	if err != nil {
		h.log.Warn("headscale: api_key_file not written (the hub uses the key anyway)", "file", f, "err", err)
	}
}

// ---------------------------------------------------------------- health

// headscale states (TailnetStatus control_health, /healthz)
const (
	hsStateStarting = "starting"
	hsStateOK       = "ok"
	hsStateDown     = "down"
)

func (h *Hub) setHeadscaleHealth(state, detail string) {
	h.tailnetMu.Lock()
	changed := h.hsHealth != state
	h.hsHealth, h.hsHealthDetail = state, detail
	h.tailnetMu.Unlock()
	if changed {
		if state == hsStateOK {
			h.log.Info("headscale: up", "api", h.cfg.Tailnet.Headscale.APIURL)
		} else {
			h.log.Warn("headscale: "+state, "detail", detail)
		}
	}
}

// headscaleHealth: "" (no Headscale), starting, ok or down, and the detail.
func (h *Hub) headscaleHealth() (string, string) {
	h.tailnetMu.Lock()
	defer h.tailnetMu.Unlock()
	return h.hsHealth, h.hsHealthDetail
}

// runHeadscale watches Headscale: its /health every 30 s (5 s until it is
// up), and with a sidecar the API key (at the start and every 10 minutes:
// a reset Headscale database invalidates it).
func (h *Hub) runHeadscale(ctx context.Context, c *headscaleClient) {
	sidecar := c.cfg.SidecarDir != ""
	if sidecar {
		h.setHeadscaleHealth(hsStateStarting, "waiting for Headscale")
		if err := h.prepareHeadscaleSidecar(); err != nil {
			h.setHeadscaleHealth(hsStateDown, RedactKeys(err.Error()))
			h.log.Error("headscale sidecar not prepared", "dir", c.cfg.SidecarDir, "err", err)
			return
		}
	}
	var keyChecked time.Time
	up := false
	for {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.health(cctx)
		cancel()
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			state := hsStateDown
			if sidecar && !up {
				state = hsStateStarting
			}
			h.setHeadscaleHealth(state, RedactKeys(err.Error()))
		case sidecar && (c.apiKey() == "" || time.Since(keyChecked) > 10*time.Minute):
			if err := h.ensureHeadscaleKey(ctx, c); err != nil {
				state := hsStateDown
				if !up {
					state = hsStateStarting
				}
				h.setHeadscaleHealth(state, "API key: "+RedactKeys(err.Error()))
				break
			}
			keyChecked = time.Now()
			up = true
			h.setHeadscaleHealth(hsStateOK, "")
		default:
			up = true
			h.setHeadscaleHealth(hsStateOK, "")
		}
		wait := 30 * time.Second
		if !up {
			wait = 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
