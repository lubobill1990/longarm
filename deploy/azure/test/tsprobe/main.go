// tsprobe registers a tailscale node through a hub's tailnet, the way
// longarm's embedded node does: it asks the hub for a key (Fleet.TailnetKey,
// with the certificate of a joined longarm) and brings a tsnet node up on the
// control URL the hub names (container-smoke.sh: a Headscale sidecar behind
// the hub's proxy, so /key and the /ts2021 Noise upgrade go through it).
//
//	tsprobe -config-dir <longarm config dir> -hub <host:port> [-timeout 90s] [-hold 2m]
//
// Prints "tsprobe: up <control url> <tailnet IPs>" once the node is Running
// and exits 0 (closing the node: an ephemeral node is then logged out and
// deleted), or with -hold stays up that long (killed meanwhile, it leaves
// the node registered).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"tailscale.com/tsnet"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
)

func main() {
	dir := flag.String("config-dir", "", "the config dir of a joined longarm (LONGARM_CONFIG_DIR)")
	hub := flag.String("hub", "", "hub address host:port")
	timeout := flag.Duration("timeout", 90*time.Second, "give up after")
	verbose := flag.Bool("v", false, "tsnet's logs")
	hold := flag.Duration("hold", 0, "stay up this long after the node is Running")
	flag.Parse()
	if *dir == "" || *hub == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*dir, *hub, *timeout, *hold, *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "tsprobe:", err)
		os.Exit(1)
	}
}

func run(dir, hub string, timeout, hold time.Duration, verbose bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	b, err := pki.LoadBundle(filepath.Join(dir, client.TLSDir))
	if err != nil {
		return err
	}
	t, err := hubclient.FetchTailnetKey(ctx, hub, b, "smoke")
	if err != nil {
		return fmt.Errorf("tailnet key from the hub: %w", err)
	}
	state, err := os.MkdirTemp("", "tsprobe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(state)
	s := &tsnet.Server{Dir: state, Hostname: "smoke-probe", ControlURL: t.ControlUrl, AuthKey: t.AuthKey, Ephemeral: true,
		Logf: func(string, ...any) {}}
	if verbose {
		s.Logf = nil // tsnet's default: the log
	}
	defer s.Close()
	st, err := s.Up(ctx)
	if err != nil {
		return fmt.Errorf("tsnet up on %s: %w", t.ControlUrl, err)
	}
	fmt.Printf("tsprobe: up %s %v\n", t.ControlUrl, st.TailscaleIPs)
	time.Sleep(hold)
	return nil
}
