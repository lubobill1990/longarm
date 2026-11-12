package server

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"longarm/internal/pki"
	"longarm/internal/tenancy"
	pb "longarm/proto/longarm/v1"
)

// copyRead reads rel of a copy offer as conn's identity.
func copyRead(t *testing.T, conn *grpc.ClientConn, g *pb.CopyGrant, rel string) ([]byte, error) {
	t.Helper()
	st, err := pb.NewCopyClient(conn).Read(cctx(t), &pb.CopyReadRequest{Grant: g, Path: rel})
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

// TestCopyOfferScope: an offered tree is read by the named peer worker with
// the token - any number of times, only below the offered path, until it is
// revoked (by its owner only) or expires.
func TestCopyOfferScope(t *testing.T) {
	e := newAuthzEnv(t)
	root := t.TempDir()
	src := filepath.Join(root, "src")
	os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "sub", "a.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(root, "secret.txt"), []byte("secret"), 0o600)
	token := bytes.Repeat([]byte{7}, 32)
	cc := pb.NewCopyClient(e.alice)
	offer := func(c pb.CopyClient, id, path string, ttl time.Duration) (*pb.CopyOfferResponse, error) {
		return c.Offer(cctx(t), &pb.CopyOfferRequest{Path: path, Peer: "w2", TransferId: id, Token: token, Ttl: durationpb.New(ttl)})
	}
	if _, err := offer(cc, "transfer-0", src, 25*time.Hour); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a ttl over the limit: %v", err)
	}
	if _, err := offer(cc, "transfer-0", filepath.Join(root, "missing"), time.Minute); status.Code(err) != codes.NotFound {
		t.Fatalf("a missing path: %v", err)
	}
	res, err := offer(cc, "transfer-1", src, time.Minute)
	if err != nil || !res.Root.IsDir {
		t.Fatalf("offer: %v %v", res, err)
	}
	if _, err := offer(cc, "transfer-1", src, time.Minute); status.Code(err) != codes.AlreadyExists {
		t.Errorf("the same transfer id again: %v", err)
	}
	g := &pb.CopyGrant{TransferId: "transfer-1", Token: token}
	w2 := e.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "w2", Hosts: []string{"127.0.0.1"}})
	w3 := e.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "w3", Hosts: []string{"127.0.0.1"}})
	if _, err := copyRead(t, e.alice, g, "sub/a.txt"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a client certificate reading: %v", err)
	}
	if _, err := copyRead(t, w3, g, "sub/a.txt"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("another peer: %v", err)
	}
	if _, err := copyRead(t, w2, &pb.CopyGrant{TransferId: "transfer-1", Token: bytes.Repeat([]byte{8}, 32)}, "sub/a.txt"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a wrong token: %v", err)
	}
	for _, rel := range []string{"../secret.txt", "sub/../../secret.txt", "/etc/passwd", "sub//a.txt"} {
		if _, err := copyRead(t, w2, g, rel); status.Code(err) != codes.InvalidArgument {
			t.Errorf("read %q: %v", rel, err)
		}
	}
	for range 2 { // any number of times
		if got, err := copyRead(t, w2, g, "sub/a.txt"); err != nil || string(got) != "hello" {
			t.Fatalf("the peer's read: %q %v", got, err)
		}
	}
	list, err := pb.NewCopyClient(w2).List(cctx(t), &pb.CopyListRequest{Grant: g})
	if err != nil || len(list.Entries) != 2 {
		t.Fatalf("list: %v %v", list, err)
	}
	if _, err := copyRead(t, w2, g, "sub"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("reading a directory: %v", err)
	}
	// revoked: by its owner only
	if _, err := pb.NewCopyClient(e.bob).Revoke(cctx(t), &pb.CopyRevokeRequest{TransferId: "transfer-1"}); status.Code(err) != codes.NotFound {
		t.Errorf("bob revoking alice's offer: %v", err)
	}
	if _, err := cc.Revoke(cctx(t), &pb.CopyRevokeRequest{TransferId: "transfer-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := copyRead(t, w2, g, "sub/a.txt"); status.Code(err) != codes.NotFound {
		t.Errorf("revoked: %v", err)
	}
	// expired
	if _, err := offer(cc, "transfer-2", src, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := copyRead(t, w2, &pb.CopyGrant{TransferId: "transfer-2", Token: token}, "sub/a.txt"); status.Code(err) != codes.NotFound {
		t.Errorf("expired: %v", err)
	}
	// a worker certificate may not offer or pull
	if _, err := offer(pb.NewCopyClient(w2), "transfer-3", src, time.Minute); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a worker offering: %v", err)
	}
	// Pull without a dialer
	if _, err := pullAll(t, cc, &pb.CopyPullRequest{From: "w2", FromEndpoints: []string{"tcp://127.0.0.1:1"}, Grant: g, Path: root}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("pull without a dialer: %v", err)
	}
	// a single file offered: only "" reads
	if _, err := offer(cc, "transfer-4", filepath.Join(src, "sub", "a.txt"), time.Minute); err != nil {
		t.Fatal(err)
	}
	g4 := &pb.CopyGrant{TransferId: "transfer-4", Token: token}
	if got, err := copyRead(t, w2, g4, ""); err != nil || string(got) != "hello" {
		t.Errorf("the offered file: %q %v", got, err)
	}
	if _, err := copyRead(t, w2, g4, "a.txt"); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a path below a file: %v", err)
	}
}

