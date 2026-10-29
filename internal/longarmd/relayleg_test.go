package longarmd

import (
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
