package hub

import (
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Environment overrides for containers (deploy/azure, deploy/hub): set, they
// win over hub.toml, so one image runs with a generated or no hub.toml.
// LONGARM_HUB_DATA_DIR is older and only a default (hub.toml data_dir wins).
const (
	EnvDB        = "LONGARM_HUB_DB"             // db_path
	EnvListen    = "LONGARM_HUB_LISTEN"         // listen
	EnvHTTP      = "LONGARM_HUB_HTTP"           // http
	EnvPublicURL = "LONGARM_HUB_PUBLIC_URL"     // public_url
	EnvRelayUDP  = "LONGARM_HUB_RELAY_UDP"      // relay_udp
	EnvSTUN2     = "LONGARM_HUB_RELAY_STUN2"    // relay_stun2
	EnvCAInDB    = "LONGARM_HUB_CA_IN_DB"       // ca_in_db (true/false, 1/0)
	EnvHTTPS     = "LONGARM_HUB_HTTPS"          // https
	EnvRedirect  = "LONGARM_HUB_HTTPS_REDIRECT" // https_redirect
	EnvACMEEmail = "LONGARM_HUB_ACME_EMAIL"     // acme_email
	EnvACMEDir   = "LONGARM_HUB_ACME_DIRECTORY" // acme_directory
	// EnvSTUNAdvertise is relay_stun_advertise, comma-separated, e.g.
	// "stun.cloudflare.com:3478,stun.l.google.com:19302" (a hub whose own
	// STUN answers are useless behind its platform's source NAT, like Azure
	// Container Instances) or "self,stun.example.net:3478". "off" (or
	// "none") advertises no STUN server at all.
	EnvSTUNAdvertise = "LONGARM_HUB_RELAY_STUN_ADVERTISE"
	// EnvAdvertise is the host (or host:port) nodes dial, e.g. the FQDN of a
	// cloud container: public_addr = host:<listen port>, and host joins the
	// certificate's SANs (hosts).
	EnvAdvertise = "LONGARM_HUB_ADVERTISE"
	// [scheduler] gate_free_gb / gate_cpu: a hub whose workers are small
	// (containers, laptops) needs a lower memory gate than the default 20 GB
	EnvGateFreeGB = "LONGARM_HUB_SCHED_GATE_FREE_GB"
	EnvGateCPU    = "LONGARM_HUB_SCHED_GATE_CPU"
	// EnvReleasePublicKey is [releases] public_key, the minisign public key
	// line (RW...): with it the hub refuses uploaded releases whose
	// signature doesn't verify.
	EnvReleasePublicKey = "LONGARM_HUB_RELEASES_PUBLIC_KEY"
	// [auth.entra]; the lists are comma-separated.
	EnvEntraTenant          = "LONGARM_HUB_ENTRA_TENANT_ID"
	EnvEntraClient          = "LONGARM_HUB_ENTRA_CLIENT_ID"
	EnvEntraScope           = "LONGARM_HUB_ENTRA_SCOPE"
	EnvEntraAdmins          = "LONGARM_HUB_ENTRA_ADMINS"
	EnvEntraAllowed         = "LONGARM_HUB_ENTRA_ALLOWED"
	EnvEntraClientAssertion = "LONGARM_HUB_ENTRA_CLIENT_ASSERTION"
	EnvEntraSecretFile      = "LONGARM_HUB_ENTRA_CLIENT_SECRET_FILE"
	EnvEntraWeb             = "LONGARM_HUB_ENTRA_WEB"
	EnvEntraApps            = "LONGARM_HUB_ENTRA_APPS"
	// [tailnet] provider and [tailnet.headscale]; the booleans take
	// true/false or 1/0.
	EnvTailnetProvider     = "LONGARM_HUB_TAILNET_PROVIDER"
	EnvHeadscaleURL        = "LONGARM_HUB_HEADSCALE_URL"
	EnvHeadscaleAPIURL     = "LONGARM_HUB_HEADSCALE_API_URL"
	EnvHeadscaleAPIKeyFile = "LONGARM_HUB_HEADSCALE_API_KEY_FILE"
	EnvHeadscaleDir        = "LONGARM_HUB_HEADSCALE_DIR" // sidecar_dir
	EnvHeadscaleProxy      = "LONGARM_HUB_HEADSCALE_PROXY"
	EnvHeadscalePublicDERP = "LONGARM_HUB_HEADSCALE_PUBLIC_DERP"
)

func (c *Config) applyEnv() error {
	for _, kv := range []struct {
		env string
		dst **float64
	}{{EnvGateFreeGB, &c.Scheduler.GateFreeGB}, {EnvGateCPU, &c.Scheduler.GateCPU}} {
		if v := strings.TrimSpace(os.Getenv(kv.env)); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 {
				return fmt.Errorf("%s=%q: want a number >= 0", kv.env, v)
			}
			*kv.dst = &f
		}
	}
	for _, kv := range []struct {
		env string
		dst *string
	}{{EnvDB, &c.DBPath}, {EnvListen, &c.Listen}, {EnvHTTP, &c.HTTP}, {EnvPublicURL, &c.PublicURL},
		{EnvRelayUDP, &c.RelayUDP}, {EnvSTUN2, &c.RelaySTUN2}, {EnvHTTPS, &c.HTTPS}, {EnvRedirect, &c.HTTPSRedirect},
		{EnvACMEEmail, &c.ACMEEmail}, {EnvACMEDir, &c.ACMEDirectory},
		{EnvEntraTenant, &c.Auth.Entra.TenantID}, {EnvEntraClient, &c.Auth.Entra.ClientID}, {EnvEntraScope, &c.Auth.Entra.Scope},
		{EnvEntraClientAssertion, &c.Auth.Entra.ClientAssertion}, {EnvEntraSecretFile, &c.Auth.Entra.ClientSecretFile},
		{EnvEntraWeb, &c.Auth.Entra.Web}, {EnvAppInsights, &c.Telemetry.ConnectionString},
		{EnvReleasePublicKey, &c.Releases.PublicKey},
		{EnvTailnetProvider, &c.Tailnet.Provider}, {EnvHeadscaleURL, &c.Tailnet.Headscale.URL},
		{EnvHeadscaleAPIURL, &c.Tailnet.Headscale.APIURL}, {EnvHeadscaleAPIKeyFile, &c.Tailnet.Headscale.APIKeyFile},
		{EnvHeadscaleDir, &c.Tailnet.Headscale.SidecarDir}} {
		if v := strings.TrimSpace(os.Getenv(kv.env)); v != "" {
			*kv.dst = v
		}
	}
	for _, kv := range []struct {
		env string
		dst *[]string
	}{{EnvSTUNAdvertise, &c.RelaySTUNAdvertise}, {EnvEntraAdmins, &c.Auth.Entra.Admins}, {EnvEntraAllowed, &c.Auth.Entra.Allowed},
		{EnvEntraApps, &c.Auth.Entra.Apps}} {
		if v := strings.TrimSpace(os.Getenv(kv.env)); v != "" {
			*kv.dst = nil
			for _, s := range strings.Split(v, ",") {
				if s = strings.TrimSpace(s); s != "" {
					*kv.dst = append(*kv.dst, s)
				}
			}
		}
	}
	for _, kv := range []struct {
		env string
		dst *bool
	}{{EnvCAInDB, &c.CAInDB}, {EnvHeadscaleProxy, &c.Tailnet.Headscale.Proxy}, {EnvHeadscalePublicDERP, &c.Tailnet.Headscale.PublicDERP}} {
		if v := strings.TrimSpace(os.Getenv(kv.env)); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("%s=%q: want true or false", kv.env, v)
			}
			*kv.dst = b
		}
	}
	adv := strings.TrimSpace(os.Getenv(EnvAdvertise))
	if adv == "" {
		return nil
	}
	host, port, err := net.SplitHostPort(adv)
	if err != nil {
		// no port: the listen port
		host, port = adv, strconv.Itoa(DefaultPort)
		if c.Listen != "" {
			if _, p, err := net.SplitHostPort(c.Listen); err == nil && p != "" {
				port = p
			}
		}
	}
	host = strings.Trim(host, "[]")
	if host == "" || strings.ContainsAny(host, "/ ") {
		return fmt.Errorf("%s=%q: want host or host:port", EnvAdvertise, adv)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s=%q: bad port", EnvAdvertise, adv)
	}
	c.PublicAddr = net.JoinHostPort(host, port)
	if !slices.Contains(c.Hosts, host) {
		c.Hosts = append(c.Hosts, host)
	}
	return nil
}
