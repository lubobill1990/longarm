package portlink

import (
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/nodeid"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestCheckEnd: the owner's consent binds the link's ends - by node ID
// when it names them, else by the ends' endorsed names. A hub that
// relabels another endorsed worker of the owner (her "development") as the
// consented end ("frontend") gets refused either way.
func TestCheckEnd(t *testing.T) {
	front, db, dev := nodeid.New(), nodeid.New(), nodeid.New()
	withIDs := &pb.PortLinkSpec{Id: "lk-1", From: "frontend", To: "database", FromId: front, ToId: db}
	legacy := &pb.PortLinkSpec{Id: "lk-2", From: "frontend", To: "database"}
	idOf := func(name, id string) pki.Identity { return pki.Identity{Role: pki.RoleWorker, Name: name, ID: id} }
	end := func(name, id string) *pki.Endorsement {
		return &pki.Endorsement{Kind: pki.KindWorker, User: "alice", Name: name, ID: id}
	}

	for _, c := range []struct {
		name string
		s    *pb.PortLinkSpec
		from bool
		id   pki.Identity
		e    *pki.Endorsement
		ok   bool
	}{
		{"by id", withIDs, true, idOf("frontend", front), end("frontend", front), true},
		{"by id, renamed end", withIDs, true, idOf("fe2", front), end("frontend", front), true},
		{"by id, no e2e", withIDs, false, idOf("database", db), nil, true},
		// the hub labels alice's development as frontend (its certificate
		// says frontend, it keeps its own endorsed id)
		{"relabel, id consent", withIDs, true, idOf("frontend", dev), end("development", dev), false},
		{"relabel, id consent, no e2e", withIDs, true, idOf("frontend", dev), nil, false},
		{"relabel, legacy consent", legacy, true, idOf("frontend", dev), end("development", dev), false},
		{"legacy consent, endorsed name", legacy, false, idOf("database", db), end("database", db), true},
		{"legacy consent without e2e evidence", legacy, true, idOf("frontend", front), nil, false},
		{"id consent, name-bound endorsement of the end", withIDs, false, idOf("database", db), end("database", ""), true},
		{"id consent, name-bound endorsement of another name", withIDs, false, idOf("database", db), end("development", ""), false},
	} {
		err := CheckEnd(c.s, c.from, c.id, c.e)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// ConsentMatches: a link with other node IDs than the consent's
	l := &pb.PortLink{Id: "lk-1", From: "frontend", To: "database", FromId: dev, ToId: db}
	if err := ConsentMatches(withIDs, l); err == nil || !strings.Contains(err.Error(), "not what its owner signed") {
		t.Fatalf("other ids: %v", err)
	}
}
