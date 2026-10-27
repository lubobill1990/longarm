package tgrpc

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/lubobill1990/longarm/internal/telemetry"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type sink struct {
	mu    sync.Mutex
	items []telemetry.Item
}

func (s *sink) add(it []telemetry.Item) {
	s.mu.Lock()
	s.items = append(s.items, it...)
	s.mu.Unlock()
}

func (s *sink) get() []telemetry.Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]telemetry.Item(nil), s.items...)
}

type nodeSrv struct {
	pb.UnimplementedNodeServer
	seen chan telemetry.Span
}

func (n *nodeSrv) Info(ctx context.Context, _ *pb.InfoRequest) (*pb.InfoResponse, error) {
	sp, _ := telemetry.SpanFrom(ctx)
	n.seen <- sp
	telemetry.Remember(ctx, "job/x")
	return &pb.InfoResponse{Name: "w"}, nil
}

// TestPropagation: a client call carries its operation (traceparent) to the
// server; the client's dependency and the server's request link up.
func TestPropagation(t *testing.T) {
	var cs, ss sink
	clientTel := telemetry.New(telemetry.Config{Component: "longarm", Detailed: true, Sink: cs.add})
	serverTel := telemetry.New(telemetry.Config{Component: "longarmd", Detailed: true, Sink: ss.add})
	telemetry.SetDefault(clientTel)
	defer telemetry.SetDefault(nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ns := &nodeSrv{seen: make(chan telemetry.Span, 1)}
	g := grpc.NewServer(grpc.ChainUnaryInterceptor(UnaryServer(func() *telemetry.Client { return serverTel })),
		grpc.ChainStreamInterceptor(StreamServer(func() *telemetry.Client { return serverTel })))
	pb.RegisterNodeServer(g, ns)
	go g.Serve(ln)
	defer g.Stop()
	cc, err := grpc.NewClient(ln.Addr().String(), append(DialOptions("worker"), grpc.WithTransportCredentials(insecure.NewCredentials()))...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	root := telemetry.NewSpan()
	ctx := telemetry.WithSpan(context.Background(), root)
	if _, err := pb.NewNodeClient(cc).Info(ctx, &pb.InfoRequest{}); err != nil {
		t.Fatal(err)
	}
	got := <-ns.seen
	if got.TraceID != root.TraceID || got.ParentID == "" || got.ParentID == root.SpanID {
		t.Fatalf("server span %+v, root %+v", got, root)
	}
	deps, reqs := cs.get(), ss.get()
	if len(deps) != 1 || len(reqs) != 1 {
		t.Fatalf("deps %+v reqs %+v", deps, reqs)
	}
	dep, req := deps[0], reqs[0]
	if dep.Kind != telemetry.KindDependency || dep.OperationID != root.TraceID || dep.ParentID != root.SpanID || dep.Target != "worker" ||
		dep.Name != "Node/Info" || !dep.Success {
		t.Fatalf("dependency %+v", dep)
	}
	if req.Kind != telemetry.KindRequest || req.OperationID != root.TraceID || req.ParentID != dep.ID || req.ID != got.SpanID ||
		req.Name != "Node/Info" || req.ResultCode != "OK" {
		t.Fatalf("request %+v (dependency id %s)", req, dep.ID)
	}
	// later items about the job link to the same operation
	if telemetry.TraceID(telemetry.Recall(context.Background(), "job/x")) != root.TraceID {
		t.Fatal("job operation not remembered")
	}

	// without an operation the server starts one
	telemetry.SetDefault(nil)
	if _, err := pb.NewNodeClient(cc).Info(context.Background(), &pb.InfoRequest{}); err != nil {
		t.Fatal(err)
	}
	if sp := <-ns.seen; !sp.Valid() || sp.TraceID == root.TraceID || sp.ParentID != "" {
		t.Fatalf("new operation %+v", sp)
	}
}

type telSrv struct {
	pb.UnimplementedTelemetryServer
	collecting bool
	mu         sync.Mutex
	got        []*pb.TelemetryItem
	calls      int
}

func (s *telSrv) Push(_ context.Context, b *pb.TelemetryBatch) (*pb.TelemetryAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if !s.collecting {
		return &pb.TelemetryAck{Collecting: false, RetryAfterSeconds: 1}, nil
	}
	s.got = append(s.got, b.Items...)
	return &pb.TelemetryAck{Collecting: true, MaxBatch: 2}, nil
}

func TestPushQueue(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ts := &telSrv{collecting: true}
	g := grpc.NewServer()
	pb.RegisterTelemetryServer(g, ts)
	go g.Serve(ln)
	defer g.Stop()
	cc, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	cl := pb.NewTelemetryClient(cc)

	dir := t.TempDir()
	tel := telemetry.New(telemetry.Config{Component: "longarm", Dir: dir})
	for range 5 {
		tel.Event(context.Background(), "e", map[string]string{"k": "v"}, nil)
	}
	tel.Flush()
	n, err := PushQueue(context.Background(), tel, cl)
	if err != nil || n != 5 || len(ts.got) != 5 || ts.got[0].Name != "e" || ts.got[0].Context.Component != "longarm" {
		t.Fatalf("sent %d %v, got %d", n, err, len(ts.got))
	}
	if q, _ := tel.Queue().Stat(); q != 0 {
		t.Fatalf("%d left in the queue", q)
	}

	// the hub doesn't collect: the queue goes, recording stops
	ts.collecting = false
	tel.Event(context.Background(), "e", nil, nil)
	tel.Flush()
	if _, err := PushQueue(context.Background(), tel, cl); err != nil {
		t.Fatal(err)
	}
	if tel.Enabled() {
		t.Fatal("still enabled")
	}
	if q, _ := tel.Queue().Stat(); q != 0 {
		t.Fatal("queue kept")
	}
	// it asks again once retry_after passed
	time.Sleep(1100 * time.Millisecond)
	ts.collecting = true
	p := &Pusher{Tel: tel, Conn: func(context.Context) grpc.ClientConnInterface { return cc }}
	if _, err := p.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !tel.Enabled() {
		t.Fatal("not enabled after the hub collects again")
	}
	// no hub: ErrNoHub, the queue stays
	tel.Event(context.Background(), "later", nil, nil)
	p = &Pusher{Tel: tel, Conn: func(context.Context) grpc.ClientConnInterface { return nil }}
	if _, err := p.Push(context.Background()); err != ErrNoHub {
		t.Fatal(err)
	}
	if q, _ := tel.Queue().Stat(); q != 1 {
		t.Fatalf("queue %d", q)
	}
}

func TestRoundTripPB(t *testing.T) {
	it := telemetry.Item{Kind: telemetry.KindException, Time: time.Unix(100, 0).UTC(), Name: "panic", Message: "m", Severity: "error",
		Props: map[string]string{"a": "b"}, Stack: []telemetry.Frame{{Function: "f", File: "x.go", Line: 3}},
		Context: telemetry.Context{Component: "longarmd", Node: "<node:1>"}, OperationID: "op", ParentID: "p", DurationMS: 2, Count: 1}
	back := FromPB(ToPB(it))
	if back.Name != it.Name || back.Stack[0] != it.Stack[0] || back.Context != it.Context || !back.Time.Equal(it.Time) || back.OperationID != "op" {
		t.Fatalf("%+v", back)
	}
}

func TestMethodName(t *testing.T) {
	if got := MethodName(pb.Jobs_Submit_FullMethodName); got != "Jobs/Submit" {
		t.Fatal(got)
	}
}
