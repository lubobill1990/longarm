package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
)

func TestConfigEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hub.toml")
	os.WriteFile(p, []byte("listen = \":8443\"\nhttp = \"0.0.0.0:7480\"\nhosts = [\"hub.example.com\"]\n"), 0o600)

	c, err := LoadConfig(p)
	if err != nil || c.PublicAddr != "" || c.DatabasePath() != filepath.Join(c.DataDir, DBFile) {
		t.Fatalf("no env: %+v %v", c, err)
	}

	t.Setenv(EnvAdvertise, "myhub.eastasia.azurecontainer.io")
	t.Setenv(EnvDB, filepath.Join(dir, "local", "hub.db"))
	t.Setenv(EnvHTTP, "127.0.0.1:7480")
	t.Setenv(EnvPublicURL, "https://myhub.eastasia.azurecontainer.io/")
	c, err = LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.PublicAddr != "myhub.eastasia.azurecontainer.io:8443" || c.HTTP != "127.0.0.1:7480" ||
		c.PublicURL != "https://myhub.eastasia.azurecontainer.io" || c.DatabasePath() != filepath.Join(dir, "local", "hub.db") {
		t.Fatalf("env: %+v", c)
	}
	if hs := c.serverHosts(); !strings.Contains(strings.Join(hs, ","), "myhub.eastasia.azurecontainer.io") || len(c.Hosts) != 2 {
		t.Fatalf("hosts %v / %v", hs, c.Hosts)
	}

	t.Setenv(EnvListen, ":9443")
	t.Setenv(EnvAdvertise, "203.0.113.10:7443")
	if c, err = LoadConfig(p); err != nil || c.Listen != ":9443" || c.PublicAddr != "203.0.113.10:7443" || c.RelayUDP != ":9443" {
		t.Fatalf("listen/advertise: %+v %v", c, err)
	}
	t.Setenv(EnvAdvertise, "[2001:db8::1]")
	if c, err = LoadConfig(p); err != nil || c.PublicAddr != "[2001:db8::1]:9443" {
		t.Fatalf("ipv6: %+v %v", c, err)
	}
	for _, bad := range []string{"http://x.example.com", "x.example.com:0", "x.example.com:http", ":7443"} {
		t.Setenv(EnvAdvertise, bad)
		if _, err := LoadConfig(p); err == nil {
			t.Errorf("advertise %q accepted", bad)
		}
	}
	t.Setenv(EnvAdvertise, "")

	// relay_stun_advertise from the environment: names, "self", ":port".
	t.Setenv(EnvSTUNAdvertise, " stun.example.net:3478, self ,:7444,")
	if c, err = LoadConfig(p); err != nil || strings.Join(c.RelaySTUNAdvertise, ",") != "stun.example.net:3478,self,:7444" {
		t.Fatalf("stun advertise: %+v %v", c, err)
	}
	t.Setenv(EnvSTUNAdvertise, "off")
	if c, err = LoadConfig(p); err != nil || strings.Join(c.RelaySTUNAdvertise, ",") != "off" {
		t.Fatalf("stun advertise off: %+v %v", c, err)
	}
	t.Setenv(EnvSTUNAdvertise, "stun.example.net")
	if _, err := LoadConfig(p); err == nil {
		t.Error("stun advertise without port accepted")
	}
}

