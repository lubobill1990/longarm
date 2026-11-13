package hub

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"longarm/internal/relay"
)

// The stats handler counts payload bytes per method, each way, and
// running streams.
func TestTrafficStatsCountsMethods(t *testing.T) {
	ts := newTrafficStats()
	g := grpc.NewServer(grpc.StatsHandler(ts))
	healthpb.RegisterHealthServer(g, health.NewServer())
	ts.setKnown(g)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go g.Serve(ln)
	defer g.Stop()
	cc, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	c := healthpb.NewHealthClient(cc)
	for range 3 {
		if _, err := c.Check(context.Background(), &healthpb.HealthCheckRequest{Service: "some-service-name"}); err == nil {
			t.Fatal("unknown service: want an error")
		}
	}
	if _, err := c.Check(context.Background(), &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	// paths the server doesn't serve (anyone may send any): one row
	for _, p := range []string{"/x.Y/A", "/x.Y/B", "/nothing"} {
		if err := cc.Invoke(context.Background(), p, &healthpb.HealthCheckRequest{}, &healthpb.HealthCheckResponse{}); err == nil {
			t.Fatalf("%s: want an error", p)
		}
	}
	rows := ts.snapshot()
	if len(rows) != 2 || rows[1].Method != unknownMethod { // (no Begin for an unknown method: no calls counted)
		t.Fatalf("rows %+v", rows)
	}
	r := rows[0]
	if r.Method != healthpb.Health_Check_FullMethodName || r.Node != "" || r.Calls != 4 || r.MsgsIn != 4 || r.MsgsOut != 1 || r.Active != 0 {
		t.Errorf("row %+v", r)
	}
	if r.BytesIn < uint64(3*len("some-service-name")) || r.BytesOut == 0 {
		t.Errorf("bytes %+v", r)
	}
}

func TestTrafficDeltaAndFold(t *testing.T) {
	prev := []TrafficRow{{Method: "/a", Node: "worker/w1", BytesOut: 100, BytesIn: 10, Calls: 1}}
	cur := []TrafficRow{
		{Method: "/a", Node: "worker/w1", BytesOut: 150, BytesIn: 10, Calls: 2},
		{Method: "/a", Node: "client/c", BytesOut: 500},
		{Method: "/b", Node: "worker/w1", BytesOut: 7},
		{Method: "/idle", Node: "worker/w1"},
	}
	d := delta(cur, prev)
	if len(d) != 3 || d[0].Node != "client/c" || d[0].BytesOut != 500 || d[1].BytesOut != 50 || d[1].Calls != 1 || d[2].Method != "/b" {
		t.Errorf("delta %+v", d)
	}
	byNode := fold(cur, func(r TrafficRow) TrafficRow { return TrafficRow{Node: r.Node} })
	if len(byNode) != 2 || byNode[0].Node != "client/c" || byNode[1].BytesOut != 157 || byNode[1].Calls != 2 {
		t.Errorf("by node %+v", byNode)
	}
	// a counter that dipped (a relay session counted twice for a moment)
	// is 0 for the period, never its whole total; each counter on its own
	if d := delta([]TrafficRow{{Node: "n", BytesOut: 5}}, []TrafficRow{{Node: "n", BytesOut: 9}}); len(d) != 0 {
		t.Errorf("dip delta %+v", d)
	}
	if d := delta([]TrafficRow{{Node: "n", BytesOut: 20, BytesIn: 1}}, []TrafficRow{{Node: "n", BytesOut: 9, BytesIn: 3}}); len(d) != 1 || d[0].BytesOut != 11 || d[0].BytesIn != 0 {
		t.Errorf("one counter dipped %+v", d)
	}
	// rows of different nodes that had the same name stay apart
	if d := delta([]TrafficRow{{Node: "client/laptop", NodeID: "B", BytesOut: 3}}, []TrafficRow{{Node: "client/laptop", NodeID: "A", BytesOut: 9}}); len(d) != 1 || d[0].BytesOut != 3 {
		t.Errorf("reused name %+v", d)
	}
}

func TestRelayEndedSumsPerNode(t *testing.T) {
	var r relayEnded
	r.add(relay.SessionStats{Node: "worker/w1", BytesIn: 1, BytesOut: 2})
	r.add(relay.SessionStats{Node: "worker/w1", BytesIn: 10, BytesOut: 20})
	r.add(relay.SessionStats{Node: "client/c", BytesOut: 5})
	got := fold(r.snapshot(), func(x TrafficRow) TrafficRow { return TrafficRow{Node: x.Node} })
	if len(got) != 2 || got[0].Node != "worker/w1" || got[0].BytesIn != 11 || got[0].BytesOut != 22 || got[1].BytesOut != 5 {
		t.Errorf("ended %+v", got)
	}
}
