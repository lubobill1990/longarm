// Command longarm-relay is the longnet relay + rendezvous server (DESIGN §4.3).
// longarm-hub embeds the same relay; a standalone one serves setups without
// a hub, and - with --hub - is an external relay of a hub that keeps
// running while the hub restarts (docs/usage.md "External relays").
//
//	longarm-relay [--listen :7800] [--udp ADDR] [--tcp ADDR] [--stun2 auto|ADDR] [--tls-dir DIR] [--metrics 127.0.0.1:7801] [--rate 32M]
//	longarm-relay --hub hub.example.com:7443 --advertise relay.example.com [--state-dir DIR] ...
//	longarm-relay healthcheck [--addr 127.0.0.1:7801]
//
// STUN is answered on the UDP (QUIC) port and on a second port (--stun2,
// default UDP port + 1): nodes compare the two answers to detect a NAT
// whose mapping varies by destination ("hard").
//
// It needs a certificate with the relay role from the Longarm CA:
//
//	longarm admin issue --role relay --name relay1 --host relay.example.com --out DIR
//	longarm-hub issue --role relay --name relay1 --host relay.example.com --out DIR   (on the hub)
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
	"github.com/lubobill1990/longarm/internal/relay/hubsync"
	"github.com/lubobill1990/longarm/internal/version"
)

func defaultTLSDir() string {
	if d := os.Getenv("LONGARM_RELAY_TLS_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "tls"
	}
	return filepath.Join(home, ".longarm-relay", "tls")
}

func defaultStateDir() string {
	if d := os.Getenv("LONGARM_RELAY_STATE_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "state"
	}
	return filepath.Join(home, ".longarm-relay", "state")
}

// parseRate parses "0", "500K", "32M", "1G" (bytes per second, powers of 1024).
func parseRate(s string) (float64, error) {
	s = strings.TrimSpace(strings.ToUpper(strings.TrimSuffix(strings.TrimSuffix(s, "/s"), "B")))
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("bad rate %q (e.g. 32M = 32 MiB/s, 0 = unlimited)", s)
	}
	return v * mult, nil
}

