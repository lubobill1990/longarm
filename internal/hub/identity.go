package hub

// Node identity (docs/security.md "Node identity"; internal/nodeid): the
// hub keys its nodes by their immutable IDs. A name is a display name,
// unique among non-revoked nodes (rule 1: the database's unique index and
// nameTakenLocked) and never shaped like an ID (rule 2: checkNewName).
// Every API that takes a node accepts a target - a name, an ID, a unique
// ID prefix, id:<prefix> or name:<name> - and resolves it here.

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/nodeid"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// nodeLocked finds a node by ID, else by exact name: the non-revoked node
// of that name, else the one revoked node of that name (nil: none, or
// several revoked ones). For internal keys (IDs) and exact names; user
// input goes through resolveLocked.
func (h *Hub) nodeLocked(key string) *node {
	if key == "" {
		return nil
	}
	if n := h.nodes[key]; n != nil {
		return n
	}
	var revoked *node
	nrev := 0
	for _, n := range h.nodes {
		if n.rec.Name != key {
			continue
		}
		if n.rec.State != stateRevoked {
			return n
		}
		revoked, nrev = n, nrev+1
	}
	if nrev == 1 {
		return revoked
	}
	return nil
}

// activeByNameLocked is the non-revoked node named name (nil: none).
func (h *Hub) activeByNameLocked(name string) *node {
	for _, n := range h.nodes {
		if n.rec.Name == name && n.rec.State != stateRevoked {
			return n
		}
	}
	return nil
}

// nameTakenLocked reports whether name is in use by a non-revoked node
// other than except (an ID): as its name, or as the name in its current
// certificate (a renamed node that hasn't renewed yet still answers to
// the old name: TLS server name, longnet relay address).
func (h *Hub) nameTakenLocked(name, except string) *node {
	for id, n := range h.nodes {
		if id == except || n.rec.State == stateRevoked {
			continue
		}
		if n.rec.Name == name || n.rec.DialName() == name {
			return n
		}
	}
	return nil
}

