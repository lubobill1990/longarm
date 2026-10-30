package hub

// Node identity (docs/security.md "Node identity"; internal/nodeid): the
// hub keys its nodes by their immutable IDs. A name is a display name,
// unique among non-revoked nodes (rule 1: the database's unique index and
// nameTakenLocked) and never shaped like an ID (rule 2: checkNewName).
// Every API that takes a node accepts a target - a name, an ID, a unique
// ID prefix, id:<prefix> or name:<name> - and resolves it here.

import (
	"cmp"
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
	"github.com/lubobill1990/longarm/internal/tenancy"
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

// resolveWorkerFor resolves a target among the workers v may see ("" stays
// "").
func (h *Hub) resolveWorkerFor(v tenancy.Caller, target string) (string, error) {
	if target == "" {
		return "", nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n, err := h.resolveLocked(target, h.workerVisibleLocked(v), "worker")
	if err != nil {
		return "", err
	}
	return n.rec.ID, nil
}

// visibleLocked is the resolution filter for user input from v (docs/
// security.md "What admins see"): users' machines v may see, and the nodes
// of nobody (relays, release credentials: operations on them are for
// admins anyway). Other users' machines are not candidates at all, so a
// target naming one - by name, ID or ID prefix - gets the answer of an
// unknown one, and an ambiguous prefix lists visible candidates only.
func (h *Hub) visibleLocked(v tenancy.Caller) func(*node) bool {
	return func(n *node) bool { return !userNodeRole(n.rec.Role) || h.visibleNodeLocked(v, n) }
}

// workerVisibleLocked is visibleLocked for worker targets.
func (h *Hub) workerVisibleLocked(v tenancy.Caller) func(*node) bool {
	return func(n *node) bool { return isWorker(n) && h.visibleNodeLocked(v, n) }
}

func isWorker(n *node) bool { return n.rec.Role == string(pki.RoleWorker) }

// nodeForCertLocked is the node a certificate belongs to: by the node ID
// it carries; a certificate issued before node IDs by its recorded serial
// (the certificates the hub issued or saw, mapped to their nodes by the
// migration). Never by name: an unknown certificate is checkNode's
// question (nil).
func (h *Hub) nodeForCertLocked(id pki.Identity, cert *x509.Certificate) *node {
	if id.ID != "" {
		return h.nodes[id.ID]
	}
	serial := pki.Serial(cert)
	nid := h.knownSerials[serial]
	if nid == "" { // recorded since the start (e.g. by another hub process)
		if s, err := h.store.CertNode(serial); err == nil && s != "" {
			nid = s
			h.knownSerials[serial] = s
		}
	}
	return h.nodes[nid]
}

// nameHistoryLocked says what history the name has ("" = none): a node
// (any state) has or had it as its name or its certificates' name, or a
// certificate was recorded for it.
func (h *Hub) nameHistoryLocked(name string) string {
	for _, n := range h.nodes {
		if n.rec.Name == name || n.rec.DialName() == name {
			return fmt.Sprintf("node %s, %s", nodeid.Short(n.rec.ID), n.rec.State)
		}
	}
	if has, err := h.store.NameHasCerts(name); err != nil || has {
		return "certificates were issued for it"
	}
	return ""
}

// reloadLegacyLocked reloads the serials of valid certificates without a
// node ID per node (NodeInfo.legacy_serials).
func (h *Hub) reloadLegacyLocked() {
	m, err := h.store.LegacyCerts(time.Now())
	if err != nil {
		h.log.Warn("legacy certificates", "err", err)
		return
	}
	h.legacySerials = m
}

// supersedeIfDue revokes a renamed node's older certificates once the
// node presents a certificate with its new name and its node ID
// (store.Node.RetirePending): no other certificate - the old name, which
// another node may get now - remains valid, and the names of its
// certificates without an ID get a legacy name revocation.
func (h *Hub) supersedeIfDue(nodeID string, cert *x509.Certificate) {
	adopted := func(n *node) bool {
		if n == nil || !n.rec.RetirePending {
			return false
		}
		id, err := pki.IdentityFromCert(cert)
		return err == nil && id.ID == n.rec.ID && id.Name == n.rec.Name
	}
	h.mu.Lock()
	due := adopted(h.nodes[nodeID])
	h.mu.Unlock()
	if !due {
		return
	}
	h.certMu.Lock()
	defer h.certMu.Unlock()
	h.mu.Lock()
	n := h.nodes[nodeID]
	if !adopted(n) {
		h.mu.Unlock()
		return
	}
	h.revokeLegacyNamesLocked(n)
	k, err := h.store.RevokeCertsExcept(nodeID, pki.Serial(cert), time.Now())
	if err != nil {
		h.mu.Unlock()
		h.log.Warn("revoke superseded certificates", "id", nodeID, "err", err)
		return
	}
	n.rec.RetirePending = false
	h.store.PutNode(n.rec)
	k++ // the legacy name revocations change the list too
	if k > 0 {
		h.revVersion++
		h.store.SetMeta("revocation_version", itoa(h.revVersion))
		if err := h.rebuildRevocations(); err != nil {
			h.log.Warn("rebuild revocations", "err", err)
		}
	}
	h.reloadLegacyLocked()
	h.mu.Unlock()
	if k > 0 {
		h.log.Info("renamed node's older certificates revoked", "id", nodeID, "name", n.rec.Name, "revoked", k)
		h.broadcast(h.revocationsMsg())
		h.conns.CloseIf(h.revoked.Revoked)
	}
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

// renameRenewal reports whether a renamed node that follows renames
// (clients, and workers with the "node-id" capability) should renew now:
// the certificate it connects with carries an old name. Certificates
// without a node ID are not forced: they get one at their next regular
// renewal (until then the hub maps them by serial).
func renameRenewal(n *node, cert *x509.Certificate) bool {
	if cert == nil || !n.followsRenames() {
		return false
	}
	id, err := pki.IdentityFromCert(cert)
	return err == nil && id.Name != n.rec.Name
}

// forcedRenewBefore is renameRenewal for a client's Whoami: a renew_before
// longer than the certificate's remaining life (a client asks once per run,
// and its renewed certificate has the new name: no loop). 0 = no need.
func forcedRenewBefore(n *node, cert *x509.Certificate) time.Duration {
	if !renameRenewal(n, cert) || n.rec.Role == string(pki.RoleWorker) {
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

// RenameNode changes a node's display name (it keeps its ID). The target
// resolves among the nodes v may see (visibleLocked: another user's
// machine is "not found", like an unknown one); then admin (HubAdmin, an
// admin panel login) may rename its own user's devices and workers and the
// fleet workers, others (Fleet.RenameWorker) their own workers only;
// without multi-tenancy (v.Admin) admins rename any user's machine. Relays
// and release credentials are never renamed (their certificate names are
// what nodes check). The new name must pass checkNewName, be free among
// non-revoked nodes (as names and certificate names) and not be a Longarm
// user's name unless the node is a device of that very user
// (nodeNameReservedLocked). Audited.
func (h *Hub) RenameNode(by string, v tenancy.Caller, admin bool, target, newName string) (*pb.NodeInfo, error) {
	if err := checkNewName(newName); err != nil {
		return nil, err
	}
	h.certMu.Lock() // no renewal with the old name in between
	defer h.certMu.Unlock()
	h.mu.Lock()
	visible := func(n *node) bool { return userNodeRole(n.rec.Role) && h.visibleNodeLocked(v, n) }
	what := "node"
	if !admin {
		visible = func(n *node) bool { return isWorker(n) && h.visibleNodeLocked(v, n) }
		what = "worker"
	}
	n, err := h.resolveLocked(target, visible, what)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	old := n.rec.Name
	user := n.rec.Owner // whose machine it is
	if !isWorker(n) {
		user = nodeUser(n.rec)
	}
	mayRename := v.Admin || // one user (no multi-tenancy): as before
		user == v.User && v.User != "" || // the caller's own machine
		admin && isWorker(n) && n.rec.Owner == "" // a fleet worker
	if !admin && !isWorker(n) {
		mayRename = false
	}
	if n.rec.Role != string(pki.RoleWorker) {
		// the user the device acts as must survive the new name: pinned
		// (a device whose user is its name and can't be written into a
		// certificate under another name can't be renamed)
		if u := cmp.Or(n.rec.User, old); !pki.ValidUser(u) {
			h.mu.Unlock()
			return nil, status.Errorf(codes.FailedPrecondition, "device %s acts as user %q, which can't be carried in a certificate under another name: enroll it again under the new name", old, u)
		}
	}
	fail := func(err error) (*pb.NodeInfo, error) {
		h.mu.Unlock()
		h.auditHub(by, "nodes/rename", renameAudit(n.rec, old, newName, err))
		return nil, err
	}
	switch {
	case !mayRename:
		return fail(status.Errorf(codes.PermissionDenied, "%s %s belongs to %s", what, old, ownerName(user)))
	case n.rec.State == stateRevoked:
		return fail(status.Errorf(codes.FailedPrecondition, "node %s (%s) is revoked", old, nodeid.Short(n.rec.ID)))
	case old == newName:
		info := h.nodeInfoLocked(n)
		h.mu.Unlock()
		return info, nil
	}
	if o := h.nameTakenLocked(newName, n.rec.ID); o != nil {
		if !userNodeRole(o.rec.Role) || h.visibleNodeLocked(v, o) {
			return fail(status.Errorf(codes.AlreadyExists, "the name %q is taken by %s %s (%s)", newName, o.rec.Role, o.rec.Name, nodeid.Short(o.rec.ID)))
		}
		// another user's machine: that the name is taken, nothing more
		return fail(status.Errorf(codes.AlreadyExists, "the name %q is taken", newName))
	}
	if h.nodeNameReservedLocked(newName, n.rec.Role, user) {
		return fail(errNameReserved(newName))
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
	h.auditHub(by, "nodes/rename", renameAudit(&rec, old, newName, nil))
	h.log.Info("node renamed", "id", rec.ID, "old", old, "new", newName, "by", by)
	h.event(context.Background(), "node.renamed", map[string]string{"role": rec.Role})
	return info, nil
}

// renameAudit is the audit record of a rename: owner (workers) or user
// (devices) attribute it to the machine's user (auditHub).
func renameAudit(r *store.Node, old, newName string, err error) map[string]any {
	m := map[string]any{"id": r.ID, "role": r.Role, "old_name": old, "new_name": newName}
	if r.Role == string(pki.RoleWorker) {
		m["owner"] = r.Owner
	} else {
		m["user"] = nodeUser(r)
	}
	if err != nil {
		m["error"] = err.Error()
	}
	return m
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
