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
	EnvDB        = "LONGARM_HUB_DB"          // db_path
	EnvListen    = "LONGARM_HUB_LISTEN"      // listen
	EnvHTTP      = "LONGARM_HUB_HTTP"        // http
	EnvPublicURL = "LONGARM_HUB_PUBLIC_URL"  // public_url
	EnvRelayUDP  = "LONGARM_HUB_RELAY_UDP"   // relay_udp
	EnvSTUN2     = "LONGARM_HUB_RELAY_STUN2" // relay_stun2
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
)

func (c *Config) applyEnv() error {
	for _, kv := range []struct {
		env string
		dst *string
	}{{EnvDB, &c.DBPath}, {EnvListen, &c.Listen}, {EnvHTTP, &c.HTTP}, {EnvPublicURL, &c.PublicURL},
		{EnvRelayUDP, &c.RelayUDP}, {EnvSTUN2, &c.RelaySTUN2}} {
		if v := strings.TrimSpace(os.Getenv(kv.env)); v != "" {
			*kv.dst = v
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvSTUNAdvertise)); v != "" {
		c.RelaySTUNAdvertise = nil
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				c.RelaySTUNAdvertise = append(c.RelaySTUNAdvertise, s)
			}
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
