// Package hub is longarm-hub, the control plane of DESIGN §3.3: CA and
// enrollment (join codes, approval, renewal, revocation), the node registry
// with heartbeats, the job index rebuilt from worker reports, fleet events,
// the scheduler with reservations and a queue, toolchain profiles with a
// canary rollout, audit storage, the longnet relay (folded in from
// internal/relay), and an HTTP JSON/SSE API with local accounts for the web
// panel, plus install scripts and a release store.
//
// The hub is never on the data plane, and its absence only degrades the
// fleet (DESIGN §3.3): workers keep running jobs and serving clients.
package hub

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/lubobill1990/longarm/internal/relay"
	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Duration is a TOML string like "10m".
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	d.Duration = v
	return err
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Config is hub.toml.
type Config struct {
	DataDir string `toml:"data_dir"`
	// DBPath is the SQLite database (default <data_dir>/hub.db; env
	// LONGARM_HUB_DB). Containers keep it on local disk, replicated by
	// Litestream, while data_dir is a network share (deploy/azure).
	DBPath string `toml:"db_path"`
	// Name is the DNS name in the hub's certificate (clients don't check it
	// unless configured; the role and the CA are always checked).
	Name string `toml:"name"`
	// Listen is the TCP address of the gRPC API; the longnet relay's TLS/TCP
	// fallback shares it (told apart by ALPN). Default ":7443".
	Listen string `toml:"listen"`
	// RelayUDP is the UDP address of the longnet relay (QUIC; STUN answers
	// share it). Default: the port of Listen. "off" disables the relay.
	RelayUDP string `toml:"relay_udp"`
	// RelaySTUN2 is a second UDP port answering STUN (longarm-relay --stun2):
	// with two observation points nodes can tell whether their NAT mapping
	// depends on the destination (NAT type easy/hard). Default "auto" = the
	// relay's UDP port + 1 on the same host (:7444); "off" disables it. Not
	// fatal when the port is taken (NAT types then stay unknown).
	RelaySTUN2 string `toml:"relay_stun2"`
	// RelaySTUNAdvertise overrides the STUN endpoints told to nodes
	// (longarm-relay --stun-advertise): ":port" = the hub's IP as the node dialed
	// it, "host:port" (names are resolved by the nodes), "self" = the ports
	// actually served (relay_udp and relay_stun2), "off" = none. Default:
	// ["self"]. Env: LONGARM_HUB_RELAY_STUN_ADVERTISE (comma-separated).
	// Behind a source NAT that hides the clients' addresses (Azure Container
	// Instances) the hub's own answers are useless: list external servers.
	RelaySTUNAdvertise []string `toml:"relay_stun_advertise"`
	// RelayRate limits each relay session (bytes/s, e.g. "32M"); "0" = off.
	RelayRate string `toml:"relay_rate"`
	// HTTP is the panel/API listener (behind a reverse proxy). Default
	// "127.0.0.1:7480"; "off" disables it.
	HTTP string `toml:"http"`
	// PublicURL is the external base URL of the HTTP listener (install
	// scripts, links), e.g. https://hub.example.com.
	PublicURL string `toml:"public_url"`
	// PublicAddr is the gRPC address nodes dial ("203.0.113.10:7443"); it goes
	// into install scripts and the hub certificate's SANs.
	PublicAddr string `toml:"public_addr"`
	// Hosts are extra SANs (DNS names / IPs) for the hub certificate.
	Hosts []string `toml:"hosts"`
	// AutoApprove admits joining workers without `longarm admin approve`.
	AutoApprove bool   `toml:"auto_approve"`
	LogFile     string `toml:"log_file"`

	Certs     CertConfig      `toml:"certs"`
	Scheduler SchedConfig     `toml:"scheduler"`
	Web       WebConfig       `toml:"web"`
	Heartbeat HeartbeatConfig `toml:"heartbeat"`
	Releases  ReleaseConfig   `toml:"releases"`
	Tailnet   TailnetConfig   `toml:"tailnet"`
	Tenancy   TenancyConfig   `toml:"tenancy"`
	// Headscale is P2's [headscale] section; still accepted (it means
	// [tailnet] provider = "headscale" with these settings).
	Headscale HeadscaleConfig `toml:"headscale"`
}

// Tailnet providers ([tailnet] provider).
const (
	ProviderTailscale = "tailscale"
	ProviderHeadscale = "headscale"
	ProviderNone      = "none"
)

