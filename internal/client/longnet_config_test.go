package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigLongNet(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, WorkersFile), []byte(`
[longnet]
relay = "relay.example.com:7800"
mode = "tcp"
disable_direct = true

[[worker]]
name = "w1"
transport = "longnet"

[[worker]]
name = "w2"
transport = "longnet"
address = "other-relay:7800"
cert_name = "worker-2"
`), 0o644)
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	w1, _ := c.Worker("w1")
	w2, _ := c.Worker("w2")
	if w1.Address != "relay.example.com:7800" || w2.Address != "other-relay:7800" || w2.CertName != "worker-2" {
		t.Fatalf("w1 = %+v, w2 = %+v", w1, w2)
	}
	if c.LongNet.Mode != "tcp" || !c.LongNet.DisableDirect {
		t.Fatalf("longnet = %+v", c.LongNet)
	}

	// longnet without any relay address is an error.
	os.WriteFile(filepath.Join(dir, WorkersFile), []byte("[[worker]]\nname = \"w1\"\ntransport = \"longnet\"\n"), 0o644)
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("longnet worker without relay accepted")
	}
}
