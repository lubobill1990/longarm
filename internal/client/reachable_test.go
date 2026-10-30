package client

import (
	"reflect"
	"testing"
)

func TestReachableEndpoints(t *testing.T) {
	old := thisHost
	thisHost = func() string { return "Laptop" }
	defer func() { thisHost = old }()
	eps := []string{"tcp://127.0.0.1:7701", "longnet://hub.example:7445?name=w", "tcp://[::1]:7701", "tcp://0.0.0.0:7701", "tcp://10.0.0.5:7701"}
	remote := []string{"longnet://hub.example:7445?name=w", "tcp://10.0.0.5:7701"}
	for _, c := range []struct {
		host string
		want []string
	}{
		{"cpc-x", remote},            // another machine: no loopback legs
		{"laptop", eps},              // this machine (case-insensitive)
		{"LAPTOP.corp.example", eps}, // with a domain
		{"", eps},                    // unknown: keep all
	} {
		if got := ReachableEndpoints(eps, c.host); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: %v, want %v", c.host, got, c.want)
		}
	}
}
