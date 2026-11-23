package hub

import (
	"bytes"
	"context"
	"crypto/x509"
	"slices"
	"sort"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"longarm/internal/hub/store"
	"longarm/internal/pki"
	pb "longarm/proto/longarm/v1"
)

const maxTrustPairings = 20

func pairingPB(p *store.TrustPairing) *pb.TrustPairing {
	return &pb.TrustPairing{Id: p.ID, Kind: p.Kind, User: p.User, UserKey: slices.Clone(p.UserKey),
		UserTag: slices.Clone(p.UserTag), ExpiresAt: tspb(p.ExpiresAt), NodeKey: slices.Clone(p.NodeKey),
		NodeProof: slices.Clone(p.NodeProof), NodeId: p.NodeID, NodeName: p.NodeName, Hostname: p.Hostname,
		Os: p.OS, Endorsement: p.Endorsement, State: p.State}
}

func pairingOpen(p *store.TrustPairing, now time.Time) bool {
	return (p.State == "joined" || p.State == "offered") && p.ExpiresAt.After(now)
}

func (h *Hub) auditPairing(by, step string, p *store.TrustPairing) {
	h.auditHub(by, "trust/pairing-"+step, map[string]any{
		"id": p.ID, "kind": p.Kind, "user": p.User, "node_id": p.NodeID, "state": p.State})
}

// checkPairingSlotLocked includes unused codes, which reserve a slot until join.
func (h *Hub) checkPairingSlotLocked(user, id, replaceNode string, now time.Time) error {
	count := 0
	for _, n := range h.nodes {
		p := n.rec.Pairing
		if p == nil || n.rec.State == stateRevoked {
			continue
		}
		if p.ID == id && n.rec.ID != replaceNode {
			return status.Error(codes.AlreadyExists, "pairing id is already in use")
		}
		if n.rec.ID != replaceNode && p.User == user && pairingOpen(p, now) {
			count++
		}
	}
	all, err := h.store.JoinCodes(now)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	for _, c := range all {
		if p := c.Pairing; p != nil && c.UsedAt.IsZero() && c.ExpiresAt.After(now) && p.ExpiresAt.After(now) {
			if p.ID == id {
				return status.Error(codes.AlreadyExists, "pairing id is already in use")
			}
			if p.User == user {
				count++
			}
		}
	}
	if count >= maxTrustPairings {
		return status.Error(codes.ResourceExhausted, "too many open trust pairings")
	}
	return nil
}

func (h *Hub) pairingCaller(c caller) error {
	if c.user == "" || (c.id.Role != pki.RoleClient && c.id.Role != pki.RoleAdmin) {
		return status.Error(codes.PermissionDenied, "trust pairing needs a client or admin certificate with a Longarm user")
	}
	h.mu.Lock()
	active := h.tenants[c.user] != nil && !h.userDisabledLocked(c.user)
	h.mu.Unlock()
	if !active {
		return status.Error(codes.PermissionDenied, "trust pairing needs an active user")
	}
	if !h.trustPairLimit.allow(c.user) {
		return status.Error(codes.ResourceExhausted, "too many pairing requests; slow down")
	}
	return nil
}

func (s *fleetServer) PublishUserKey(ctx context.Context, req *pb.PublishUserKeyRequest) (*pb.TenantUser, error) {
	h, c := s.h, callerFrom(ctx)
	if err := h.pairingCaller(c); err != nil {
		return nil, err
	}
	if len(req.UserKey) != 32 {
		return nil, status.Error(codes.InvalidArgument, "user key must be 32 bytes")
	}
	h.mu.Lock()
	t := h.tenants[c.user]
	if t == nil || h.userDisabledLocked(c.user) {
		h.mu.Unlock()
		return nil, status.Error(codes.PermissionDenied, "trust pairing needs an active user")
	}
	u := proto.Clone(t.u).(*pb.TenantUser)
	u.UserKey = slices.Clone(req.UserKey)
	if err := h.saveTenantLocked(u); err != nil {
		h.mu.Unlock()
		return nil, status.Error(codes.Internal, err.Error())
	}
	t.u = u
	out := h.userInfoLocked(t)
	h.mu.Unlock()
	h.auditHub(c.id.String(), "trust/pairing-publish-key", map[string]any{"user": c.user})
	return out, nil
}

