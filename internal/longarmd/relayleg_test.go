package longarmd

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func TestRelayListenURL(t *testing.T) {
	for _, c := range []struct{ hub, relay, want string }{
		{"hub.example:7443", ":7445", "longnet://hub.example:7445?tcp=hub.example:7443&name=w1"},
		{"hub.example:7443", ":7443", "longnet://hub.example:7443?tcp=hub.example:7443&name=w1"},
		{"hub.example:7443", "relay.example:7445", "longnet://relay.example:7445?name=w1"},
		{"hub.example:7443", "", ""},
		{"hub.example:7443", ":0", ""},
		{"", ":7445", ""},
	} {
		if got := relayListenURL(c.hub, c.relay, "w1"); got != c.want {
			t.Errorf("%s %q: %q, want %q", c.hub, c.relay, got, c.want)
		}
	}
}

func TestAutoRelayLeg(t *testing.T) {
	for _, c := range []struct {
		cfg  Config
		want bool
	}{
		{Config{Listen: []string{"tcp://127.0.0.1:7701"}}, true}, // sign-in installs
		{Config{Listen: []string{"tcp://127.0.0.1:7702"}}, true}, // the admin worker
		{Config{Listen: []string{"tcp://localhost:7701", "tcp://[::1]:7701"}}, true},
		{Config{Listen: []string{"tcp://127.0.0.1:7701"}, Hub: HubConfig{Relay: "off"}}, false},
		{Config{Listen: []string{"tcp://127.0.0.1:7701"}, Hub: HubConfig{Advertise: []string{"tcp://10.0.0.1:7701"}}}, false},
		{Config{Listen: []string{"tcp://127.0.0.1:7701", "longnet://hub:7445?name=w"}}, false},
		{Config{Listen: []string{"tcp://0.0.0.0:7701"}}, false},
		{Config{Listen: []string{"tailscale://w1"}}, false},
	} {
		if got := autoRelayLeg(c.cfg); got != c.want {
			t.Errorf("%+v: %v, want %v", c.cfg.Listen, got, c.want)
		}
	}
}

func TestRelaySetURL(t *testing.T) {
	for _, c := range []struct {
		r    *pb.RelayEndpoint
		want string
	}{
		{&pb.RelayEndpoint{Udp: ":7445", Embedded: true}, "longnet://hub.example:7445?tcp=hub.example:7443&name=w1"},
		{&pb.RelayEndpoint{Udp: "relay.example:7445", Tcp: "relay.example:7443"}, "longnet://relay.example:7445?tcp=relay.example:7443&name=w1"},
		{&pb.RelayEndpoint{Udp: "relay.example:7445", Tcp: "relay.example:7445"}, "longnet://relay.example:7445?name=w1"},
		{&pb.RelayEndpoint{Udp: "relay.example:7445"}, "longnet://relay.example:7445?name=w1"},
		{&pb.RelayEndpoint{Udp: "relay.example:7445", Tcp: ":7443"}, ""},
		{&pb.RelayEndpoint{Udp: "relay.example"}, ""},
		{&pb.RelayEndpoint{Udp: ""}, ""},
	} {
		if got := relaySetURL("hub.example:7443", c.r, "w1"); got != c.want {
			t.Errorf("%v: %q, want %q", c.r, got, c.want)
		}
	}
}

// The admin worker's hub.toml keeps the hub as typed for the installer
// (its sign-in installs hf.Address, not the resolved host:port): the
// relay legs must come up for it as for the hub session, which normalizes
// the address (hubclient.NormalizeAddr).
func TestRelayURLHubAsTyped(t *testing.T) {
	want := "longnet://hub.example:7443?tcp=hub.example:7443&name=w1-admin"
	for _, hub := range []string{"hub.example", "https://hub.example", "https://hub.example/", " hub.example ", "hub.example:7443"} {
		if got := relaySetURL(hub, &pb.RelayEndpoint{Udp: ":7443", Embedded: true}, "w1-admin"); got != want {
			t.Errorf("hub %q: %q, want %q", hub, got, want)
		}
	}
	if got := relayListenURL("https://hub.example:8443", ":7445", "w1"); got != "longnet://hub.example:7445?tcp=hub.example:8443&name=w1" {
		t.Errorf("https with a port: %q", got)
	}
	// external relays don't depend on the hub's address
	if got := relaySetURL("hub.example", &pb.RelayEndpoint{Udp: "relay.example:7445"}, "w1"); got != "longnet://relay.example:7445?name=w1" {
		t.Errorf("external relay: %q", got)
	}
}

// A relay of the set without a usable listen URL is logged, not skipped
// silently.
func TestRelayLegWarnsUnusableRelay(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no legs start: only the URL check runs
	r := &relayLeg{ctx: ctx, log: slog.New(slog.NewTextHandler(&buf, nil)), node: "w1"}
	r.start("hub.example", []*pb.RelayEndpoint{{Udp: "relay.example"}})
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "relay.example") {
		t.Fatalf("log: %s", out)
	}
}

func TestNoteEndpoints(t *testing.T) {
	var buf bytes.Buffer
	a := &hubAgent{log: slog.New(slog.NewTextHandler(&buf, nil))}
	step := func(eps []string, want string) {
		t.Helper()
		buf.Reset()
		a.noteEndpoints(eps)
		if got := buf.String(); want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Fatalf("%v: logged %q, want %q", eps, got, want)
		}
	}
	lo := "tcp://127.0.0.1:7702"
	relayed := "longnet://hub.example:7443?name=w1-admin"
	step([]string{lo}, "level=INFO msg=\"hub: endpoints reported to the hub\"")
	step([]string{lo}, "")
	step([]string{lo, relayed}, "level=INFO msg=\"hub: the endpoints reported to the hub changed\"")
	step([]string{lo}, "level=WARN msg=\"hub: the endpoints reported to the hub changed\"")
}
