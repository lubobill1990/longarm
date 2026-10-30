package hub

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/nodeid"
	"github.com/lubobill1990/longarm/internal/pki"
)

// LocalIssueOptions is a certificate issued from the hub's data directory
// by another process than the hub (longarm-hub issue and its aliases:
// longarm-hub-relay-cert, longarm-hub-release-cert).
type LocalIssueOptions struct {
	Role     pki.Role
	Name     string
	Hosts    []string
	Validity time.Duration // 0: by role (pki)
}

// LocalIssued is a certificate from IssueLocal.
type LocalIssued struct {
	CertPEM, KeyPEM []byte
	CAPEM           []byte
	NodeID          string
	Serial          string
	NewNode         bool // the node was created (no live node of the name and role)
}

// IssueLocal issues a certificate from the hub's CA for the node o names,
// next to a running hub (az container exec), the way the hub's own issue()
// does: the certificate carries the node's ID (URI SAN urn:longarm:node:<id>)
// and its serial is recorded against that node, so the hub maps it by ID -
// never by name, which it refuses for a name with history (checkNode).
//
//   - A live (non-revoked, approved) node of the role with that name: the
//     certificate is that node's. Its record is not touched (the running hub
//     holds it in memory and writes it back); only the certificate is
//     recorded (an append-only row).
//   - None: a new approved node with a fresh ID is created together with
//     the certificate record (one transaction; the database refuses a name
//     another live node has). The running hub adopts it on the node's first
//     call (nodeForCertLocked), as it reads recorded certificates.
//
// The database is the hub's SQLite file in WAL mode with SQLite's locking and
// a busy timeout (store.Open), which several processes may open at once (as
// longarm-hub join-code and user add do); Litestream replicates every
// committed write.
func IssueLocal(cfg *Config, o LocalIssueOptions) (*LocalIssued, error) {
	switch o.Role {
	case pki.RoleWorker, pki.RoleClient, pki.RoleAdmin, pki.RoleRelay, pki.RoleRelease:
	default:
		return nil, fmt.Errorf("role %q: want worker, client, admin, relay or release", o.Role)
	}
	if o.Name == "" {
		return nil, errors.New("no node name")
	}
	ca, err := pki.LoadCA(filepath.Join(cfg.DataDir, CADir))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath()), 0o700); err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.DatabasePath())
	if err != nil {
		return nil, err
	}
	defer st.Close()
	recs, err := st.Nodes()
	if err != nil {
		return nil, err
	}
	var rec *store.Node
	for _, r := range recs {
		if r.State == stateRevoked || r.Name != o.Name && r.DialName() != o.Name && !slices.Contains(r.PendingNames, o.Name) {
			continue
		}
		if r.Role != string(o.Role) {
			return nil, fmt.Errorf("the name %s belongs to node %s, a %s (not a %s)", o.Name, nodeid.Short(r.ID), r.Role, o.Role)
		}
		if r.State != stateApproved {
			return nil, fmt.Errorf("node %s (%s) is %s: approve it first (longarm admin approve %s)", r.Name, nodeid.Short(r.ID), r.State, r.ID)
		}
		if r.Name != o.Name {
			// renamed: its certificates' new name is its name (issueHosts)
			return nil, fmt.Errorf("the name %s is an older name of node %s (now %s): issue for %s", o.Name, nodeid.Short(r.ID), r.Name, r.Name)
		}
		rec = r
		break
	}
	res := &LocalIssued{CAPEM: ca.CertPEM}
	if rec == nil {
		if err := checkNewName(o.Name); err != nil {
			return nil, err
		}
		now := time.Now().UTC()
		rec = &store.Node{ID: nodeid.New(), Name: o.Name, Role: string(o.Role), State: stateApproved, JoinedAt: now, ApprovedAt: now, CertHasID: true}
		res.NewNode = true
	}
	if res.NewNode && o.Role != pki.RoleWorker {
		rec.User = o.Name // pinned, as checkNode does for a certificate it registers
	}
	certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{Role: o.Role, Name: o.Name, User: certUser(rec), ID: rec.ID, Hosts: o.Hosts, Validity: o.Validity})
	if err != nil {
		return nil, err
	}
	leaf, err := pki.ParseCertPEM(certPEM)
	if err != nil {
		return nil, err
	}
	c := store.Cert{Serial: pki.Serial(leaf), Node: o.Name, NodeID: rec.ID, NotAfter: leaf.NotAfter, IssuedAt: time.Now(), HasID: true}
	if res.NewNode {
		rec.CertSerial, rec.CertNotAfter = c.Serial, c.NotAfter
		if err := st.CreateNodeWithCert(rec, c); err != nil {
			return nil, err
		}
	} else if err := st.AddCert(c); err != nil {
		return nil, err
	}
	res.CertPEM, res.KeyPEM, res.NodeID, res.Serial = certPEM, keyPEM, rec.ID, c.Serial
	return res, nil
}
