package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/acme/autocert"

	"github.com/lubobill1990/longarm/internal/hub/store"
)

func TestConfigHTTPS(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hub.toml")
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("public_url = \"https://hub.example.com/\"\nhttps = \":443\"\nhttps_redirect = \":80\"\n")
	c, err := LoadConfig(p)
	if err != nil || c.ACMEHost() != "hub.example.com" || c.HTTPS != ":443" || c.HTTPSRedirect != ":80" {
		t.Fatalf("https: %+v %v", c, err)
	}
	write("public_url = \"https://hub.example.com\"\nhttps = \"off\"\n")
	if c, err = LoadConfig(p); err != nil || c.HTTPS != "" {
		t.Fatalf("off: %+v %v", c, err)
	}
	for _, bad := range []string{
		"https = \":443\"\n", // no public_url
		"public_url = \"http://hub.example.com\"\nhttps = \":443\"\n",       // not https
		"public_url = \"https://hub.example.com:8443\"\nhttps = \":443\"\n", // TLS-ALPN-01 is on 443
		"public_url = \"https://hub.example.com\"\nhttps = \"443\"\n",       // no host:port
		"https_redirect = \":80\"\n",                                        // without https
	} {
		write(bad)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("accepted: %q", bad)
		}
	}

	write("")
	t.Setenv(EnvPublicURL, "https://myhub.eastasia.azurecontainer.io")
	t.Setenv(EnvHTTPS, ":443")
	t.Setenv(EnvRedirect, ":80")
	t.Setenv(EnvACMEEmail, "ops@example.com")
	t.Setenv(EnvACMEDir, "https://acme-staging-v02.api.letsencrypt.org/directory")
	c, err = LoadConfig(p)
	if err != nil || c.ACMEHost() != "myhub.eastasia.azurecontainer.io" || c.HTTPSRedirect != ":80" ||
		c.ACMEEmail != "ops@example.com" || c.ACMEDirectory == "" {
		t.Fatalf("env: %+v %v", c, err)
	}
}

func TestACMECache(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := acmeCache{st: st}
	ctx := context.Background()
	if _, err := c.Get(ctx, "hub.example.com"); !errors.Is(err, autocert.ErrCacheMiss) {
		t.Fatalf("empty: %v", err)
	}
	if err := c.Put(ctx, "hub.example.com", []byte("autocert cache entry\n")); err != nil {
		t.Fatal(err)
	}
	if b, err := c.Get(ctx, "hub.example.com"); err != nil || string(b) != "autocert cache entry\n" {
		t.Fatalf("get: %q %v", b, err)
	}
	if v, _ := st.Meta(acmeMetaPrefix + "hub.example.com"); v == "" {
		t.Fatal("not in the meta table")
	}
	if err := c.Delete(ctx, "hub.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "hub.example.com"); !errors.Is(err, autocert.ErrCacheMiss) {
		t.Fatalf("deleted: %v", err)
	}
}

// The scheduler gate from the environment (containers have no hub.toml to edit).
func TestConfigGateEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hub.toml")
	os.WriteFile(p, nil, 0o600)
	t.Setenv(EnvGateFreeGB, "1.5")
	t.Setenv(EnvGateCPU, "90")
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if sp := c.SchedParams(); sp.GateFreeGB != 1.5 || sp.GateCPU != 90 {
		t.Fatalf("gate %+v", sp)
	}
	t.Setenv(EnvGateFreeGB, "lots")
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("bad LONGARM_HUB_SCHED_GATE_FREE_GB accepted")
	}
}

// The release signing key from the environment (an ACI hub's hub.toml is
// generated), winning over hub.toml.
func TestConfigReleasePublicKeyEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hub.toml")
	os.WriteFile(p, []byte("[releases]\npublic_key = \"RWfromtoml\"\n"), 0o600)
	c, err := LoadConfig(p)
	if err != nil || c.Releases.PublicKey != "RWfromtoml" {
		t.Fatalf("toml: %q %v", c.Releases.PublicKey, err)
	}
	t.Setenv(EnvReleasePublicKey, " RWfromenv ")
	if c, err = LoadConfig(p); err != nil || c.Releases.PublicKey != "RWfromenv" {
		t.Fatalf("env: %q %v", c.Releases.PublicKey, err)
	}
}