// TailnetConfig is [tailnet]: which control plane the embedded tailscale
// nodes of longarmd / longarm join (DESIGN §4.2, §4.4). The hub mints a one-time,
// pre-authorized, tagged key per node: through the Tailscale API (OAuth
// client with the auth_keys scope) or Headscale's REST API.
type TailnetConfig struct {
	// Provider: "tailscale" | "headscale" | "none". Default: headscale when
	// [tailnet.headscale] (or the old [headscale]) has a url, tailscale when
	// an OAuth secret is configured, else none.
	Provider string `toml:"provider"`
	// EphemeralClients: client nodes (longarm, longarm daemon, longarm mcp) vanish from
	// the tailnet when offline (default true). Workers are never ephemeral:
	// the hub advertises their stable tailnet IP.
	EphemeralClients *bool `toml:"ephemeral_clients"`
	// KeyTTL is the lifetime of a minted key (default 1h; used once).
	KeyTTL Duration `toml:"key_ttl"`
	// WorkerTags / ClientTags are the ACL tags of minted keys (defaults
	// tag:longarm-worker / tag:longarm-client; the tailnet policy needs tagOwners for
	// them, and a Tailscale OAuth client must be allowed to use them).
	WorkerTags []string `toml:"worker_tags"`
	ClientTags []string `toml:"client_tags"`

	Tailscale TailscaleConfig `toml:"tailscale"`
	Headscale HeadscaleConfig `toml:"headscale"`
}

// TailscaleConfig is [tailnet.tailscale]: the official Tailscale service.
type TailscaleConfig struct {
	// OAuthClientID of the OAuth client / trust credential (scope auth_keys,
	// tag tag:longarm-hub, which owns the node tags: TagsHint). Empty = taken from the secret
	// (tskey-client-<id>-...).
	OAuthClientID string `toml:"oauth_client_id"`
	// OAuthSecretFile holds the client secret (e.g.
	// /etc/longarm-hub/tailscale-oauth.secret, root:longarm-hub 0640); env
	// LONGARM_HUB_TAILSCALE_OAUTH_SECRET wins.
	OAuthSecretFile string `toml:"oauth_secret_file"`
	// Tailnet the keys are minted in; "-" (default) = the OAuth client's.
	Tailnet string `toml:"tailnet"`
	// APIURL default https://api.tailscale.com.
	APIURL string `toml:"api_url"`
	// ControlURL nodes use; empty = Tailscale's default
	// (https://controlplane.tailscale.com).
	ControlURL string `toml:"control_url"`
}

// DefaultTailscaleControlURL is Tailscale's control server (what tsnet
// uses when no ControlURL is set).
const DefaultTailscaleControlURL = "https://controlplane.tailscale.com"

// EnvTailscaleSecret overrides [tailnet.tailscale] oauth_secret_file.
const EnvTailscaleSecret = "LONGARM_HUB_TAILSCALE_OAUTH_SECRET"

// ReleaseConfig is [releases] (self-update, P2).
type ReleaseConfig struct {
	// PublicKey is the minisign public key releases are signed with. When
	// set, `release add` refuses binaries whose signature doesn't verify
	// (workers always verify against the key built into them).
	PublicKey string `toml:"public_key"`
}

// HeadscaleConfig is [headscale]: the self-hosted Tailscale control plane
// (DESIGN §4.4). With it the hub mints pre-auth keys for joining nodes.
type HeadscaleConfig struct {
	// URL is the Headscale server (the tsnet ControlURL), e.g.
	// https://hs.example.com. Empty = no tailnet enrollment.
	URL string `toml:"url"`
	// APIURL is where the hub reaches Headscale's REST API, if not URL
	// (e.g. http://127.0.0.1:8080 on the same host).
	APIURL string `toml:"api_url"`
	// APIKeyFile holds a Headscale API key (`headscale apikeys create`);
	// env LONGARM_HUB_HEADSCALE_API_KEY wins.
	APIKeyFile string `toml:"api_key_file"`
	// User owns the nodes (created if missing; default "longarm").
	User string `toml:"user"`
	// Old [headscale] spellings of the [tailnet] settings, used when
	// [tailnet] doesn't set them. Ephemeral now only applies to clients
	// (ephemeral_clients); workers are never ephemeral.
	WorkerTags []string `toml:"worker_tags"`
	ClientTags []string `toml:"client_tags"`
	Ephemeral  bool     `toml:"ephemeral"`
	KeyTTL     Duration `toml:"key_ttl"`
}

