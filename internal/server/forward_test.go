package server

import (
	"context"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestForwardTargetsDefaultLoopback: in shared isolation forwards reach the
// worker's own loopback ports only, unless [forward] targets = "any" -
// for every caller, admins included; the built-in sshd's check agrees.
func TestForwardTargetsDefaultLoopback(t *testing.T) {
	e := newAuthzEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("hello"))
			c.Close()
		}
	}()
	open := func(conn pb.ForwardClient, target string) (string, error) {
		st, err := conn.Open(cctx(t))
		if err != nil {
			return "", err
		}
		if err := st.Send(&pb.ForwardFrame{Target: target}); err != nil {
			return "", err
		}
		st.CloseSend()
		var got []byte
		for {
			m, err := st.Recv()
			if err == io.EOF {
				return string(got), nil
			}
			if err != nil {
				return string(got), err
			}
			got = append(got, m.Data...)
		}
	}
	for _, c := range []struct {
		name string
		fc   pb.ForwardClient
	}{{"alice", pb.NewForwardClient(e.alice)}, {"root", pb.NewForwardClient(e.root)}} {
		if got, err := open(c.fc, ln.Addr().String()); err != nil || got != "hello" {
			t.Fatalf("%s: loopback forward: %q %v", c.name, got, err)
		}
		_, err := open(c.fc, "192.0.2.1:80")
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("%s: forward to another host with the default targets: %v", c.name, err)
		}
	}
	if err := e.srv.ForwardTargetCheck("192.0.2.1", 22); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("sshd target check: %v", err)
	}
	if err := e.srv.ForwardTargetCheck("::1", 22); err != nil {
		t.Fatalf("sshd target check loopback: %v", err)
	}

	// "any" is opt-in.
	e.srv.ForwardTargets = ForwardAny
	ctx := context.WithValue(context.Background(), idKey{}, pki.Identity{Role: pki.RoleClient, Name: "bob-laptop", User: "bob"})
	if err := e.srv.forwardAllowed(ctx, "192.0.2.1:80"); err != nil {
		t.Fatalf("targets = any: %v", err)
	}
	if err := e.srv.ForwardTargetCheck("192.0.2.1", 22); err != nil {
		t.Fatalf("sshd with targets = any: %v", err)
	}
}

func TestParseForwardTargets(t *testing.T) {
	for in, want := range map[string]string{"": ForwardLoopback, "loopback": ForwardLoopback, "any": ForwardAny} {
		if got, err := ParseForwardTargets(in); err != nil || got != want {
			t.Fatalf("%q: %q %v", in, got, err)
		}
	}
	if _, err := ParseForwardTargets("lan"); err == nil {
		t.Fatal("bad value accepted")
	}
}