func (s *fleetServer) ListTrustPairings(ctx context.Context, req *pb.ListTrustPairingsRequest) (*pb.ListTrustPairingsResponse, error) {
	h, c := s.h, callerFrom(ctx)
	if err := h.pairingCaller(c); err != nil {
		return nil, err
	}
	if len(req.Ids) > 100 {
		return nil, status.Error(codes.InvalidArgument, "too many pairing ids")
	}
	for _, id := range req.Ids {
		if !pki.ValidPairID(id) {
			return nil, status.Error(codes.InvalidArgument, "invalid pairing id")
		}
	}
	out := &pb.ListTrustPairingsResponse{}
	h.mu.Lock()
	for _, n := range h.nodes {
		p := n.rec.Pairing
		if p == nil || p.User != c.user || n.rec.State == stateRevoked {
			continue
		}
		if len(req.Ids) == 0 && !pairingOpen(p, time.Now()) || len(req.Ids) != 0 && !slices.Contains(req.Ids, p.ID) {
			continue
		}
		out.Pairings = append(out.Pairings, pairingPB(p))
	}
	h.mu.Unlock()
	sort.Slice(out.Pairings, func(i, j int) bool { return out.Pairings[i].Id < out.Pairings[j].Id })
	h.auditHub(c.id.String(), "trust/pairing-list", map[string]any{"user": c.user, "count": len(out.Pairings)})
	return out, nil
}

func (s *fleetServer) OfferDevicePairing(ctx context.Context, req *pb.TrustPairing) (*pb.TrustPairing, error) {
	h, c := s.h, callerFrom(ctx)
	if err := h.pairingCaller(c); err != nil {
		return nil, err
	}
	if !pki.ValidPairID(req.Id) || req.Kind != pki.KindDevice || len(req.NodeProof) != 32 {
		return nil, status.Error(codes.InvalidArgument, "invalid device pairing")
	}
	trust, err := pki.TrustOf(c.cert)
	if err != nil || trust == nil {
		return nil, status.Error(codes.FailedPrecondition, "device certificate needs a trust node key")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.callerNodeLocked(c)
	if n == nil || n.rec.State == stateRevoked {
		return nil, status.Error(codes.FailedPrecondition, "device is not registered")
	}
	now := time.Now().UTC()
	// A repeated offer must not erase a completed exchange before the device fetches it.
	if p := n.rec.Pairing; p != nil && p.ID == req.Id {
		if !bytes.Equal(p.NodeKey, trust.NodeKey) || !bytes.Equal(p.NodeProof, req.NodeProof) {
			return nil, status.Error(codes.InvalidArgument, "pairing id has different node fields")
		}
		if p.ExpiresAt.After(now) || p.State == "endorsed" || p.State == "installed" {
			return pairingPB(p), nil
		}
	}
	if err := h.checkPairingSlotLocked(c.user, req.Id, n.rec.ID, now); err != nil {
		return nil, err
	}
	p := &store.TrustPairing{ID: req.Id, Kind: pki.KindDevice, User: c.user,
		NodeKey: slices.Clone(trust.NodeKey), NodeProof: slices.Clone(req.NodeProof),
		NodeID: n.rec.ID, NodeName: c.id.Name, Hostname: n.rec.Hostname, OS: n.rec.OS,
		ExpiresAt: now.Add(time.Hour), State: "offered"}
	rec := *n.rec
	rec.Pairing = p
	if err := h.store.PutNode(&rec); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	n.rec = &rec
	h.auditPairing(c.id.String(), "offered", p)
	return pairingPB(p), nil
}

func (s *fleetServer) CompleteTrustPairing(ctx context.Context, req *pb.CompleteTrustPairingRequest) (*pb.TrustPairing, error) {
	h, c := s.h, callerFrom(ctx)
	if err := h.pairingCaller(c); err != nil {
		return nil, err
	}
	if !pki.ValidPairID(req.Id) || len(req.Endorsement) > 8192 {
		return nil, status.Error(codes.InvalidArgument, "invalid pairing completion")
	}
	e, err := pki.ParseEndorsement(req.Endorsement)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid endorsement")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, n := range h.nodes {
		p := n.rec.Pairing
		if p == nil || p.ID != req.Id || p.User != c.user || n.rec.State == stateRevoked {
			continue
		}
		if !bytes.Equal(e.NodeKey, p.NodeKey) || e.ID != p.NodeID || e.Kind != p.Kind || e.User != c.user || e.Name != p.NodeName {
			return nil, status.Error(codes.InvalidArgument, "endorsement does not match pairing")
		}
		if p.Kind == pki.KindDevice {
			if len(req.UserKey) != 32 || len(req.UserTag) != 32 || !bytes.Equal(e.UserKey, req.UserKey) {
				return nil, status.Error(codes.InvalidArgument, "invalid device user key or tag")
			}
		} else if !bytes.Equal(e.UserKey, p.UserKey) {
			return nil, status.Error(codes.InvalidArgument, "endorsement has a different user key")
		}
		if p.State == "endorsed" || p.State == "installed" {
			if e.String() != p.Endorsement || p.Kind == pki.KindDevice &&
				(!bytes.Equal(req.UserKey, p.UserKey) || !bytes.Equal(req.UserTag, p.UserTag)) {
				return nil, status.Error(codes.FailedPrecondition, "pairing is already completed")
			}
			return pairingPB(p), nil
		}
		if !pairingOpen(p, time.Now()) {
			return nil, status.Error(codes.FailedPrecondition, "pairing has expired")
		}
		paired := *p
		paired.Endorsement, paired.State = e.String(), "endorsed"
		if p.Kind == pki.KindDevice {
			paired.UserKey, paired.UserTag = slices.Clone(req.UserKey), slices.Clone(req.UserTag)
		}
		rec := *n.rec
		rec.Pairing = &paired
		if err := h.store.PutNode(&rec); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		n.rec = &rec
		if paired.Kind == pki.KindWorker {
			h.sendLocked(n, trustEndorsementCmd(&paired))
		}
		h.auditPairing(c.id.String(), "endorsed", &paired)
		return pairingPB(&paired), nil
	}
	return nil, status.Error(codes.NotFound, "no such trust pairing")
}

