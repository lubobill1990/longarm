// Command longarm-relay is the longnet relay + rendezvous server (DESIGN §4.3),
// standalone until it is folded into longarm-hub (P1).
//
//	longarm-relay [--listen :7800] [--udp ADDR] [--tcp ADDR] [--stun2 auto|ADDR] [--tls-dir DIR] [--metrics 127.0.0.1:7801] [--rate 32M]
//
// STUN is answered on the UDP (QUIC) port and on a second port (--stun2,
// default UDP port + 1): nodes compare the two answers to detect a NAT
// whose mapping varies by destination ("hard").
//
// It needs a certificate with the relay role from the Longarm CA:
//
//	longarm admin issue --role relay --name relay1 --host relay.example.com --out DIR
package main

import (
	"context"
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
			rate, err := parseRate(rateStr)
			if err != nil {
				return err
			}
			b, err := pki.LoadBundle(tlsDir)
			if err != nil {
				return fmt.Errorf("%w (issue one with `longarm admin issue --role relay --name <name> --host <public name> --out %s`)", err, tlsDir)
			}
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
			if quicPC != nil {
				go func() { errc <- fmt.Errorf("udp: %w", srv.ServeQUIC(quicPC)) }()
			}
			if stunPC != nil {
				go func() { errc <- fmt.Errorf("stun: %w", srv.ServeSTUN(stunPC)) }()
			}
			if tcpAddr != "" {
				ln, err := net.Listen("tcp", tcpAddr)
				if err != nil {
					return err
				}
				go func() { errc <- fmt.Errorf("tcp: %w", srv.ServeTCP(ln)) }()
				tcpAddr = ln.Addr().String()
			}
			if metricsAddr != "" {
				ln, err := net.Listen("tcp", metricsAddr)
				if err != nil {
					return err
				}
				hs := &http.Server{Handler: srv.MetricsHandler(), ReadHeaderTimeout: 10 * time.Second}
				defer hs.Close()
				go hs.Serve(ln)
				metricsAddr = ln.Addr().String()
			}
			log.Info("longarm-relay started", "name", b.Identity.Name, "version", version.Version, "udp", udpAddr,
				"tcp", tcpAddr, "stun2", stun2Addr, "stun_advertised", srv.STUN, "metrics", metricsAddr, "rate_per_node", rateStr, "pid", os.Getpid())
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
	f.StringVar(&stunAdvertise, "stun-advertise", "", "override the STUN endpoints told to nodes, comma-separated: \"self\" = this relay's STUN ports, \":port\" = this relay's IP, host:port (resolved by the nodes), \"off\" = none; e.g. \"self,stun.l.google.com:19302\"")
	f.StringVar(&tlsDir, "tls-dir", defaultTLSDir(), "relay certificate bundle (ca.crt, node.crt, node.key); env LONGARM_RELAY_TLS_DIR")
	f.StringVar(&metricsAddr, "metrics", "", "serve /metrics (Prometheus) and /sessions (JSON) on this address, e.g. 127.0.0.1:7801")
	f.StringVar(&rateStr, "rate", "32M", "per-node relay rate limit in bytes/s (K/M/G suffixes; 0 = unlimited)")
	f.DurationVar(&logInterval, "log-interval", time.Minute, "log session/byte counters this often (0 = off)")
	f.StringVar(&logFile, "log-file", "", "also write logs to this file")
	f.BoolVarP(&verbose, "verbose", "v", false, "debug logging")
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