func main() {
	var listen, udpAddr, tcpAddr, tlsDir, metricsAddr, rateStr, logFile, stun2Addr, stunAdvertise string
	var hubAddr, advertise, advUDP, advTCP, stateDir string
	var logInterval time.Duration
	var verbose bool
	root := &cobra.Command{
		Use:           "longarm-relay",
		Short:         "longnet relay and rendezvous server",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var logW io.Writer = os.Stderr
			if logFile != "" {
				f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
				if err != nil {
					return err
				}
				defer f.Close()
				logW = io.MultiWriter(os.Stderr, f)
			}
			level := slog.LevelInfo
			if verbose {
				level = slog.LevelDebug
			}
			log := slog.New(slog.NewTextHandler(logW, &slog.HandlerOptions{Level: level}))

			if udpAddr == "" && !cmd.Flags().Changed("udp") {
				udpAddr = listen
			}
			if tcpAddr == "" && !cmd.Flags().Changed("tcp") {
				tcpAddr = listen
			}
			if udpAddr == "" && tcpAddr == "" {
				return errors.New("nothing to listen on")
			}
			if hubAddr == "" && (advertise != "" || advUDP != "" || advTCP != "") {
				return errors.New("--advertise* registers the relay with a hub: needs --hub")
			}
			rate, err := parseRate(rateStr)
			if err != nil {
				return err
			}
			if err := installEnvBundle(tlsDir, log); err != nil {
				return err
			}
			b, err := pki.LoadBundle(tlsDir)
			if err != nil {
				return fmt.Errorf("%w (issue one with `longarm admin issue --role relay --name <name> --host <public name> --out %s`)", err, tlsDir)
			}
			b = b.Live() // renewals through the hub swap the certificate
			tlsConf, err := relay.ServerTLS(b)
			if err != nil {
				return err
			}
			if time.Until(b.NotAfter) < 7*24*time.Hour {
				log.Warn("relay certificate expires soon", "not_after", b.NotAfter)
			}
			srv := &relay.Server{TLS: tlsConf, Logger: log, Rate: rate, Burst: int(max(rate/4, 1<<20))}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			errc := make(chan error, 4)
			var stunEPs []string
			var quicPC, stunPC net.PacketConn // served once srv.STUN is final (Welcome reads it)
			if udpAddr != "" {
				pc, err := net.ListenPacket("udp", udpAddr)
				if err != nil {
					return err
				}
				defer pc.Close()
				udpAddr = pc.LocalAddr().String()
				stunEPs = append(stunEPs, fmt.Sprintf(":%d", pc.LocalAddr().(*net.UDPAddr).Port))
				if stun2Addr == "auto" { // same host, next port
					ua := pc.LocalAddr().(*net.UDPAddr)
					stun2Addr = net.JoinHostPort(hostOf(udpAddr), strconv.Itoa(ua.Port+1))
				}
				quicPC = pc
			} else if stun2Addr == "auto" {
				stun2Addr = ""
			}
			if stun2Addr != "" {
				// The second observation point for NAT type detection. Not
				// fatal: without it nodes still get their public address,
				// only the NAT type stays unknown.
				if pc, err := net.ListenPacket("udp", stun2Addr); err != nil {
					log.Warn("second STUN port unavailable; NAT type detection off", "addr", stun2Addr, "err", err)
					stun2Addr = ""
				} else {
					defer pc.Close()
					stun2Addr = pc.LocalAddr().String()
					stunEPs = append(stunEPs, fmt.Sprintf(":%d", pc.LocalAddr().(*net.UDPAddr).Port))
					stunPC = pc
				}
			}
			var adv []string
			if stunAdvertise != "" {
				adv = strings.Split(stunAdvertise, ",")
			}
			srv.STUN = relay.ExpandSTUN(adv, stunEPs)
			var tcpLn net.Listener
			if tcpAddr != "" {
				ln, err := net.Listen("tcp", tcpAddr)
				if err != nil {
					return err
				}
				tcpLn = ln
				tcpAddr = ln.Addr().String()
			}
			// --hub: the policy (revocations, worker pairs) comes from the
			// hub and is checked on every handshake: set before serving.
			var hs *hubsync.Sync
			if hubAddr != "" {
				pubUDP, pubTCP, err := advertised(advertise, advUDP, advTCP, udpAddr, tcpAddr)
				if err != nil {
					return err
				}
				hs, err = hubsync.New(hubsync.Config{Hub: hubAddr, Bundle: b, TLSDir: tlsDir, StateDir: stateDir,
					UDP: pubUDP, TCP: pubTCP, Server: srv, Logger: log, Version: version.Version})
				if err != nil {
					return err
				}
				tlsConf.VerifyPeerCertificate = hs.VerifyPeerCertificate
				go hs.Run(ctx)
				log.Info("relay: registering with the hub", "hub", hubAddr, "udp", pubUDP, "tcp", pubTCP, "state_dir", stateDir)
			}
			if quicPC != nil {
				go func() { errc <- fmt.Errorf("udp: %w", srv.ServeQUIC(quicPC)) }()
			}
			if stunPC != nil {
				go func() { errc <- fmt.Errorf("stun: %w", srv.ServeSTUN(stunPC)) }()
			}
			if tcpLn != nil {
				go func() { errc <- fmt.Errorf("tcp: %w", srv.ServeTCP(tcpLn)) }()
			}
			if metricsAddr != "" {
				ln, err := net.Listen("tcp", metricsAddr)
				if err != nil {
					return err
				}
				mux := http.NewServeMux()
				mux.Handle("/", srv.MetricsHandler())
				mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					ok := true
					st := map[string]any{"version": version.Version}
					if hs != nil {
						s := hs.Status()
						st["hub"], ok = s, s.Ready
					}
					st["ok"] = ok
					if !ok {
						w.WriteHeader(http.StatusServiceUnavailable)
					}
					json.NewEncoder(w).Encode(st)
				})
				hsrv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
				defer hsrv.Close()
				go hsrv.Serve(ln)
				metricsAddr = ln.Addr().String()
			}
			log.Info("longarm-relay started", "name", b.Identity.Name, "version", version.Version, "udp", udpAddr,
				"tcp", tcpAddr, "stun2", stun2Addr, "stun_advertised", srv.STUN, "metrics", metricsAddr, "rate_per_node", rateStr,
				"hub", hubAddr, "pid", os.Getpid())
			// Machine-readable line for scripts/tests.
			fmt.Printf("longarm-relay listening udp=%s tcp=%s metrics=%s\n", udpAddr, tcpAddr, metricsAddr)

			var tick <-chan time.Time
			if logInterval > 0 {
				t := time.NewTicker(logInterval)
				defer t.Stop()
				tick = t.C
			}
			var lastBytes uint64
			for {
				select {
				case <-ctx.Done():
					log.Info("longarm-relay stopping")
					srv.Close()
					return nil
				case err := <-errc:
					srv.Close()
					return err
				case <-tick:
					st := srv.Stats()
					log.Info("relay stats", "sessions", len(st.Sessions), "bytes_relayed", st.BytesRelayed,
						"bytes_since_last", st.BytesRelayed-lastBytes, "packets", st.PacketsRelayed,
						"drop_no_peer", st.DropNoPeer, "drop_rate", st.DropRateLimit, "drop_queue", st.DropQueueFull,
						"drop_policy", st.DropPolicy)
					lastBytes = st.BytesRelayed
				}
			}
		},
	}
	f := root.Flags()
	f.StringVar(&listen, "listen", ":7800", "address for both UDP (QUIC) and TCP (TLS fallback)")
	f.StringVar(&udpAddr, "udp", "", "UDP/QUIC listen address (default --listen; empty with the flag set = off)")
	f.StringVar(&tcpAddr, "tcp", "", "TLS/TCP listen address (default --listen; empty with the flag set = off), e.g. :443")
	f.StringVar(&stun2Addr, "stun2", "auto", "second UDP STUN port for NAT type detection (auto = UDP port + 1 on the same host; empty = off)")
	f.StringVar(&stunAdvertise, "stun-advertise", os.Getenv("LONGARM_RELAY_STUN_ADVERTISE"), "override the STUN endpoints told to nodes, comma-separated: \"self\" = this relay's STUN ports, \":port\" = this relay's IP, host:port (resolved by the nodes), \"off\" = none; e.g. \"self,stun.l.google.com:19302\"; env LONGARM_RELAY_STUN_ADVERTISE")
	f.StringVar(&tlsDir, "tls-dir", defaultTLSDir(), "relay certificate bundle (ca.crt, node.crt, node.key); env LONGARM_RELAY_TLS_DIR ("+EnvTLSBundle+": the bundle as base64 tar.gz, written here)")
	f.StringVar(&metricsAddr, "metrics", "", "serve /metrics (Prometheus), /sessions (JSON) and /healthz on this address, e.g. 127.0.0.1:7801")
	f.StringVar(&rateStr, "rate", "32M", "per-node relay rate limit in bytes/s (K/M/G suffixes; 0 = unlimited)")
	f.DurationVar(&logInterval, "log-interval", time.Minute, "log session/byte counters this often (0 = off)")
	f.StringVar(&logFile, "log-file", "", "also write logs to this file")
	f.BoolVarP(&verbose, "verbose", "v", false, "debug logging")
	f.StringVar(&hubAddr, "hub", os.Getenv("LONGARM_RELAY_HUB"), "register with this hub (host:port) as one of its external relays and enforce its policy (revocations, port-link pairs); env LONGARM_RELAY_HUB")
	f.StringVar(&advertise, "advertise", os.Getenv("LONGARM_RELAY_ADVERTISE"), "with --hub: the public host name nodes reach this relay at (with the bound ports); env LONGARM_RELAY_ADVERTISE")
	f.StringVar(&advUDP, "advertise-udp", "", "with --hub: the public UDP host:port (overrides --advertise for UDP)")
	f.StringVar(&advTCP, "advertise-tcp", "", "with --hub: the public TLS/TCP host:port (overrides --advertise for TCP)")
	f.StringVar(&stateDir, "state-dir", defaultStateDir(), "with --hub: where the last hub policy is kept (used while the hub is down); env LONGARM_RELAY_STATE_DIR")
	root.AddCommand(healthcheckCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "longarm-relay:", err)
		os.Exit(1)
	}
}

