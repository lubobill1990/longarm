package main

import (
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestPlacementNotReusedForExplicitTargets: the worker the hub assigned to
// an automatic placement is unconstrained (any endorsed worker) for that
// operation only - consumed once, by its job id - and never answers a
// later explicit request for its name, which is held to the name.
func TestPlacementNotReusedForExplicitTargets(t *testing.T) {
	prod := &pb.NodeInfo{Name: "production", Id: "3f2a9c1e-4b5d-4c3e-8f00-1234567890ab", State: "approved", Role: "worker",
		Endpoints: []string{"tcp://127.0.0.1:7701"}}
	s := &session{cfg: &client.Config{Hub: hubclient.Config{Address: "127.0.0.1:1"}},
		placements: map[string]*pb.NodeInfo{"J1": prod}, nodes: []*pb.NodeInfo{prod}, nodesAt: time.Now()}
	// an explicit request for the placed worker's name: held to the name
	w, err := s.worker("production")
	if err != nil || w.TrustAny || w.TrustName != "production" {
		t.Fatalf("explicit target after a placement: %+v %v", w, err)
	}
	// another operation's job id, or another target: no placement
	if _, ok := s.takePlaced("J2", "production"); ok {
		t.Fatal("placement of another job used")
	}
	if _, ok := s.takePlaced("J1", "staging"); ok {
		t.Fatal("placement used for another target")
	}
	s.placements["J1"] = prod
	w, ok := s.takePlaced("J1", "production")
	if !ok || !w.TrustAny || w.ID != prod.Id {
		t.Fatalf("the placement's own operation: %+v %v", w, ok)
	}
	if _, ok := s.takePlaced("J1", "production"); ok {
		t.Fatal("a placement used twice")
	}
}

// TestLinkSignsAuthenticatedEnd: the consent signs the link end the
// verification handshake authenticated; a hub whose listing (looked up
// separately) names another node is refused instead of signed.
func TestLinkSignsAuthenticatedEnd(t *testing.T) {
	a := "3f2a9c1e-4b5d-4c3e-8f00-1234567890ab"
	b := "77aa0000-0000-4000-8000-000000000001"
	listedA := &pb.NodeInfo{Name: "frontend", Id: a}
	authedA := pki.Identity{Role: pki.RoleWorker, Name: "frontend", ID: a}
	if name, id, err := signedEnd(listedA, authedA); err != nil || name != "frontend" || id != a {
		t.Fatalf("matching: %s %s %v", name, id, err)
	}
	// verification met genuine A, the listing hands B's identity for signing
	if _, _, err := signedEnd(&pb.NodeInfo{Name: "frontend", Id: b}, authedA); err == nil {
		t.Fatal("signed another node than the one that answered")
	}
	if _, _, err := signedEnd(&pb.NodeInfo{Name: "frontend2", CertName: "frontend2", Id: a}, authedA); err == nil {
		t.Fatal("signed another name than the one that answered")
	}
	// a certificate without an ID: the name only is signed
	if name, id, err := signedEnd(listedA, pki.Identity{Role: pki.RoleWorker, Name: "frontend"}); err != nil || name != "frontend" || id != "" {
		t.Fatalf("legacy certificate: %s %q %v", name, id, err)
	}
}