// TenancyConfig is [tenancy]: several people share the hub and its
// workers (docs/security.md). Separation is on as soon as a user exists
// (longarm admin user add), a worker isolates users ([isolation] mode =
// "per-user") or enabled is set.
type TenancyConfig struct {
	Enabled bool `toml:"enabled"`
	// DefaultQuota applies to every user without an override (0 = no limit).
	DefaultQuota QuotaConfig `toml:"default_quota"`
}

// QuotaConfig is a quota in hub.toml.
type QuotaConfig struct {
	MaxJobs              int32   `toml:"max_jobs"`              // active jobs in the fleet
	MaxMemGB             float64 `toml:"max_mem_gb"`            // reserved memory in the fleet
	MaxJobsPerWorker     int32   `toml:"max_jobs_per_worker"`   // enforced by each worker
	MaxMemGBPerWorker    float64 `toml:"max_mem_gb_per_worker"` // enforced by each worker
	MaxSessionsPerWorker int32   `toml:"max_sessions_per_worker"`
	JobMemGB             float64 `toml:"job_mem_gb"`     // default memory limit of a job
	MaxJobMemGB          float64 `toml:"max_job_mem_gb"` // cap on a job's memory limit
	JobCPUs              float64 `toml:"job_cpus"`       // default CPU limit of a job (cores)
	MaxJobCPUs           float64 `toml:"max_job_cpus"`   // cap on a job's CPU limit
}

// PB converts the quota.
func (q QuotaConfig) PB() *pb.Quota {
	return &pb.Quota{MaxJobs: q.MaxJobs, MaxMemGb: q.MaxMemGB, MaxJobsPerWorker: q.MaxJobsPerWorker,
		MaxMemGbPerWorker: q.MaxMemGBPerWorker, MaxSessionsPerWorker: q.MaxSessionsPerWorker, JobMemGb: q.JobMemGB,
		MaxJobMemGb: q.MaxJobMemGB, JobCpus: q.JobCPUs, MaxJobCpus: q.MaxJobCPUs}
}

// CertConfig is [certs].
type CertConfig struct {
	WorkerTTL Duration `toml:"worker_ttl"` // default 90 days
	ClientTTL Duration `toml:"client_ttl"` // default 30 days (client and admin)
	// RenewBefore: nodes renew when their certificate expires sooner than
	// this. Default: a third of the TTL.
	RenewBefore Duration `toml:"renew_before"`
}

// SchedConfig is [scheduler].
type SchedConfig struct {
	GateFreeGB *float64           `toml:"gate_free_gb"`
	GateCPU    *float64           `toml:"gate_cpu"`
	MemGB      map[string]float64 `toml:"mem_gb"` // per job type
	// ReservationTTL: a placement nobody submitted is released after this.
	ReservationTTL Duration `toml:"reservation_ttl"`
}

// WebConfig is [web].
type WebConfig struct {
	SessionTTL Duration `toml:"session_ttl"` // default 12h
	// InsecureCookie drops the Secure cookie flag (plain-http testing).
	InsecureCookie bool `toml:"insecure_cookie"`
	// TrustedProxies are the reverse proxies (CIDRs) whose X-Forwarded-For /
	// -Host / -Proto headers are believed (default loopback; with docker add
	// the bridge network, e.g. "172.16.0.0/12").
	TrustedProxies []string `toml:"trusted_proxies"`
}

// HeartbeatConfig is [heartbeat].
type HeartbeatConfig struct {
	Interval Duration `toml:"interval"` // metrics period asked from workers, default 2s
	Offline  Duration `toml:"offline"`  // no report for this long = offline, default 10s
}

// DefaultPort of the hub.
const DefaultPort = 7443