// checkNewName is the rule for names the hub hands out or accepts:
// pki.ValidName, and nothing that looks like an ID (nodeid.CheckName).
func checkNewName(name string) error {
	if err := nodeid.CheckName(name, pki.ValidName); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

func candidate(n *node) nodeid.Candidate {
	owner := n.rec.Owner
	if n.rec.Role != string(pki.RoleWorker) {
		owner = nodeUser(n.rec)
	}
	return nodeid.Candidate{ID: n.rec.ID, Name: n.rec.Name, Owner: owner, State: n.rec.State}
}

// resolveLocked turns a target into a node among those visible passes
// (nil = all); what names them in errors ("worker", "node"). Errors are
// gRPC statuses: NotFound, or InvalidArgument for an ambiguous target
// (listing the candidates) or a malformed one.
func (h *Hub) resolveLocked(target string, visible func(*node) bool, what string) (*node, error) {
	t, err := nodeid.ParseTarget(target)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if t.Mode == nodeid.ByID { // the common case for new clients: no scan
		if n := h.nodes[t.Value]; n != nil && (visible == nil || visible(n)) {
			return n, nil
		}
		return nil, status.Errorf(codes.NotFound, "no %s with id %s", what, t.Value)
	}
	var cands []nodeid.Candidate
	for _, n := range h.nodes {
		if visible == nil || visible(n) {
			cands = append(cands, candidate(n))
		}
	}
	c, err := t.Resolve(cands, what)
	if err != nil {
		if errors.Is(err, nodeid.ErrAmbiguous) {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return h.nodes[c.ID], nil
}

// resolve is resolveLocked taking h.mu; it returns the node's ID.
func (h *Hub) resolve(target string, visible func(*node) bool, what string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, err := h.resolveLocked(target, visible, what)
	if err != nil {
		return "", err
	}
	return n.rec.ID, nil
}

// resolveWorker resolves a target among workers ("" stays "").
func (h *Hub) resolveWorker(target string) (string, error) {
	if target == "" {
		return "", nil
	}
	return h.resolve(target, isWorker, "worker")
}

func isWorker(n *node) bool { return n.rec.Role == string(pki.RoleWorker) }

// nodeForCertLocked is the node a certificate belongs to: by the node ID
// it carries; a certificate issued before node IDs by its recorded serial,
// else (issued by hand, never seen) by its name among non-revoked nodes.
func (h *Hub) nodeForCertLocked(id pki.Identity, cert *x509.Certificate) *node {
	if id.ID != "" {
		return h.nodes[id.ID]
	}
	if nid := h.knownSerials[pki.Serial(cert)]; nid != "" {
		if n := h.nodes[nid]; n != nil {
			return n
		}
	}
	return h.activeByNameLocked(id.Name)
}

// certName is the name to put in a new certificate of rec: its name, but
// for a worker that doesn't follow renames (an older longarmd: its longnet
// listener keeps the name it started with) the name its certificates had.
func certName(n *node, rec *store.Node) string {
	if n != nil && !n.followsRenames() {
		return rec.DialName()
	}
	return rec.Name
}

// newNodeRecord is a fresh node record with a new ID.
func newNodeRecord(name, role string) *store.Node {
	return &store.Node{ID: nodeid.New(), Name: name, Role: role}
}

// trustNodeKey returns the end-to-end trust node key of a CSR (base64;
// "" = none).
func trustNodeKey(csrPEM []byte) string {
	csr, err := pki.ParseCSR(csrPEM)
	if err != nil {
		return ""
	}
	t, err := pki.TrustOfCSR(csr)
	if err != nil || t == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(t.NodeKey)
}

// nodeKeyTakenLocked is the non-revoked node other than except whose
// certificate carries the node key k (nil: none).
func (h *Hub) nodeKeyTakenLocked(k, except string) *node {
	if k == "" {
		return nil
	}
	for id, n := range h.nodes {
		if id != except && n.rec.State != stateRevoked && n.rec.NodeKey == k {
			return n
		}
	}
	return nil
}

// forcedRenewBefore makes a renamed node renew its certificate at once
// (one that follows renames: clients, and workers with the "node-id"
// capability) when the certificate it connects with carries an old name: a
// renew_before longer than the certificate's remaining life (and shorter
// than a fresh certificate's, so the renewed node doesn't renew again).
// 0 = no need. Certificates without a node ID are not forced: they get one
// at their next regular renewal (until then the hub maps them by serial).
func forcedRenewBefore(n *node, cert *x509.Certificate) time.Duration {
	if cert == nil {
		return 0
	}
	id, err := pki.IdentityFromCert(cert)
	if err != nil || id.Name == n.rec.Name || !n.followsRenames() {
		return 0
	}
	left := time.Until(cert.NotAfter)
	if left <= 0 {
		return 0
	}
	return left + time.Hour
}

// followsRenames: a client or admin device (its certificate's name is
// only a label), or a worker that renews and restarts when its name
// changes (NodeHello capability "node-id"). Older workers keep the name of
// their certificate (issue: certName) - clients reach them by it
// (NodeInfo.cert_name).
func (n *node) followsRenames() bool {
	if n.rec.Role != string(pki.RoleWorker) {
		return true
	}
	for _, c := range n.rec.Capabilities {
		if c == "node-id" {
			return true
		}
	}
	return false
}

// RenameNode changes a node's display name (it keeps its ID): admin =
// any node; otherwise only a worker of user's own (owner == user). The
// new name must pass checkNewName and be free among non-revoked nodes.
// Audited.
func (h *Hub) RenameNode(by, user string, admin bool, target, newName string) (*pb.NodeInfo, error) {
	if err := checkNewName(newName); err != nil {
		return nil, err
	}
	h.certMu.Lock() // no renewal with the old name in between
	defer h.certMu.Unlock()
	h.mu.Lock()
	visible := func(n *node) bool { return true }
	what := "node"
	if !admin {
		visible = func(n *node) bool { return isWorker(n) && h.mayUseLocked(user, n) }
		what = "worker"
	}
	n, err := h.resolveLocked(target, visible, what)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	old := n.rec.Name
	fail := func(err error) (*pb.NodeInfo, error) {
		h.mu.Unlock()
		h.auditHub(by, "nodes/rename", map[string]any{"id": n.rec.ID, "old_name": old, "new_name": newName, "error": errString(err)})
		return nil, err
	}
	switch {
	case !admin && n.rec.Owner != user:
		return fail(status.Errorf(codes.PermissionDenied, "worker %s belongs to %s", old, ownerName(n.rec.Owner)))
	case n.rec.State == stateRevoked:
		return fail(status.Errorf(codes.FailedPrecondition, "node %s (%s) is revoked", old, nodeid.Short(n.rec.ID)))
	case old == newName:
		info := h.nodeInfoLocked(n)
		h.mu.Unlock()
		return info, nil
	}
	if o := h.nameTakenLocked(newName, n.rec.ID); o != nil {
		return fail(status.Errorf(codes.AlreadyExists, "the name %q is taken by %s %s (%s)", newName, o.rec.Role, o.rec.Name, nodeid.Short(o.rec.ID)))
	}
	if n.rec.CertName == "" {
		n.rec.CertName = old // what its certificate says until it renews
	}
	rec := *n.rec
	rec.Name = newName
	if rec.Role != string(pki.RoleWorker) && rec.User == "" {
		// a device whose certificate named no user acts as the user of
		// its name: keep that user when the name changes
		rec.User = old
	}
	if err := h.store.PutNode(&rec); err != nil {
		if errors.Is(err, store.ErrNameTaken) {
			return fail(status.Errorf(codes.AlreadyExists, "the name %q is taken", newName))
		}
		return fail(status.Error(codes.Internal, err.Error()))
	}
	n.rec = &rec
	h.renamedLocked(n, old)
	h.publishNodeLocked(n, fmt.Sprintf("renamed from %s by %s", old, by))
	info := h.nodeInfoLocked(n)
	h.mu.Unlock()
	h.auditHub(by, "nodes/rename", map[string]any{"id": rec.ID, "role": rec.Role, "old_name": old, "new_name": newName, "owner": rec.Owner})
	h.log.Info("node renamed", "id", rec.ID, "old", old, "new", newName, "by", by)
	h.event(context.Background(), "node.renamed", map[string]string{"role": rec.Role})
	return info, nil
}

// renamedLocked follows a rename: a worker that follows renames is asked
// to renew (its session restarts and the welcome's renew_before makes it
// renew with the new name; it restarts itself once renewed).
func (h *Hub) renamedLocked(n *node, old string) {
	if n.sess != nil && isWorker(n) && n.followsRenames() {
		h.log.Info("renamed worker reconnects to renew its certificate", "id", n.rec.ID, "old", old, "new", n.rec.Name)
		n.sess.cancel()
		h.setOfflineLocked(n, n.sess.id)
	}
}

// nodeNameLocked is the name of the node with ID id (id itself if unknown).
func (h *Hub) nodeNameLocked(id string) string {
	if n := h.nodes[id]; n != nil {
		return n.rec.Name
	}
	return id
}

// pairLocked links a new admin worker with the worker it was enrolled with
// (store.Node.Companion; the pairing of one machine): both point at each
// other.
func (h *Hub) pairLocked(n *node) {
	c := h.nodes[n.rec.Companion]
	if c == nil || c.rec.State == stateRevoked {
		n.rec.Companion = ""
		return
	}
	if c.rec.Companion != n.rec.ID {
		c.rec.Companion = n.rec.ID
		if err := h.store.PutNode(c.rec); err != nil {
			h.log.Warn("save companion link", "node", c.rec.Name, "err", err)
		}
		h.publishNodeLocked(c, "paired with admin worker "+n.rec.Name)
	}
}

// unpairLocked drops the companion link of a revoked node (on the other
// side; the revoked record keeps it as history).
func (h *Hub) unpairLocked(n *node) {
	c := h.nodes[n.rec.Companion]
	if c == nil || c.rec.Companion != n.rec.ID {
		return
	}
	c.rec.Companion = ""
	if err := h.store.PutNode(c.rec); err != nil {
		h.log.Warn("save companion link", "node", c.rec.Name, "err", err)
	}
	h.publishNodeLocked(c, "companion "+n.rec.Name+" revoked")
}