// TestCopyOwnedWorker: a worker with an owner copies for its owner only.
func TestCopyOwnedWorker(t *testing.T) {
	e := newAuthzEnv(t)
	dir := t.TempDir()
	token := bytes.Repeat([]byte{7}, 32)
	wa, _ := tenancy.LoadWorkerAccess("")
	wa.Set(&pb.WorkerAccess{Version: 1, Owner: "alice", Shares: &pb.WorkerShares{Users: []string{"bob"}}})
	e.srv.WorkerAccess = wa
	defer func() { e.srv.WorkerAccess = nil }()
	offer := func(c *grpc.ClientConn, id string) error {
		_, err := pb.NewCopyClient(c).Offer(cctx(t), &pb.CopyOfferRequest{Path: dir, Peer: "w2", TransferId: id, Token: token, Ttl: durationpb.New(time.Minute)})
		return err
	}
	if err := offer(e.alice, "transfer-own"); err != nil {
		t.Fatalf("the owner's offer: %v", err)
	}
	if err := offer(e.bob, "transfer-bob"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob (shared) offering from alice's worker: %v", err)
	}
	if _, err := pullAll(t, pb.NewCopyClient(e.bob), &pb.CopyPullRequest{From: "w2", FromEndpoints: []string{"tcp://127.0.0.1:1"},
		Grant: &pb.CopyGrant{TransferId: "transfer-x", Token: token}, Path: dir}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("bob pulling into alice's worker: %v", err)
	}
	// the worker changes hands after the offer: its reads are refused
	w2 := e.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "w2", Hosts: []string{"127.0.0.1"}})
	wa.Set(&pb.WorkerAccess{Version: 2, Owner: "bob"})
	if _, err := pb.NewCopyClient(w2).List(cctx(t), &pb.CopyListRequest{Grant: &pb.CopyGrant{TransferId: "transfer-own", Token: token}}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("list of alice's offer on bob's worker: %v", err)
	}
}

// pullAll runs a Pull to its end: the last progress.
func pullAll(t *testing.T, c pb.CopyClient, req *pb.CopyPullRequest) (*pb.CopyProgress, error) {
	t.Helper()
	st, err := c.Pull(cctx(t), req)
	if err != nil {
		return nil, err
	}
	var last *pb.CopyProgress
	for {
		p, err := st.Recv()
		if errors.Is(err, io.EOF) {
			if last == nil || !last.Done {
				return last, errors.New("no final progress")
			}
			return last, nil
		}
		if err != nil {
			return last, err
		}
		last = p
	}
}

