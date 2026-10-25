package client

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"

	"github.com/lubobill1990/longarm/internal/transport"
)

func TestNodeLegs(t *testing.T) {
	legs := NodeLegs("w1", []string{"tailscale://longarm-w1:7701", "tcp://127.0.0.1:7701", "longnet://relay.example:7443", "tcp://10.0.0.5:7701", "bogus", "tcp://10.0.0.5:7701"})
	var got []string
	for _, l := range legs {
		got = append(got, l.Transport+" "+l.Address)
	}
	want := []string{"tcp 10.0.0.5:7701", "longnet relay.example:7443", "tailscale longarm-w1:7701", "tcp 127.0.0.1:7701"}
	if len(got) != len(want) {
		t.Fatalf("legs %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("legs %v, want %v", got, want)
		}
	}
	w, ok := WorkerFromEndpoints("w1", []string{"tailscale://longarm-w1:7701", "tcp://10.0.0.5:7701"})
	if !ok || w.Transport != "tcp" || len(w.Alt) != 1 || w.Alt[0].Transport != "tailscale" || w.CertName != "w1" {
		t.Fatalf("WorkerFromEndpoints %+v", w)
	}
	if _, ok := WorkerFromEndpoints("w1", []string{"bogus"}); ok {
		t.Fatal("no usable endpoint")
	}
}

func TestLegLabelAndNAT(t *testing.T) {
	cases := map[string]transport.PathInfo{
		"longnet relay-udp":    {Transport: "longnet", Kind: transport.PathRelay, Detail: "udp 1.2.3.4:7443, nat hard/hard"},
		"longnet relay-tcp":    {Transport: "longnet", Kind: transport.PathRelay, Detail: "tcp 1.2.3.4:7443"},
		"longnet direct":       {Transport: "longnet", Kind: transport.PathDirect},
		"tailscale derp":       {Transport: "tailscale", Kind: transport.PathDERP},
		"tailscale peer-relay": {Transport: "tailscale", Kind: transport.PathPeerRelay},
		"tcp":                  {Transport: "tcp", Kind: transport.PathDirect},
	}
	for want, pi := range cases {
		if got := LegLabel(pi); got != want {
			t.Errorf("LegLabel(%v) = %q, want %q", pi, got, want)
		}
	}
	if NATOf("udp 1.2.3.4:7443, nat easy/hard") != "easy/hard" || NATOf("10.0.0.1:5") != "" {
		t.Fatal("NATOf")
	}
}

func TestSettingsPolicy(t *testing.T) {
	dir := t.TempDir()
	if s := LoadSettings(dir); s.Transport != PolicyRace {
		t.Fatalf("default %v", s)
	}
	if err := SaveSettings(dir, Settings{Transport: "nope"}); err == nil {
		t.Fatal("bad policy saved")
	}
	if err := SaveSettings(dir, Settings{Transport: "tsnet"}); err != nil {
		t.Fatal(err)
	}
	if s := LoadSettings(dir); s.Transport != "tailscale" {
		t.Fatalf("saved %v", s)
	}
	if p, err := NormalizePolicy("auto"); err != nil || p != PolicyRace {
		t.Fatalf("auto = %q %v", p, err)
	}
	// The dialer: settings.json, overridden by LONGARM_TRANSPORT.
	writeClientBundle(t, dir)
	d, err := NewDialer(&Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if d.Policy() != "tailscale" {
		t.Fatalf("dialer policy from settings.json: %q", d.Policy())
	}
	t.Setenv("LONGARM_TRANSPORT", "longnet")
	if d, err = NewDialer(&Config{Dir: dir}); err != nil || d.Policy() != "longnet" {
		t.Fatalf("LONGARM_TRANSPORT: %v %v", d, err)
	}
	t.Setenv("LONGARM_TRANSPORT", "bogus")
	if _, err = NewDialer(&Config{Dir: dir}); err == nil {
		t.Fatal("bad LONGARM_TRANSPORT accepted")
	}
}

func writeClientBundle(t *testing.T, dir string) {
	ca, err := pki.InitCA(filepath.Join(t.TempDir(), "ca"), "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := ca.Issue(pki.IssueOptions{Role: pki.RoleClient, Name: "alice", Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteBundle(filepath.Join(dir, TLSDir), ca.CertPEM, cert, key); err != nil {
		t.Fatal(err)
	}
}
