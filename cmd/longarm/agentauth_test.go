package main

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/client"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// fakeAttach records every open; the first stream breaks after "started"
// (a lost connection), the second ends with the program's exit.
type fakeAttach struct {
	pb.UnimplementedSessionsServer
	mu    sync.Mutex
	opens []*pb.AttachOpen
}

func (f *fakeAttach) Attach(st pb.Sessions_AttachServer) error {
	m, err := st.Recv()
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.opens = append(f.opens, proto.Clone(m.GetOpen()).(*pb.AttachOpen))
	n := len(f.opens)
	f.mu.Unlock()
	if err := st.Send(&pb.AttachOutput{Msg: &pb.AttachOutput_Started{Started: &pb.AttachStarted{
		Session: &pb.Session{Name: "s"}, Created: n == 1}}}); err != nil {
		return err
	}
	if n == 1 {
		return status.Error(codes.Unavailable, "connection lost")
	}
	return st.Send(&pb.AttachOutput{Msg: &pb.AttachOutput_ExitCode{ExitCode: 0}})
}

// A re-attach after a disconnect never resends the credentials (nor
// re-creates the session): only the first open carries them.
func TestAttachReconnectDoesNotResendCredentials(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	fa := &fakeAttach{}
	pb.RegisterSessionsServer(srv, fa)
	go srv.Serve(ln)
	defer srv.Stop()
	cc, err := grpc.NewClient("passthrough:///"+ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	conn := &client.Conn{ClientConn: cc, Worker: client.Worker{Name: "w1"}}
	open := &pb.AttachOpen{Name: "s", Argv: []string{"claude"}, Env: map[string]string{"ANTHROPIC_MODEL": "m"},
		SecretEnv: map[string]string{"ANTHROPIC_AUTH_TOKEN": "value"}, AgentAuth: &pb.AgentAuth{Agents: []string{"claude"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := runAttach(ctx, conn, open); err != nil {
		t.Fatal(err)
	}
	fa.mu.Lock()
	defer fa.mu.Unlock()
	if len(fa.opens) != 2 {
		t.Fatalf("%d opens", len(fa.opens))
	}
	if first := fa.opens[0]; first.SecretEnv["ANTHROPIC_AUTH_TOKEN"] != "value" || first.AgentAuth == nil || first.NoCreate {
		t.Fatalf("first open: %v", first)
	}
	if re := fa.opens[1]; len(re.SecretEnv) != 0 || re.AgentAuth != nil || !re.NoCreate {
		t.Fatalf("re-attach open carries credentials or may create: %v", re)
	}
}