// TestCopyPull: a tree is copied as it is - files with their contents,
// empty directories, no symlinks; a file already there is skipped, a
// partial file resumed (or, not a prefix of the source, started again);
// pulling again sends nothing. The worker is source and target at once: its
// peer dialer comes back to it with w2's certificate.
func TestCopyPull(t *testing.T) {
	e := newAuthzEnv(t)
	e.srv.PeerDial = func(string, []string) (grpc.ClientConnInterface, func(), error) {
		c := e.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "w2", Hosts: []string{"127.0.0.1"}})
		return c, func() { c.Close() }, nil
	}
	root := t.TempDir()
	src := filepath.Join(root, "src")
	big := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB: several chunks
	other := bytes.Repeat([]byte("x"), 200<<10)
	files := map[string][]byte{"a.txt": []byte("hello"), "deep/b/big.bin": big, "deep/other.bin": other, "same.txt": []byte("same")}
	for rel, b := range files {
		p := filepath.Join(src, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, b, 0o644)
	}
	os.MkdirAll(filepath.Join(src, "empty"), 0o755)
	links := 0
	if runtime.GOOS != "windows" { // a symlink out of the tree: not copied
		if os.Symlink(filepath.Join(root, "outside"), filepath.Join(src, "link")) == nil {
			links = 1
		}
	}
	dst := filepath.Join(root, "dst")
	os.MkdirAll(filepath.Join(dst, "deep", "b"), 0o755)
	os.WriteFile(filepath.Join(dst, "same.txt"), []byte("same"), 0o644)                          // up to date
	os.WriteFile(filepath.Join(dst, "deep", "b", "big.bin"+PartialSuffix), big[:300<<10], 0o644) // resumed
	os.WriteFile(filepath.Join(dst, "deep", "other.bin"+PartialSuffix), []byte("not a prefix"), 0o644)

	token := bytes.Repeat([]byte{9}, 32)
	cc := pb.NewCopyClient(e.alice)
	if _, err := cc.Offer(cctx(t), &pb.CopyOfferRequest{Path: src, Peer: "w2", TransferId: "transfer-tree", Token: token, Ttl: durationpb.New(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	req := &pb.CopyPullRequest{From: "wsrc", FromEndpoints: []string{"tcp://127.0.0.1:1"}, Grant: &pb.CopyGrant{TransferId: "transfer-tree", Token: token}, Path: dst}
	p, err := pullAll(t, cc, req)
	if err != nil {
		t.Fatalf("pull: %v (%v)", err, p)
	}
	total := int64(0)
	for _, b := range files {
		total += int64(len(b))
	}
	if p.Files != 4 || p.FilesTotal != 4 || p.Bytes != total || p.BytesTotal != total || p.UpToDate != 1 || p.Resumed != 300<<10 || p.Links != int64(links) {
		t.Errorf("progress %v", p)
	}
	if want := total - int64(len("same")) - 300<<10; p.Sent != want {
		t.Errorf("sent %d, want %d", p.Sent, want)
	}
	if abs, _ := filepath.Abs(dst); p.AbsPath != abs {
		t.Errorf("abs_path %q, want %q", p.AbsPath, abs)
	}
	for rel, b := range files {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s: %d bytes, %v", rel, len(got), err)
		}
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(rel)) + PartialSuffix); err == nil {
			t.Errorf("%s: partial file left", rel)
		}
	}
	if fi, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !fi.IsDir() {
		t.Errorf("empty directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); err == nil {
		t.Error("the symlink was copied")
	}
	// again: everything is there
	if p, err := pullAll(t, cc, req); err != nil || p.UpToDate != 4 || p.Sent != 0 {
		t.Errorf("again: %v %v", p, err)
	}
	// one file to an exact path
	if _, err := cc.Offer(cctx(t), &pb.CopyOfferRequest{Path: filepath.Join(src, "a.txt"), Peer: "w2", TransferId: "transfer-file", Token: token, Ttl: durationpb.New(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	one := filepath.Join(root, "one", "copy.txt")
	p, err = pullAll(t, cc, &pb.CopyPullRequest{From: "wsrc", FromEndpoints: []string{"tcp://127.0.0.1:1"}, Grant: &pb.CopyGrant{TransferId: "transfer-file", Token: token}, Path: one})
	if err != nil || p.Files != 1 || p.Sent != 5 {
		t.Fatalf("a file: %v %v", p, err)
	}
	if got, _ := os.ReadFile(one); string(got) != "hello" {
		t.Errorf("a file: %q", got)
	}
	// a wrong token: refused by the source, nothing written
	bad := &pb.CopyPullRequest{From: "wsrc", FromEndpoints: []string{"tcp://127.0.0.1:1"}, Grant: &pb.CopyGrant{TransferId: "transfer-file", Token: bytes.Repeat([]byte{1}, 32)},
		Path: filepath.Join(root, "bad")}
	if _, err := pullAll(t, cc, bad); status.Code(err) != codes.PermissionDenied {
		t.Errorf("a wrong token: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bad")); err == nil {
		t.Error("written with a wrong token")
	}
}
