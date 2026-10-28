package client

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/transport"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The transport policy (DESIGN §4.1 "prefer=tailscale|longnet, race") is the
// one user-facing setting for how longarm reaches workers with several legs:
//
//	race (default)          all legs, first mTLS handshake wins (sticky winner, see race.go)
//	tcp | tailscale | longnet prefer that transport; the others start after a grace
//
// It is persisted in <config>/settings.json by `longarm transport <policy>` (and
// the tray's Settings tab through longarm daemon). LONGARM_TRANSPORT and the global
// --transport flag override it for one process.

// Policies are the valid transport policies.
var Policies = []string{PolicyRace, "tcp", "tailscale", "longnet"}

// NormalizePolicy maps aliases ("" and "auto" = race, "tsnet" = tailscale)
// and rejects unknown policies.
func NormalizePolicy(p string) (string, error) {
	p = strings.ToLower(strings.TrimSpace(p))
	switch p {
	case "", "auto", PolicyRace:
		return PolicyRace, nil
	case "tsnet":
		return "tailscale", nil
	case "tcp", "tailscale", "longnet":
		return p, nil
	}
	return "", fmt.Errorf("transport policy %q: want %s", p, strings.Join(Policies, ", "))
}

// SettingsFile holds client settings written by `longarm transport` / the tray.
const SettingsFile = "settings.json"

// Settings are the client's UI-adjustable settings.
type Settings struct {
	Transport string `json:"transport"`
}

// LoadSettings reads <dir>/settings.json (defaults if missing or invalid).
// Environment overrides are applied by the Dialer, not here.
func LoadSettings(dir string) Settings {
	var s Settings
	if b, err := os.ReadFile(filepath.Join(dir, SettingsFile)); err == nil {
		json.Unmarshal(b, &s)
	}
	p, err := NormalizePolicy(s.Transport)
	if err != nil {
		p = PolicyRace
	}
	s.Transport = p
	return s
}

// SaveSettings writes <dir>/settings.json.
func SaveSettings(dir string, s Settings) error {
	p, err := NormalizePolicy(s.Transport)
	if err != nil {
		return err
	}
	s.Transport = p
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := fmt.Sprintf("%s.%d.tmp", filepath.Join(dir, SettingsFile), os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := platform.ReplaceFile(tmp, filepath.Join(dir, SettingsFile)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// NodeLegs turns a worker's advertised endpoint URLs into single-leg
// Workers, best first: tcp, longnet, tailscale; loopback/unspecified
// addresses (only usable on the worker's own machine) last. The pool
// monitor probes each of them on its own connection.
func NodeLegs(name string, endpoints []string) []Worker {
	type ranked struct {
		w    Worker
		rank int
	}
	var rs []ranked
	seen := map[string]bool{}
	for _, ep := range endpoints {
		e, err := ParseEndpoint(ep)
		if err != nil || seen[e.Key()] {
			continue
		}
		seen[e.Key()] = true
		r := map[string]int{"tcp": 0, "longnet": 1, "tailscale": 2}[e.Transport]
		host, _, err := net.SplitHostPort(e.Address)
		if err != nil {
			host = e.Address
		}
		if ip := net.ParseIP(strings.Trim(host, "[]")); host == "localhost" || ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
			r += 10
		}
		rs = append(rs, ranked{Worker{Name: name, CertName: name, Transport: e.Transport, Address: e.Address}, r})
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].rank < rs[j].rank })
	out := make([]Worker, len(rs))
	for i, r := range rs {
		out[i] = r.w
	}
	return out
}

// WorkerFromNode is WorkerFromEndpoints for a node the hub reported: it is
// dialed by the name in its certificate (NodeInfo.cert_name: after a
// rename, until the worker renewed) and must present its node ID.
func WorkerFromNode(n *pb.NodeInfo) (Worker, bool) {
	w, ok := WorkerFromEndpoints(n.Name, n.Endpoints)
	if !ok {
		return w, false
	}
	if n.CertName != "" {
		w.CertName = n.CertName
	}
	w.ID = n.Id
	return w, true
}

// NodeLegsOf is NodeLegs for a node the hub reported (WorkerFromNode).
func NodeLegsOf(n *pb.NodeInfo) []Worker {
	legs := NodeLegs(n.Name, n.Endpoints)
	for i := range legs {
		if n.CertName != "" {
			legs[i].CertName = n.CertName
		}
		legs[i].ID = n.Id
	}
	return legs
}

// WorkerFromEndpoints is a hub-known worker whose legs are all its
// advertised endpoints (raced by the dialer under the transport policy),
// the best ranked one first.
func WorkerFromEndpoints(name string, endpoints []string) (Worker, bool) {
	legs := NodeLegs(name, endpoints)
	if len(legs) == 0 {
		return Worker{}, false
	}
	w := legs[0]
	for _, l := range legs[1:] {
		w.Alt = append(w.Alt, Endpoint{Transport: l.Transport, Address: l.Address})
	}
	return w, true
}

// LegLabel names a leg's way through the network: "tcp", "tailscale
// direct|peer-relay|derp", "longnet direct|relay-udp|relay-tcp".
func LegLabel(p transport.PathInfo) string {
	switch p.Transport {
	case "longnet":
		if p.Kind == transport.PathRelay {
			k := "udp"
			if strings.HasPrefix(p.Detail, "tcp") {
				k = "tcp"
			}
			return "longnet relay-" + k
		}
		return "longnet " + string(p.Kind)
	case "tailscale":
		return "tailscale " + string(p.Kind)
	case "":
		return ""
	}
	return p.Transport
}

// NATOf extracts "easy/hard" from a longnet path detail ("..., nat easy/hard").
func NATOf(detail string) string {
	if i := strings.LastIndex(detail, "nat "); i >= 0 {
		return strings.TrimSpace(detail[i+4:])
	}
	return ""
}
