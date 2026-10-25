package transport

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseListen(t *testing.T) {
	tr, err := ParseListen("tcp://127.0.0.1:7701", "")
	if err != nil || tr.Name() != "tcp" || tr.(*TCP).Addr != "127.0.0.1:7701" {
		t.Fatalf("tcp: %v %v", tr, err)
	}
	t.Setenv("TS_AUTHKEY", "tskey-auth-test")
	tr, err = ParseListen("tailscale://longarm-box:7800?control=https://hs.example.com&ephemeral=1", "/state")
	if err != nil {
		t.Fatal(err)
	}
	cfg := tr.(*TSNet).Config()
	// One state dir per control server.
	want := TSNetConfig{Hostname: "longarm-box", Dir: "/state-hs.example.com", AuthKey: "tskey-auth-test", ControlURL: "https://hs.example.com", Port: 7800, Ephemeral: true}
	if cfg.Hostname != want.Hostname || cfg.Dir != want.Dir || cfg.AuthKey != want.AuthKey || cfg.AuthKeyFile != want.Dir+".authkey" ||
		cfg.ControlURL != want.ControlURL || cfg.Port != want.Port || cfg.Ephemeral != want.Ephemeral || cfg.KeyFunc != nil {
		t.Fatalf("tsnet cfg = %+v", cfg)
	}
	// Keys from the hub (Env.TailnetKey) when there is no TS_AUTHKEY, for
	// listeners with a control server only.
	t.Setenv("TS_AUTHKEY", "")
	var asked string
	env := Env{StateDir: "/state", TailnetKey: func(_ context.Context, control, reason string) (string, error) {
		asked = control + " " + reason
		return "k", nil
	}}
	tr, _ = ParseListenEnv("tailscale://longarm-box?control=https://hs.example.com", env)
	if kf := tr.(*TSNet).Config().KeyFunc; kf == nil {
		t.Fatal("no KeyFunc")
	} else if k, _ := kf(context.Background(), "no state"); k != "k" || asked != "https://hs.example.com no state" {
		t.Fatalf("KeyFunc: %q %q", k, asked)
	}
	tr, _ = ParseListenEnv("tailscale://longarm-box", env)
	if c := tr.(*TSNet).Config(); c.KeyFunc != nil || c.Dir != "/state" {
		t.Fatalf("without control: %+v", c)
	}
	tr, _ = ParseListen("tailscale://longarm-box?dir=/x", "/state")
	if c := tr.(*TSNet).Config(); c.Port != DefaultPort || c.Dir != "/x" {
		t.Fatalf("defaults: %+v", c)
	}
	for _, bad := range []string{"udp://x:1", "tcp://", "tailscale://h:notaport"} {
		if _, err := ParseListen(bad, ""); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestClassifyPath(t *testing.T) {
	cases := []struct {
		cur, pr, derp string
		want          PathKind
	}{
		{"1.2.3.4:41641", "", "fra", PathDirect},
		{"", "5.6.7.8:7:1", "fra", PathPeerRelay},
		{"", "", "fra", PathDERP},
		{"", "", "", PathUnknown},
	}
	for _, c := range cases {
		if got, _ := ClassifyPath(c.cur, c.pr, c.derp); got != c.want {
			t.Errorf("%+v -> %s", c, got)
		}
	}
}

func TestTCPRoundTrip(t *testing.T) {
	ctx := context.Background()
	srv := &TCP{Addr: "127.0.0.1:0"}
	ln, err := srv.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			io.Copy(c, c)
			c.Close()
		}
	}()
	c, pi, err := (&TCP{}).Dial(ctx, NodeID(ln.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if pi.Kind != PathDirect || pi.Transport != "tcp" {
		t.Fatalf("path %v", pi)
	}
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
}

func TestTSNetConfigErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := NewTSNet(TSNetConfig{Dir: t.TempDir()}).Listen(ctx); err == nil {
		t.Fatal("missing hostname accepted")
	}
	if _, _, err := NewTSNet(TSNetConfig{Hostname: "x"}).Dial(ctx, "y"); err == nil {
		t.Fatal("missing dir accepted")
	}
}

// TestTSNetIntegration brings up two ephemeral tsnet nodes and dials one from
// the other. Needs TS_AUTHKEY (a reusable, ephemeral-capable key).
func TestTSNetIntegration(t *testing.T) {
	key := os.Getenv("TS_AUTHKEY")
	if key == "" {
		t.Skip("TS_AUTHKEY not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dir := t.TempDir()
	suffix := time.Now().Format("150405")
	srv := NewTSNet(TSNetConfig{Hostname: "latest-srv-" + suffix, Dir: filepath.Join(dir, "s"), AuthKey: key, Ephemeral: true, Port: 7701})
	defer srv.Close()
	ln, err := srv.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		c, err := ln.Accept()
		if err == nil {
			io.Copy(c, c)
			c.Close()
		}
	}()
	cli := NewTSNet(TSNetConfig{Hostname: "latest-cli-" + suffix, Dir: filepath.Join(dir, "c"), AuthKey: key, Ephemeral: true})
	defer cli.Close()
	c, pi, err := cli.Dial(ctx, NodeID("latest-srv-"+suffix+":7701"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t.Logf("path: %v", pi)
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
}
