package server

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"longarm/internal/pki"
	pb "longarm/proto/longarm/v1"
)

// sourceAll reads a Source to its end.
func sourceAll(t *testing.T, conn *grpc.ClientConn, g *pb.CopyGrant, maxBytes int64) (int64, error) {
	t.Helper()
	st, err := pb.NewCopyClient(conn).Source(cctx(t), &pb.SpeedSourceRequest{Grant: g, Duration: durationpb.New(time.Second), MaxBytes: maxBytes})
	if err != nil {
		return 0, err
	}
	var n int64
	for {
		m, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		n += int64(len(m.Data))
	}
}

// sinkSome sends n bytes to a Sink.
func sinkSome(t *testing.T, conn *grpc.ClientConn, g *pb.CopyGrant, n int) (*pb.SpeedSinkResponse, error) {
	t.Helper()
	st, err := pb.NewCopyClient(conn).Sink(cctx(t))
	if err != nil {
		return nil, err
	}
	st.Send(&pb.SpeedSinkChunk{Grant: g, Data: make([]byte, n)})
	return st.CloseAndRecv()
}

// TestSpeedGrants: the owner's client measures directly; a peer worker only
// with a speed offer made for it - which serves no file - and a file offer
// serves no speed test.
func TestSpeedGrants(t *testing.T) {
	e := newAuthzEnv(t)
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644)
	cc := pb.NewCopyClient(e.alice)
	token := bytes.Repeat([]byte{7}, 32)
	w2 := e.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "w2", Hosts: []string{"127.0.0.1"}})
	w3 := e.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "w3", Hosts: []string{"127.0.0.1"}})

	// the owner's client, directly
	if n, err := sourceAll(t, e.alice, nil, 1<<20); err != nil || n != 1<<20 {
		t.Fatalf("source: %d %v", n, err)
	}
	if r, err := sinkSome(t, e.alice, nil, 1<<20); err != nil || r.Bytes != 1<<20 {
		t.Fatalf("sink: %v %v", r, err)
	}
	// a worker without a grant
	if _, err := sourceAll(t, w2, nil, 1<<20); status.Code(err) != codes.NotFound {
		t.Errorf("a worker without a grant: %v", err)
	}
	// a speed offer: for w2, Source and Sink only
	if _, err := cc.Offer(cctx(t), &pb.CopyOfferRequest{Speed: true, Path: root, Peer: "w2", TransferId: "speed-000", Token: token, Ttl: durationpb.New(time.Minute)}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a speed offer with a path: %v", err)
	}
	if _, err := cc.Offer(cctx(t), &pb.CopyOfferRequest{Speed: true, Peer: "w2", TransferId: "speed-001", Token: token, Ttl: durationpb.New(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	sg := &pb.CopyGrant{TransferId: "speed-001", Token: token}
	if n, err := sourceAll(t, w2, sg, 1<<20); err != nil || n != 1<<20 {
		t.Errorf("the peer's source: %d %v", n, err)
	}
	if r, err := sinkSome(t, w2, sg, 4096); err != nil || r.Bytes != 4096 {
		t.Errorf("the peer's sink: %v %v", r, err)
	}
	if _, err := sourceAll(t, w3, sg, 1<<20); status.Code(err) != codes.PermissionDenied {
		t.Errorf("another peer: %v", err)
	}
	if _, err := copyRead(t, w2, sg, "a.txt"); status.Code(err) != codes.NotFound {
		t.Errorf("a speed offer reading a file: %v", err)
	}
	if _, err := pb.NewCopyClient(w2).List(cctx(t), &pb.CopyListRequest{Grant: sg}); status.Code(err) != codes.NotFound {
		t.Errorf("a speed offer listing: %v", err)
	}
	// a file offer is no speed test's
	if _, err := cc.Offer(cctx(t), &pb.CopyOfferRequest{Path: root, Peer: "w2", TransferId: "files-001", Token: token, Ttl: durationpb.New(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceAll(t, w2, &pb.CopyGrant{TransferId: "files-001", Token: token}, 1<<20); status.Code(err) != codes.NotFound {
		t.Errorf("a file offer for a speed test: %v", err)
	}
	// limits: no more than max_bytes, no more than the duration cap
	if n, err := sourceAll(t, e.alice, nil, 1000); err != nil || n != 1000 {
		t.Errorf("max_bytes 1000: %d %v", n, err)
	}
}
