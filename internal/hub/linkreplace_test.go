package hub

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestLinkReplaceProofs: a pairing replaces a node only with a proof made
// by that node's key over this pairing's CSRs. Someone holding only the
// certificate (it is no secret), a proof made for another pairing, a
// certificate of another CA or a proof for the wrong kind of node is
// refused before any code is shown; a revoked node can't be replaced.
func TestLinkReplaceProofs(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	bob := newWeb(t, h)
	bob.passwordSignIn(h, "bob-web", "user", "bob")
	// bob's worker, through a pairing
	codeCh := make(chan string, 1)
	done := make(chan error, 1)
	var w *hubclient.JoinResult
	go func() {
		var err error
		w, err = hubclient.Link(context.Background(), h.Addr("grpc"), hubclient.LinkOptions{Kind: linkWorker,
			JoinOptions: hubclient.JoinOptions{Name: "bobs-box", Fingerprint: pki.Fingerprint(th.ca.Cert)},
			Show:        func(st *pb.StartLinkResponse) { codeCh <- st.UserCode }})
		done <- err
	}()
	select {
	case code := <-codeCh:
		_, confirm := bob.lookup(code)
		if c, body := bob.decide(confirm, "approve"); c != 200 {
			t.Fatalf("approve: %d %s", c, body)
		}
	case err := <-done:
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Link did not finish")
	}

	h.joinLimit = newIPLimiter(100, 100)
	ec := enrollClient(t, h)
	start := func(kind string, proof func(csr []byte) *pb.LinkReplace) error {
		t.Helper()
		_, csr, err := pki.NewKeyAndCSR("", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ec.StartLink(tctx(t), &pb.StartLinkRequest{Kind: kind, CsrPem: csr, Hostname: "bobs-box", Os: "windows", Arch: "amd64",
			Version: "1.2.3", Replaces: []*pb.LinkReplace{proof(csr)}})
		return err
	}
	sign := func(certPEM, keyPEM, csr []byte) *pb.LinkReplace {
		sig, err := pki.SignReplace(certPEM, keyPEM, pki.ReplaceMessage(csr, nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		return &pb.LinkReplace{CertPem: certPEM, Signature: sig}
	}
	// a key of its own, with a certificate of the same CA naming bob's worker
	squatCert, squatKey, err := th.ca.Issue(pki.IssueOptions{Role: pki.RoleClient, Name: "squatter", Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	otherCA, err := pki.InitCA(t.TempDir(), "other", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	fakeCert, fakeKey, _ := otherCA.Issue(pki.IssueOptions{Role: pki.RoleWorker, Name: "bobs-box", Validity: time.Hour})
	for name, c := range map[string]struct {
		kind  string
		proof func(csr []byte) *pb.LinkReplace
		code  codes.Code
	}{
		"the certificate without its key": {linkWorker, func(csr []byte) *pb.LinkReplace {
			p := sign(squatCert, squatKey, csr)
			p.CertPem = w.Resp.CertPem
			return p
		}, codes.PermissionDenied},
		"a proof for another pairing": {linkWorker, func([]byte) *pb.LinkReplace {
			_, other, _ := pki.NewKeyAndCSR("", nil)
			return sign(w.Resp.CertPem, w.KeyPEM, other)
		}, codes.PermissionDenied},
		"another CA's certificate": {linkWorker, func(csr []byte) *pb.LinkReplace { return sign(fakeCert, fakeKey, csr) }, codes.PermissionDenied},
		"a worker for a device":    {linkClient, func(csr []byte) *pb.LinkReplace { return sign(w.Resp.CertPem, w.KeyPEM, csr) }, codes.InvalidArgument},
	} {
		if err := start(c.kind, c.proof); status.Code(err) != c.code {
			t.Errorf("%s: %v, want %v", name, err, c.code)
		}
	}
	// the real proof is taken
	if err := start(linkWorker, func(csr []byte) *pb.LinkReplace { return sign(w.Resp.CertPem, w.KeyPEM, csr) }); err != nil {
		t.Fatalf("bob's own proof: %v", err)
	}
	// once removed, it proves nothing
	if _, err := h.Revoke("test", "bobs-box"); err != nil {
		t.Fatal(err)
	}
	if err := start(linkWorker, func(csr []byte) *pb.LinkReplace { return sign(w.Resp.CertPem, w.KeyPEM, csr) }); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a revoked worker's proof: %v", err)
	}
}
