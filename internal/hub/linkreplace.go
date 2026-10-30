package hub

import (
	"crypto/x509"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Signing in again on an enrolled machine (StartLinkRequest.replaces;
// docs/security.md "Signing in again"): the pairing carries the machine's
// current certificates, each with its key's signature over the pairing's
// new CSRs. At the approval the hub revokes those nodes - what their own
// `leave` would do, which whoever holds their keys can do anyway - and
// enrolls the new parts for the approver. A replaced worker's name goes to
// the new worker. Its sharing, profile and labels carry over only when the
// approver is its owner: the hub then knows it is the same person (the same
// Longarm user). Another user's node is removed, never moved: nothing of it
// becomes the approver's.

// linkReplace is one node a pairing replaces.
type linkReplace struct {
	name, role string
	worker     bool   // a worker (else a client or admin device)
	who        string // the worker's owner ("": a fleet worker) / the device's user
}

// maxReplaceCert bounds a replaced node's certificate (PEM).
const maxReplaceCert = 16 << 10

// linkReplaces checks a pairing's replacement proofs: at most one device
// (kind client or both) and one worker (kind worker or both), each a
// certificate of this hub's CA for an enrolled node - not an admin worker
// (its key is SYSTEM's; it signs in by itself) - signed over the pairing's
// CSRs by its key. An expired certificate still proves its key (signing in
// again after it expired), when it is the node's latest.
func (h *Hub) linkReplaces(req *pb.StartLinkRequest) ([]linkReplace, error) {
	if len(req.Replaces) == 0 {
		return nil, nil
	}
	if len(req.Replaces) > 2 {
		return nil, status.Error(codes.InvalidArgument, "replaces: at most a device and a worker")
	}
	msg := pki.ReplaceMessage(req.CsrPem, req.WorkerCsrPem, req.AdminWorkerCsrPem)
	wantWorker := req.Kind == linkWorker || req.Kind == linkBoth
	wantDevice := req.Kind == linkClient || req.Kind == linkBoth
	var out []linkReplace
	for _, r := range req.Replaces {
		if len(r.CertPem) > maxReplaceCert {
			return nil, status.Error(codes.InvalidArgument, "replaces: certificate too large")
		}
		c, err := pki.ParseCertPEM(r.CertPem)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "replaces: %v", err)
		}
		id, err := pki.IdentityFromCert(c)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "replaces: %v", err)
		}
		if err := h.verifyReplacedCert(c); err != nil {
			return nil, status.Errorf(codes.PermissionDenied, "replaces %s: not a certificate of this hub (%v)", id.Name, err)
		}
		if err := pki.VerifyReplace(c, msg, r.Signature); err != nil {
			return nil, status.Errorf(codes.PermissionDenied, "replaces %s: %v", id.Name, err)
		}
		h.mu.Lock()
		var rec store.Node
		n := h.nodes[id.Name]
		if n != nil {
			rec = *n.rec
		}
		revoked := h.revoked.Revoked(c)
		h.mu.Unlock()
		switch {
		case n == nil || rec.State != stateApproved || rec.Role != string(id.Role) || revoked:
			return nil, status.Errorf(codes.PermissionDenied, "replaces %s: no such enrolled node (removed, or its certificate revoked)", id.Name)
		case time.Now().After(c.NotAfter) && pki.Serial(c) != rec.CertSerial:
			return nil, status.Errorf(codes.PermissionDenied, "replaces %s: an expired certificate that is not its latest", id.Name)
		}
		lr := linkReplace{name: id.Name, role: rec.Role}
		switch pki.Role(rec.Role) {
		case pki.RoleWorker:
			if rec.Admin {
				return nil, status.Errorf(codes.InvalidArgument, "replaces %s: an admin worker signs in by itself (disable it and turn it on again)", id.Name)
			}
			if !wantWorker {
				return nil, status.Errorf(codes.InvalidArgument, "replaces %s: a worker, but the pairing enrolls no worker", id.Name)
			}
			lr.worker, lr.who = true, rec.Owner
		case pki.RoleClient, pki.RoleAdmin:
			if !wantDevice {
				return nil, status.Errorf(codes.InvalidArgument, "replaces %s: a device, but the pairing enrolls no device", id.Name)
			}
			lr.who = nodeUser(&rec)
		default:
			return nil, status.Errorf(codes.InvalidArgument, "replaces %s: a %s can't be replaced by a sign-in", id.Name, rec.Role)
		}
		for _, x := range out {
			if x.worker == lr.worker {
				return nil, status.Error(codes.InvalidArgument, "replaces: two nodes of one kind")
			}
		}
		out = append(out, lr)
	}
	return out, nil
}

// verifyReplacedCert: issued by this hub's CA (checked within the
// certificate's validity: an expired one still names its node).
func (h *Hub) verifyReplacedCert(c *x509.Certificate) error {
	pool := x509.NewCertPool()
	pool.AddCert(h.ca.Cert)
	at := time.Now()
	if at.After(c.NotAfter) {
		at = c.NotAfter.Add(-time.Second)
	}
	_, err := c.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err
}

// replaceLinkNodes revokes the nodes l replaces, at its approval by user
// (who: the sign-in, for the record). It returns the replaced worker's
// record when user owns it (its sharing carries over to the new worker);
// another user's worker or device is removed only. A node gone since the
// pairing started is skipped.
func (h *Hub) replaceLinkNodes(who, user string, l *pairLink) (*store.Node, error) {
	var carry *store.Node
	for _, r := range l.replaces {
		h.mu.Lock()
		var rec store.Node
		n := h.nodes[r.name]
		live := n != nil && n.rec.State == stateApproved && n.rec.Role == r.role
		if live {
			rec = *n.rec
		}
		h.mu.Unlock()
		if !live {
			continue
		}
		same := false
		if r.worker {
			same = rec.Owner != "" && rec.Owner == user
			if same {
				c := rec
				carry = &c
			}
		} else {
			same = nodeUser(&rec) == user
		}
		if _, err := h.Revoke(who+" (replaced by a sign-in on its machine)", r.name); err != nil {
			return nil, err
		}
		h.auditHub(who, "link/replace", map[string]any{"id": l.id, "node": r.name, "role": rec.Role, "same_user": same, "user": user})
		h.log.Info("pairing: node replaced", "node", r.name, "role", rec.Role, "same_user", same, "by", user)
	}
	return carry, nil
}

// linkReplaceView is a replaced node on the /link page: named only when
// it is the approver's (another user's machines are not shown to it).
type linkReplaceView struct {
	Kind, Name string
	Yours      bool
}

func replaceViews(rs []linkReplace, user string) []linkReplaceView {
	var out []linkReplaceView
	for _, r := range rs {
		v := linkReplaceView{Kind: "device", Yours: r.who != "" && r.who == user}
		if r.worker {
			v.Kind = "worker"
		}
		if v.Yours {
			v.Name = r.name
		}
		out = append(out, v)
	}
	return out
}
