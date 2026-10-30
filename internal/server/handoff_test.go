package server

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
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

// TestHandoffOwnedWorker: a worker with an owner takes part in handoffs of
// its owner only - not of a user it is shared with, not of an admin
// certificate of another user (shared with them too, so that only the
// handoff rule stands in the way) - on every handoff method (Offer, Fetch of an
// offer made before the owner changed, Pull, PushHandoff).
func TestHandoffOwnedWorker(t *testing.T) {
	e := newAuthzEnv(t)
	dir := t.TempDir()
	bundle := filepath.Join(dir, "longarm-handoff-abc12345-x.bundle")
	os.WriteFile(bundle, []byte("bundle"), 0o600)
	token := bytes.Repeat([]byte{7}, 32)
	offer := func(c *grpc.ClientConn, id string) error {
		_, err := pb.NewHandoffClient(c).Offer(cctx(t), &pb.HandoffOfferRequest{Path: bundle, Peer: "w2", TransferId: id, Token: token,
			Ttl: durationpb.New(time.Minute), MaxBytes: 1 << 20})
		return err
	}
	pull := func(c *grpc.ClientConn) error {
		_, err := pb.NewHandoffClient(c).Pull(cctx(t), &pb.HandoffPullRequest{From: "w2", FromEndpoints: []string{"tcp://127.0.0.1:1"},
			TransferId: "transfer-p", Token: token, Size: 1, Sha256: make([]byte, 32), Header: &pb.SyncHeader{Repo: "app", Commit: "x", Path: dir}})
		return err
	}
	push := func(c *grpc.ClientConn) error {
		_, err := pb.NewWorkspaceClient(c).PushHandoff(cctx(t), &pb.PushHandoffRequest{Dir: dir, Url: "https://example.com/r.git",
			Branch: "longarm/handoff/alice/abc12345-x", GitAuth: &pb.GitAuth{Scheme: jobs.GitAuthBearer, Secret: []byte("example-token")}})
		return err
	}
	denied := func(err error) bool {
		return status.Code(err) == codes.PermissionDenied && strings.Contains(status.Convert(err).Message(), "another user's")
	}

	// a fleet worker (no owner): the existing rules
	if err := offer(e.alice, "transfer-fleet"); err != nil {
		t.Fatalf("fleet worker: %v", err)
	}

	wa, _ := tenancy.LoadWorkerAccess("")
	wa.Set(&pb.WorkerAccess{Version: 1, Owner: "alice", Shares: &pb.WorkerShares{Users: []string{"bob", "root", "carol"}}})
	e.srv.WorkerAccess = wa
	defer func() { e.srv.WorkerAccess = nil }()
	if err := offer(e.alice, "transfer-own"); err != nil {
		t.Fatalf("the owner's offer: %v", err)
	}
	hubAdmin := e.dial(pki.IssueOptions{Role: pki.RoleAdmin, Name: "hub-admin", User: "carol"})
	for name, c := range map[string]*grpc.ClientConn{"bob (shared)": e.bob, "root (admin)": e.root, "hub-admin (carol's admin cert)": hubAdmin} {
		if err := offer(c, "transfer-x"); !denied(err) {
			t.Errorf("%s offered on alice's worker: %v", name, err)
		}
		if err := pull(c); !denied(err) {
			t.Errorf("%s pulled into alice's worker: %v", name, err)
		}
		if err := push(c); !denied(err) {
			t.Errorf("%s pushed a handoff branch from alice's worker: %v", name, err)
		}
	}
	// the worker changes hands between an offer and its fetch: refused
	w2 := e.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "w2", Hosts: []string{"127.0.0.1"}})
	wa.Set(&pb.WorkerAccess{Version: 2, Owner: "bob"})
	if _, err := handoffFetch(t, w2, "transfer-own", token); status.Code(err) != codes.PermissionDenied {
		t.Errorf("fetch of alice's offer from bob's worker: %v", err)
	}
	// an owner pinned in longarmd.toml ([access] owners)
	e.srv.WorkerAccess = nil
	e.srv.Access = &Access{Owners: []string{"carol"}}
	defer func() { e.srv.Access = nil }()
	if err := offer(e.alice, "transfer-pinned"); !denied(err) {
		t.Errorf("alice on carol's pinned worker: %v", err)
	}
}
