package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// fakeDaemons is the daemons of one config dir: one runs at a time, the
// replacer stops it and starts another.
type fakeDaemons struct {
	mu      sync.Mutex
	running *fakeDaemon
	alive   map[int]bool
	nextPID int
	// starts: the versions started in turn (the first: by this longarm;
	// an app's bundled copy may win the race)
	starts []string
	stops  int
	moved  []*pb.StartForwardRequest
}

type fakeDaemon struct {
	pb.LocalClientClient
	ds       *fakeDaemons
	pid      int
	version  string
	forwards []*pb.ForwardInfo
}

func (d *fakeDaemon) Status(context.Context, *pb.LocalStatusRequest, ...grpc.CallOption) (*pb.ClientStatus, error) {
	return &pb.ClientStatus{Pid: int32(d.pid), Version: d.version, Forwards: int32(len(d.forwards))}, nil
}

func (d *fakeDaemon) ListForwards(context.Context, *pb.ListForwardsRequest, ...grpc.CallOption) (*pb.ListForwardsResponse, error) {
	return &pb.ListForwardsResponse{Forwards: d.forwards}, nil
}

func (d *fakeDaemon) Shutdown(context.Context, *pb.ShutdownRequest, ...grpc.CallOption) (*pb.ShutdownResponse, error) {
	d.ds.mu.Lock()
	defer d.ds.mu.Unlock()
	d.ds.stops++
	delete(d.ds.alive, d.pid)
	if d.ds.running == d {
		d.ds.running = nil
	}
	return &pb.ShutdownResponse{}, nil
}

func (d *fakeDaemon) StartForward(_ context.Context, r *pb.StartForwardRequest, _ ...grpc.CallOption) (*pb.ForwardInfo, error) {
	d.ds.mu.Lock()
	defer d.ds.mu.Unlock()
	d.ds.moved = append(d.ds.moved, r)
	return &pb.ForwardInfo{Worker: r.Worker, Target: r.Target}, nil
}

func (ds *fakeDaemons) run(version string) *fakeDaemon {
	ds.nextPID++
	d := &fakeDaemon{ds: ds, pid: ds.nextPID, version: version}
	ds.running, ds.alive[d.pid] = d, true
	return d
}

func (ds *fakeDaemons) replacer(mine string) *daemonReplacer {
	return &daemonReplacer{mine: mine,
		dial: func() (pb.LocalClientClient, func(), *pb.ClientStatus, error) {
			ds.mu.Lock()
			d := ds.running
			ds.mu.Unlock()
			if d == nil {
				return nil, nil, nil, fmt.Errorf("not running")
			}
			st, _ := d.Status(context.Background(), nil)
			return d, func() {}, st, nil
		},
		start: func() error {
			ds.mu.Lock()
			defer ds.mu.Unlock()
			v := mine
			if len(ds.starts) > 0 {
				v, ds.starts = ds.starts[0], ds.starts[1:]
			}
			ds.run(v)
			return nil
		},
		alive:    func(pid int) bool { ds.mu.Lock(); defer ds.mu.Unlock(); return ds.alive[pid] },
		logf:     func(string, ...any) {},
		stopWait: time.Second, startWait: time.Second,
	}
}

func newFakeDaemons(running string) (*fakeDaemons, *fakeDaemon) {
	ds := &fakeDaemons{alive: map[int]bool{}}
	return ds, ds.run(running)
}

func upgradeWith(t *testing.T, ds *fakeDaemons, d *fakeDaemon, mine string) *pb.ClientStatus {
	t.Helper()
	st, _ := d.Status(context.Background(), nil)
	c, closeFn, err := ds.replacer(mine).upgrade(context.Background(), d, func() {}, st)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	now, _ := c.Status(context.Background(), &pb.LocalStatusRequest{})
	return now
}