// hostOf is the host part of a "host:port" listen address ("" for ":port").
func hostOf(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return h
}

// advertised are the public endpoints registered with the hub: explicit
// --advertise-udp/--advertise-tcp, else --advertise's host with the bound
// ports.
func advertised(host, udp, tcp, boundUDP, boundTCP string) (string, string, error) {
	port := func(a string) string {
		_, p, err := net.SplitHostPort(a)
		if err != nil {
			return ""
		}
		return p
	}
	if udp == "" && host != "" && boundUDP != "" {
		udp = net.JoinHostPort(host, port(boundUDP))
	}
	if tcp == "" && host != "" && boundTCP != "" {
		tcp = net.JoinHostPort(host, port(boundTCP))
	}
	if udp == "" {
		return "", "", errors.New("--hub needs the relay's public address: --advertise HOST (or --advertise-udp HOST:PORT)")
	}
	for _, a := range []string{udp, tcp} {
		if a == "" {
			continue
		}
		if h, _, err := net.SplitHostPort(a); err != nil || h == "" {
			return "", "", fmt.Errorf("advertised address %q: want host:port with a host", a)
		}
	}
	return udp, tcp, nil
}

// EnvTLSBundle carries the relay's bundle as base64 of a tar.gz with
// ca.crt, node.crt and node.key (longarm-hub issue --role relay --b64; a
// container's secure environment variable). It is written to --tls-dir
// unless that already holds a later certificate of the same identity (a
// renewal through the hub).
const EnvTLSBundle = "LONGARM_RELAY_TLS_B64"

