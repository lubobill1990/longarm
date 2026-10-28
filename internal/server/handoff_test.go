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

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// handoffFetch fetches a transfer as conn's identity.
func handoffFetch(t *testing.T, conn *grpc.ClientConn, id string, token []byte) ([]byte, error) {
	t.Helper()
	st, err := pb.NewHandoffClient(conn).Fetch(cctx(t), &pb.HandoffFetchRequest{TransferId: id, Token: token})
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for {
		ch, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return b.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		b.Write(ch.Data)
	}
}

// TestHandoffOfferScope: an offered bundle goes once, to the named peer
// worker with the token, before the expiry - and only bundles handoff made
// can be offered.
func TestHandoffOfferScope(t *testing.T) {
	e := newAuthzEnv(t)
	dir := t.TempDir()
	content := bytes.Repeat([]byte("bundle "), 1000)
	bundle := filepath.Join(dir, "longarm-handoff-abc12345-x.bundle")
	os.WriteFile(bundle, content, 0o600)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret"), 0o600)
	token := bytes.Repeat([]byte{7}, 32)
	hc := pb.NewHandoffClient(e.alice)
	offer := func(id, path, peer string, ttl time.Duration) (*pb.HandoffOfferResponse, error) {
		return hc.Offer(cctx(t), &pb.HandoffOfferRequest{Path: path, Peer: peer, TransferId: id, Token: token,
			Ttl: durationpb.New(ttl), MaxBytes: 1 << 20})
	}
	if _, err := offer("transfer-0", filepath.Join(dir, "secret.txt"), "w2", time.Minute); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("offering another file: %v", err)
	}
	if _, err := offer("transfer-0", bundle, "w2", time.Hour); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a ttl over the limit: %v", err)
	}
	res, err := offer("transfer-1", bundle, "w2", time.Minute)
	if err != nil || res.Size != int64(len(content)) {
		t.Fatalf("offer: %v %v", res, err)
	}
	w2 := e.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "w2", Hosts: []string{"127.0.0.1"}})
	w3 := e.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "w3", Hosts: []string{"127.0.0.1"}})
	if _, err := handoffFetch(t, e.alice, "transfer-1", token); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a client certificate fetching: %v", err)
	}
	if _, err := handoffFetch(t, w3, "transfer-1", token); status.Code(err) != codes.PermissionDenied {
		t.Errorf("another peer: %v", err)
	}
	if _, err := handoffFetch(t, w2, "transfer-1", bytes.Repeat([]byte{8}, 32)); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a wrong token: %v", err)
	}
	if _, err := handoffFetch(t, w2, "transfer-2", token); status.Code(err) != codes.NotFound {
		t.Errorf("another transfer: %v", err)
	}
	got, err := handoffFetch(t, w2, "transfer-1", token)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("the peer's fetch: %v (%d bytes)", err, len(got))
	}
	if _, err := handoffFetch(t, w2, "transfer-1", token); status.Code(err) != codes.NotFound {
		t.Errorf("reused: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(e.srv.DataDir, "staging", "handoff")); len(ents) != 0 {
		t.Errorf("staging copy left after the fetch: %v", ents)
	}
	// expired
	if _, err := offer("transfer-3", bundle, "w2", 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := handoffFetch(t, w2, "transfer-3", token); status.Code(err) != codes.NotFound {
		t.Errorf("expired: %v", err)
	}
	// a worker certificate may not offer or pull
	if _, err := pb.NewHandoffClient(w2).Offer(cctx(t), &pb.HandoffOfferRequest{Path: bundle, Peer: "w3", TransferId: "transfer-4",
		Token: token, Ttl: durationpb.New(time.Minute), MaxBytes: 1 << 20}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a worker offering: %v", err)
	}
	// Pull without a dialer: FailedPrecondition (the client falls back to its relay)
	if _, err := hc.Pull(cctx(t), &pb.HandoffPullRequest{From: "w2", FromEndpoints: []string{"tcp://127.0.0.1:1"}, TransferId: "transfer-5",
		Token: token, Size: 1, Sha256: make([]byte, 32), Header: &pb.SyncHeader{Repo: "app", Commit: "x", Path: dir}}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("pull without a dialer: %v", err)
	}
}
