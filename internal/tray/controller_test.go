package tray

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type statusOnly struct {
	pb.LocalClientClient
	st *pb.ClientStatus
}

func (s statusOnly) Status(context.Context, *pb.LocalStatusRequest, ...grpc.CallOption) (*pb.ClientStatus, error) {
	return s.st, nil
}

// TestControllerReplacesOlderDaemon: the tray, restarted by an upgrade,
// finds the daemon the upgrade left running (older than itself) and runs
// longarm daemon start, which replaces it - once per daemon process; a
// daemon as new or newer (or a development build) is left alone.
func TestControllerReplacesOlderDaemon(t *testing.T) {
	starts := 0
	c := NewController(Options{Version: "0.1.17", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, nil, func() error { starts++; return nil })
	old := statusOnly{st: &pb.ClientStatus{Version: "0.1.15", Pid: 41}}
	if !c.replaceOlder(context.Background(), old) || starts != 1 {
		t.Fatalf("older daemon: %d starts", starts)
	}
	if c.replaceOlder(context.Background(), old) || starts != 1 {
		t.Fatalf("the same daemon again: %d starts", starts)
	}
	for _, v := range []string{"0.1.17", "0.1.18", "dev"} {
		if c.replaceOlder(context.Background(), statusOnly{st: &pb.ClientStatus{Version: v, Pid: 42}}) {
			t.Errorf("daemon %s replaced", v)
		}
	}
	if c.replaceOlder(context.Background(), statusOnly{st: &pb.ClientStatus{Version: "0.1.2", Pid: 43}}) != true || starts != 2 {
		t.Fatalf("another older daemon (an app's bundled copy): %d starts", starts)
	}
	dev := NewController(Options{Version: "dev", Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, nil, func() error { starts++; return nil })
	if dev.replaceOlder(context.Background(), old) {
		t.Fatal("a development tray replaced a release daemon")
	}
}
