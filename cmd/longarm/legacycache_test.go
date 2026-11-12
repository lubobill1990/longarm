package main

import (
	"crypto/x509"
	"encoding/pem"
	"path/filepath"
	"testing"
	"time"

	"longarm/internal/client"
	"longarm/internal/hubclient"
	"longarm/internal/nodeid"
	"longarm/internal/pki"
	pb "longarm/proto/longarm/v1"
)

// TestCachedLegacySerials: the hub's cached node list keeps a node's legacy
// serials next to its ID, so a client on the cache (hub unreachable) still
// accepts the worker's certificate from before node IDs that the hub last
// reported - and still refuses any other certificate without an ID.
func TestCachedLegacySerials(t *testing.T) {
	ca, err := pki.InitCA(filepath.Join(t.TempDir(), "ca"), "test CA", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	issue := func() *x509.Certificate {
		certPEM, _, err := ca.Issue(pki.IssueOptions{Role: pki.RoleWorker, Name: "box"})
		if err != nil {
			t.Fatal(err)
		}
		blk, _ := pem.Decode(certPEM)
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	legacy, other := issue(), issue()
	id := nodeid.New()

	dir := t.TempDir()
	cfg := &client.Config{Dir: dir, Hub: hubclient.Config{Address: "127.0.0.1:1"}}
	(&session{cfg: cfg}).saveCache([]*pb.NodeInfo{{Name: "box", Id: id, State: "approved", Role: "worker",
		Endpoints: []string{"tcp://127.0.0.1:7701"}, LegacySerials: []string{pki.Serial(legacy)}}})

	defer func(v bool) { noHub = v }(noHub)
	noHub = true // the hub is not asked: known workers come from the cache
	for _, target := range []string{"box", id} {
		s := &session{cfg: cfg}
		w, err := s.worker(target)
		if err != nil {
			t.Fatalf("%s from the cache: %v", target, err)
		}
		if s.nodesLive {
			t.Fatal("expected the cached node list")
		}
		check := func(c *x509.Certificate) error {
			ident, _ := pki.IdentityFromCert(c)
			return pki.CheckNodeID(c, ident, w.DialTarget())
		}
		if err := check(legacy); err != nil {
			t.Fatalf("%s: the cached legacy serial refused: %v", target, err)
		}
		if check(other) == nil {
			t.Fatalf("%s: a certificate without an id the hub never reported accepted", target)
		}
	}
}
