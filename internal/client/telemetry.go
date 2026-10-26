package client

import (
	"context"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/telemetry"
	"github.com/lubobill1990/longarm/internal/transport"
	"github.com/lubobill1990/longarm/internal/version"
)

// recordDial reports a worker dial (the transport race) to telemetry:
// which leg won, how each leg failed (error class only, never addresses),
// how long it took.
func recordDial(ctx context.Context, took time.Duration, pi transport.PathInfo, legs []LegResult, err error) {
	c := telemetry.Default()
	if !c.Enabled() {
		return
	}
	var parts []string
	for _, l := range legs {
		outcome := "lost"
		switch {
		case l.Won:
			outcome = "won"
		case l.Err != "":
			outcome = ErrClass(l.Err)
		}
		parts = append(parts, l.Endpoint.Transport+":"+outcome)
		c.Count("dial.leg", map[string]string{"transport": l.Endpoint.Transport, "outcome": outcome}, 1)
	}
	props := map[string]string{"legs": strings.Join(parts, ",")}
	code := "ok"
	if err == nil {
		props["winner"], props["via"] = pi.Transport, string(pi.Kind)
	} else {
		code = ErrClass(err.Error())
	}
	c.Dependency(ctx, telemetry.Dependency{Type: "dial", Target: "worker", Name: "dial", Duration: took, Code: code,
		Success: err == nil, Props: props})
}

// ErrClass reduces a connection error to a class for telemetry.
func ErrClass(s string) string {
	s = strings.ToLower(s)
	for _, c := range []struct{ match, class string }{
		{"cancel", "cancelled"},
		{"skipped", "skipped"},
		{"deadline", "timeout"}, {"timeout", "timeout"}, {"timed out", "timeout"},
		{"refused", "refused"},
		{"no route", "no_route"}, {"unreachable", "unreachable"},
		{"no such host", "dns"}, {"lookup", "dns"},
		{"trust", "trust"}, {"endorse", "trust"},
		{"certificate", "tls"}, {"tls", "tls"}, {"handshake", "tls"},
		{"reset", "reset"}, {"eof", "eof"},
		{"relay", "relay"},
	} {
		if strings.Contains(s, c.match) {
			return c.class
		}
	}
	return "other"
}

// TelemetryDir holds the client's telemetry queue, the hub's answer and
// crash reports (<config>/telemetry), shared by longarm runs, longarm
// daemon, longarm mcp and longarm-tray.
const TelemetryDir = "telemetry"

// TelemetryConfig is the telemetry configuration of a client process
// (component longarm or longarm-tray) for config dir dir. off says why
// telemetry is off ("" = on): no hub to send to, LONGARM_TELEMETRY=0, or
// `longarm telemetry off`.
func TelemetryConfig(dir, component string) (cfg telemetry.Config, off string) {
	tdir := filepath.Join(dir, TelemetryDir)
	cfg = telemetry.Config{Component: component, Version: version.Version, Dir: tdir}
	if b, err := pki.LoadBundle(filepath.Join(dir, TLSDir)); err == nil {
		cfg.Identity = telemetry.Identity{Node: b.Identity.Name, User: b.Identity.Owner(), Role: string(b.Identity.Role),
			CA: caDER(filepath.Join(dir, TLSDir))}
	}
	switch c, err := LoadConfig(dir); {
	case telemetry.EnvDisabled():
		off = telemetry.EnvOptOut + "=0"
	case err != nil || !c.Hub.Enabled():
		off = "no hub"
	}
	if s, err := telemetry.LoadSettings(tdir); err == nil && s.Disabled && off == "" {
		off = "turned off (longarm telemetry off)"
	}
	cfg.Disabled = off != ""
	return cfg, off
}

func caDER(tlsDir string) []byte {
	b, err := os.ReadFile(filepath.Join(tlsDir, pki.CACertFile))
	if err != nil {
		return nil
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil
	}
	return blk.Bytes
}