func TestBootstrap(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvDB, filepath.Join(root, "local", "hub.db"))
	t.Setenv(EnvAdvertise, "hub.example.com")
	t.Setenv("LONGARM_HUB_DATA_DIR", filepath.Join(root, "share"))
	cfgPath := filepath.Join(root, "share", "hub.toml")
	if wrote, err := WriteConfigTemplate(cfgPath); err != nil || !wrote {
		t.Fatal(wrote, err)
	}
	if wrote, err := WriteConfigTemplate(cfgPath); err != nil || wrote {
		t.Fatal("template rewritten", err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	r1, err := Bootstrap(cfg, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !r1.NewCA || r1.AdminCode == "" || r1.AdminJoined || r1.HubAddress != "hub.example.com:7443" || len(r1.Fingerprint) != 64 ||
		time.Until(r1.AdminCodeExpires) < 59*time.Minute {
		t.Fatalf("first: %+v", r1)
	}
	// database on "local disk", everything else on the share
	if _, err := os.Stat(filepath.Join(root, "local", "hub.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "share", DBFile)); err == nil {
		t.Fatal("hub.db in the data dir")
	}
	kv := readKV(filepath.Join(root, "share", BootstrapFile))
	if kv["admin_join_code"] != r1.AdminCode || kv["ca_fingerprint"] != r1.Fingerprint || kv["admin_joined"] != "false" {
		t.Fatalf("bootstrap.env %v", kv)
	}
	if l := r1.LogLine(); !strings.HasPrefix(l, BootstrapLogPrefix+" boot_id="+r1.BootID+" ca_fingerprint="+r1.Fingerprint) ||
		strings.Contains(l, r1.AdminCode) || len(r1.BootID) != 16 {
		t.Fatal("log line (never the code):", l)
	}

	// a restart keeps the CA and the still valid code
	r2, err := Bootstrap(cfg, time.Hour, nil)
	if err != nil || r2.NewCA || r2.AdminCode != r1.AdminCode || r2.Fingerprint != r1.Fingerprint || r2.BootID == r1.BootID {
		t.Fatalf("second: %+v %v", r2, err)
	}

	// a database without the code (lost, or the code was used by a node
	// that is gone): a new code
	os.Remove(filepath.Join(root, "local", "hub.db"))
	r3, err := Bootstrap(cfg, time.Hour, nil)
	if err != nil || r3.AdminCode == "" || r3.AdminCode == r1.AdminCode || r3.Fingerprint != r1.Fingerprint {
		t.Fatalf("third: %+v %v", r3, err)
	}

	// once an admin joined: no code any more
	st, err := store.Open(cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutNode(&store.Node{Name: "laptop", Role: string(pki.RoleAdmin), State: stateApproved, JoinedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	r4, err := Bootstrap(cfg, time.Hour, nil)
	if err != nil || !r4.AdminJoined || r4.AdminCode != "" {
		t.Fatalf("fourth: %+v %v", r4, err)
	}
	if kv := readKV(filepath.Join(root, "share", BootstrapFile)); kv["admin_join_code"] != "" || kv["admin_joined"] != "true" {
		t.Fatalf("bootstrap.env after join %v", kv)
	}

	// no new CA next to a database with nodes (lost or wrong share): half a
	// CA, or none at all
	os.Remove(filepath.Join(root, "share", CADir, pki.CAKeyFile))
	if _, err := Bootstrap(cfg, time.Hour, nil); err == nil || !strings.Contains(err.Error(), "has 1 nodes") {
		t.Fatalf("incomplete CA with nodes: %v", err)
	}
	os.RemoveAll(filepath.Join(root, "share", CADir))
	if _, err := Bootstrap(cfg, time.Hour, nil); err == nil || !strings.Contains(err.Error(), "ca import") {
		t.Fatalf("no CA with nodes: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "share", CADir)); err == nil {
		t.Fatal("a CA was created")
	}

	// half a CA on a hub that never issued anything (an interrupted first
	// start): set aside, a new CA
	fresh := filepath.Join(root, "fresh")
	t.Setenv(EnvDB, filepath.Join(fresh, "hub.db"))
	t.Setenv("LONGARM_HUB_DATA_DIR", fresh)
	cfg2, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InitCA(fresh); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(fresh, CADir, pki.CAKeyFile))
	r5, err := Bootstrap(cfg2, time.Hour, nil)
	if err != nil || !r5.NewCA || r5.AdminCode == "" {
		t.Fatalf("interrupted first start: %+v %v", r5, err)
	}
	if m, _ := filepath.Glob(filepath.Join(fresh, CADir+".incomplete-*")); len(m) != 1 {
		t.Fatalf("incomplete CA not kept aside: %v", m)
	}
}