// LoadConfig reads a hub.toml (path "" = defaults) and applies defaults.
func LoadConfig(path string) (*Config, error) {
	c := &Config{}
	if path != "" {
		if _, err := toml.DecodeFile(path, c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := c.applyEnv(); err != nil {
		return nil, err
	}
	return c, c.applyDefaults()
}

func (c *Config) applyDefaults() error {
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir()
	}
	if c.Name == "" {
		c.Name = "hub"
	}
	if c.Listen == "" {
		c.Listen = fmt.Sprintf(":%d", DefaultPort)
	}
	if c.RelayUDP == "" {
		_, port, err := net.SplitHostPort(c.Listen)
		if err != nil {
			return fmt.Errorf("listen %q: %w", c.Listen, err)
		}
		host, _, _ := net.SplitHostPort(c.Listen)
		c.RelayUDP = net.JoinHostPort(host, port)
	}
	if c.RelaySTUN2 == "" {
		c.RelaySTUN2 = "auto"
	}
	if c.RelaySTUN2 != "auto" && c.RelaySTUN2 != "off" {
		if _, _, err := net.SplitHostPort(c.RelaySTUN2); err != nil {
			return fmt.Errorf("relay_stun2 %q: want host:port, \"auto\" or \"off\": %w", c.RelaySTUN2, err)
		}
	}
	for _, s := range c.RelaySTUNAdvertise {
		if relay.STUNKeyword(s) {
			continue
		}
		if _, _, err := net.SplitHostPort(s); err != nil {
			return fmt.Errorf("relay_stun_advertise %q: want \"self\", \"off\", \":port\" or host:port: %w", s, err)
		}
	}
	if c.RelayRate == "" {
		c.RelayRate = "32M"
	}
	if c.HTTP == "" {
		c.HTTP = "127.0.0.1:7480"
	}
	if c.Certs.WorkerTTL.Duration <= 0 {
		c.Certs.WorkerTTL.Duration = 90 * 24 * time.Hour
	}
	if c.Certs.ClientTTL.Duration <= 0 {
		c.Certs.ClientTTL.Duration = 30 * 24 * time.Hour
	}
	if rb := c.Certs.RenewBefore.Duration; rb > 0 && (rb >= c.Certs.WorkerTTL.Duration || rb >= c.Certs.ClientTTL.Duration) {
		return fmt.Errorf("certs.renew_before (%s) must be shorter than worker_ttl and client_ttl", rb)
	}
	if c.Web.SessionTTL.Duration <= 0 {
		c.Web.SessionTTL.Duration = 12 * time.Hour
	}
	if c.Heartbeat.Interval.Duration <= 0 {
		c.Heartbeat.Interval.Duration = 2 * time.Second
	}
	if c.Heartbeat.Offline.Duration <= 0 {
		c.Heartbeat.Offline.Duration = 5 * c.Heartbeat.Interval.Duration
	}
	if c.Scheduler.ReservationTTL.Duration <= 0 {
		c.Scheduler.ReservationTTL.Duration = 2 * time.Minute
	}
	if len(c.Web.TrustedProxies) == 0 {
		c.Web.TrustedProxies = []string{"127.0.0.0/8", "::1/128"}
	}
	for _, p := range c.Web.TrustedProxies {
		if _, _, err := net.ParseCIDR(p); err != nil {
			return fmt.Errorf("web.trusted_proxies: %w", err)
		}
	}
	if err := c.tailnetDefaults(); err != nil {
		return err
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.PublicURL != "" && !strings.HasPrefix(c.PublicURL, "http://") && !strings.HasPrefix(c.PublicURL, "https://") {
		return errors.New("public_url must start with http:// or https://")
	}
	return nil
}

// DatabasePath is db_path, or hub.db in the data dir.
func (c *Config) DatabasePath() string {
	if c.DBPath != "" {
		return c.DBPath
	}
	return filepath.Join(c.DataDir, DBFile)
}

// SchedParams are the scheduler parameters with defaults.
func (c *Config) SchedParams() sched.Params {
	p := sched.Defaults()
	if c.Scheduler.GateFreeGB != nil {
		p.GateFreeGB = *c.Scheduler.GateFreeGB
	}
	if c.Scheduler.GateCPU != nil {
		p.GateCPU = *c.Scheduler.GateCPU
	}
	for k, v := range c.Scheduler.MemGB {
		p.MemGB[k] = v
	}
	return p
}

// TTL of a certificate for role.
func (c *Config) TTL(role string) time.Duration {
	if role == "worker" {
		return c.Certs.WorkerTTL.Duration
	}
	return c.Certs.ClientTTL.Duration
}

// RenewBefore for a certificate of role.
func (c *Config) RenewBefore(role string) time.Duration {
	if c.Certs.RenewBefore.Duration > 0 {
		return c.Certs.RenewBefore.Duration
	}
	return c.TTL(role) / 3
}

// DefaultDataDir is $LONGARM_HUB_DATA_DIR, /var/lib/longarm-hub when writable as
// root, else ~/.longarm-hub.
func DefaultDataDir() string {
	if d := os.Getenv("LONGARM_HUB_DATA_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".longarm-hub"
	}
	return filepath.Join(home, ".longarm-hub")
}

// Data-dir layout.
const (
	DBFile      = "hub.db"
	CADir       = "ca"
	HubTLSDir   = "tls/hub"
	RelayTLSDir = "tls/relay"
	ReleasesDir = "releases"
)