// TestDaemonReplacedByNewerLongarm: after an upgrade, a newer longarm
// (the tray's daemon start, the CLI) replaces the older daemon that kept
// running and moves its forwards over.
func TestDaemonReplacedByNewerLongarm(t *testing.T) {
	ds, d := newFakeDaemons("0.1.15")
	d.forwards = []*pb.ForwardInfo{{Worker: "w1", Target: "3000", LocalAddr: "127.0.0.1:3000"}, {Worker: "w2", Target: "db:5432", LocalAddr: "[::1]:15432"}}
	st := upgradeWith(t, ds, d, "0.1.17")
	if st.Version != "0.1.17" || ds.stops != 1 || ds.alive[d.pid] {
		t.Fatalf("running %v, stops %d", st, ds.stops)
	}
	if len(ds.moved) != 2 || ds.moved[0].LocalPort != 3000 || ds.moved[0].Bind != "127.0.0.1" || ds.moved[1].Target != "db:5432" ||
		ds.moved[1].LocalPort != 15432 || ds.moved[1].Bind != "::1" {
		t.Fatalf("forwards moved: %v", ds.moved)
	}
}

// TestDaemonNeverDowngraded: an older longarm (an app's bundled 0.1.2, an
// old CLI) uses a newer running daemon as it is; a development build
// neither replaces nor is replaced.
func TestDaemonNeverDowngraded(t *testing.T) {
	for _, c := range []struct{ running, mine string }{
		{"0.1.17", "0.1.2"}, {"0.1.17", "0.1.17"}, {"0.1.17", "dev"}, {"dev", "0.1.17"},
	} {
		ds, d := newFakeDaemons(c.running)
		if st := upgradeWith(t, ds, d, c.mine); st.Version != c.running || ds.stops != 0 {
			t.Errorf("%s running, %s asking: now %s, %d stops", c.running, c.mine, st.Version, ds.stops)
		}
	}
}

// TestDaemonReplaceRace: an app's bundled older longarm starts its daemon
// first after ours stopped the old one: replaced again; when it keeps
// winning, the replacer gives up after replaceAttempts and uses it.
func TestDaemonReplaceRace(t *testing.T) {
	ds, d := newFakeDaemons("0.1.15")
	ds.starts = []string{"0.1.2", "0.1.17"}
	if st := upgradeWith(t, ds, d, "0.1.17"); st.Version != "0.1.17" || ds.stops != 2 {
		t.Fatalf("now %s after %d stops", st.Version, ds.stops)
	}
	ds, d = newFakeDaemons("0.1.15")
	ds.starts = []string{"0.1.2", "0.1.2", "0.1.2", "0.1.2"}
	if st := upgradeWith(t, ds, d, "0.1.17"); st.Version != "0.1.2" || ds.stops != replaceAttempts {
		t.Fatalf("now %s after %d stops", st.Version, ds.stops)
	}
}

// TestDaemonReplaceOldWontStop: an old daemon that doesn't exit is used as
// it is (no second daemon started next to it).
func TestDaemonReplaceOldWontStop(t *testing.T) {
	ds, d := newFakeDaemons("0.1.15")
	r := ds.replacer("0.1.17")
	r.alive = func(int) bool { return true }
	r.stopWait = 200 * time.Millisecond
	ds.mu.Lock()
	ds.running = d // Shutdown "succeeds" but it keeps running
	ds.mu.Unlock()
	st, _ := d.Status(context.Background(), nil)
	stuck := &stubbornDaemon{fakeDaemon: d}
	c, _, err := r.upgrade(context.Background(), stuck, func() {}, st)
	if err != nil {
		t.Fatal(err)
	}
	if now, _ := c.Status(context.Background(), nil); now.Version != "0.1.15" || len(ds.starts) != 0 || ds.nextPID != 1 {
		t.Fatalf("now %v, %d daemons started", now, ds.nextPID)
	}
}

type stubbornDaemon struct{ *fakeDaemon }

func (*stubbornDaemon) Shutdown(context.Context, *pb.ShutdownRequest, ...grpc.CallOption) (*pb.ShutdownResponse, error) {
	return &pb.ShutdownResponse{}, nil
}