func installEnvBundle(dir string, log *slog.Logger) error {
	v := strings.TrimSpace(os.Getenv(EnvTLSBundle))
	if v == "" {
		return nil
	}
	files, err := untarBundle(v)
	if err != nil {
		return fmt.Errorf("%s: %w", EnvTLSBundle, err)
	}
	nb, err := pki.ParseBundle(files[pki.CACertFile], files[pki.NodeCertFile], files[pki.NodeKeyFile])
	if err != nil {
		return fmt.Errorf("%s: %w", EnvTLSBundle, err)
	}
	if cur, err := pki.LoadBundle(dir); err == nil && cur.Identity == nb.Identity && cur.NotAfter.After(nb.NotAfter) {
		log.Info("relay: keeping the renewed certificate in --tls-dir (later than the environment's)", "not_after", cur.NotAfter)
		return nil
	}
	return pki.WriteBundleAtomic(dir, files[pki.CACertFile], files[pki.NodeCertFile], files[pki.NodeKeyFile])
}

// untarBundle decodes EnvTLSBundle's format (whitespace ignored: a value
// copied from a terminal).
func untarBundle(b64 string) (map[string][]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(b64), ""))
	if err != nil {
		return nil, fmt.Errorf("not base64: %w", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("not a tar.gz: %w", err)
	}
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("not a tar.gz: %w", err)
		}
		name := filepath.Base(h.Name)
		if name != pki.CACertFile && name != pki.NodeCertFile && name != pki.NodeKeyFile {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 64<<10))
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
	for _, n := range []string{pki.CACertFile, pki.NodeCertFile, pki.NodeKeyFile} {
		if len(out[n]) == 0 {
			return nil, fmt.Errorf("the bundle lacks %s", n)
		}
	}
	return out, nil
}

// healthcheckCmd: GET /healthz on the --metrics address (container probes).
func healthcheckCmd() *cobra.Command {
	var addr string
	var ready bool
	c := &cobra.Command{
		Use:   "healthcheck",
		Short: "Exit 0 when the relay serving --metrics on --addr answers (--ready: and, with --hub, has a policy)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			hc := &http.Client{Timeout: 5 * time.Second}
			resp, err := hc.Get("http://" + addr + "/healthz")
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			if resp.StatusCode != http.StatusOK && (ready || resp.StatusCode != http.StatusServiceUnavailable) {
				return fmt.Errorf("unhealthy (%s): %s", resp.Status, strings.TrimSpace(string(body)))
			}
			return nil
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:7801", "the relay's --metrics address")
	c.Flags().BoolVar(&ready, "ready", false, "also require the hub policy (a relay waiting for its hub is alive, not ready)")
	return c
}
