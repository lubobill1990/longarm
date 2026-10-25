package hub

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

// tailnetDefaults resolves the provider (folding the old [headscale] into
// [tailnet]) and fills in defaults.
func (c *Config) tailnetDefaults() error {
	t := &c.Tailnet
	if !headscaleEmpty(c.Headscale) {
		if !headscaleEmpty(t.Headscale) {
			return errors.New("both [headscale] and [tailnet.headscale] are set: keep one ([tailnet.headscale])")
		}
		t.Headscale = c.Headscale
		if t.EphemeralClients == nil && c.Headscale.Ephemeral {
			on := true
			t.EphemeralClients = &on
		}
	}
	h := &t.Headscale
	if len(t.WorkerTags) == 0 {
		t.WorkerTags = h.WorkerTags
	}
	if len(t.ClientTags) == 0 {
		t.ClientTags = h.ClientTags
	}
	if t.KeyTTL.Duration <= 0 {
		t.KeyTTL = h.KeyTTL
	}
	t.Provider = strings.ToLower(strings.TrimSpace(t.Provider))
	if t.Provider == "" {
		switch {
		case h.URL != "":
			t.Provider = ProviderHeadscale
		case t.Tailscale.OAuthSecretFile != "" || os.Getenv(EnvTailscaleSecret) != "":
			t.Provider = ProviderTailscale
		default:
			t.Provider = ProviderNone
		}
	}
	switch t.Provider {
	case ProviderNone:
	case ProviderHeadscale:
		if h.URL == "" {
			return errors.New(`[tailnet] provider = "headscale" needs [tailnet.headscale] url`)
		}
		if err := httpURL(h.URL); err != nil {
			return fmt.Errorf("[tailnet.headscale] url: %w", err)
		}
		if h.APIURL != "" {
			if err := httpURL(h.APIURL); err != nil {
				return fmt.Errorf("[tailnet.headscale] api_url: %w", err)
			}
		}
	case ProviderTailscale:
		ts := &t.Tailscale
		if ts.Tailnet == "" {
			ts.Tailnet = "-"
		}
		if ts.APIURL == "" {
			ts.APIURL = "https://api.tailscale.com"
		}
		ts.APIURL = strings.TrimRight(ts.APIURL, "/")
		if err := httpURL(ts.APIURL); err != nil {
			return fmt.Errorf("[tailnet.tailscale] api_url: %w", err)
		}
		if ts.ControlURL != "" {
			if err := httpURL(ts.ControlURL); err != nil {
				return fmt.Errorf("[tailnet.tailscale] control_url: %w", err)
			}
		}
		if ts.OAuthSecretFile == "" && os.Getenv(EnvTailscaleSecret) == "" {
			return errors.New(`[tailnet] provider = "tailscale" needs [tailnet.tailscale] oauth_secret_file (or ` + EnvTailscaleSecret + ")")
		}
	default:
		return fmt.Errorf("[tailnet] provider %q: want tailscale, headscale or none", t.Provider)
	}
	if len(t.WorkerTags) == 0 {
		t.WorkerTags = []string{"tag:longarm-worker"}
	}
	if len(t.ClientTags) == 0 {
		t.ClientTags = []string{"tag:longarm-client"}
	}
	for _, tag := range append(append([]string(nil), t.WorkerTags...), t.ClientTags...) {
		if !strings.HasPrefix(tag, "tag:") || len(tag) < 5 || strings.ContainsAny(tag, " ,") {
			return fmt.Errorf("[tailnet] tag %q: tags look like tag:name", tag)
		}
	}
	if t.KeyTTL.Duration <= 0 {
		t.KeyTTL.Duration = time.Hour
	}
	if t.KeyTTL.Duration < time.Minute || t.KeyTTL.Duration > 90*24*time.Hour {
		return fmt.Errorf("[tailnet] key_ttl %s: between 1m and 90 days", t.KeyTTL.Duration)
	}
	if t.EphemeralClients == nil {
		on := true
		t.EphemeralClients = &on
	}
	return nil
}

func httpURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%q: want an http(s) URL", s)
	}
	return nil
}

func headscaleEmpty(h HeadscaleConfig) bool {
	return h.URL == "" && h.APIURL == "" && h.APIKeyFile == "" && h.User == "" && len(h.WorkerTags) == 0 &&
		len(h.ClientTags) == 0 && !h.Ephemeral && h.KeyTTL.Duration == 0
}

// ControlURL is the control server nodes of this tailnet use ("" = none).
func (t *TailnetConfig) ControlURL() string {
	switch t.Provider {
	case ProviderTailscale:
		if t.Tailscale.ControlURL != "" {
			return strings.TrimRight(t.Tailscale.ControlURL, "/")
		}
		return DefaultTailscaleControlURL
	case ProviderHeadscale:
		return strings.TrimRight(t.Headscale.URL, "/")
	}
	return ""
}