func trustEndorsementCmd(p *store.TrustPairing) *pb.HubCommand {
	return &pb.HubCommand{Msg: &pb.HubCommand_TrustEndorsement{TrustEndorsement: &pb.TrustEndorsement{
		PairingId: p.ID, Endorsement: p.Endorsement}}}
}

func (h *Hub) pairingConnectLocked(n *node, cert *x509.Certificate) *pb.HubCommand {
	p := n.rec.Pairing
	if p == nil || p.Kind != pki.KindWorker || p.State != "endorsed" {
		return nil
	}
	trust, err := pki.TrustOf(cert)
	if err == nil && trust != nil && bytes.Equal(trust.NodeKey, p.NodeKey) && trust.Endorsement != nil &&
		trust.Endorsement.ID == p.NodeID && trust.Endorsement.Kind == p.Kind && trust.Endorsement.User == p.User &&
		bytes.Equal(trust.Endorsement.UserKey, p.UserKey) && trust.Endorsement.Verify() == nil {
		paired := *p
		paired.State = "installed"
		rec := *n.rec
		rec.Pairing = &paired
		if err := h.store.PutNode(&rec); err != nil {
			h.log.Warn("save installed trust pairing", "id", p.ID, "err", err)
			return trustEndorsementCmd(p)
		}
		n.rec = &rec
		h.auditPairing("node/"+n.rec.Name, "installed", &paired)
		return nil
	}
	return trustEndorsementCmd(p)
}

func (h *Hub) sweepTrustPairings(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, n := range h.nodes {
		p := n.rec.Pairing
		if p == nil || p.ExpiresAt.After(now) || (p.State != "joined" && p.State != "offered") {
			continue
		}
		rec := *n.rec
		rec.Pairing = nil
		if err := h.store.PutNode(&rec); err != nil {
			h.log.Warn("expire trust pairing", "id", p.ID, "err", err)
			continue
		}
		n.rec = &rec
		h.auditPairing("hub", "expired", p)
	}
}
