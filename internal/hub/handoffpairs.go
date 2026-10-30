package hub

// Handoff pairs: a direct handoff transfer (longarm handoff, worker ->
// worker) has the target worker dial the source with its worker
// certificate. Without a direct path that goes through a relay, and relays
// carry worker <-> worker traffic only for the pairs the hub lists
// (relay.Server.SetWorkerPairs, RelayPolicy.worker_pairs): the pairs of the
// active port links - and, for a few minutes, the pair of a handoff. The
// client asks for it before the transfer and releases it afterwards; the
// hub lists it for the caller's own workers only (never another user's,
// admins included) and drops it when it expires.

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Limits of handoff pairs (variables for tests).
var (
	handoffPairMaxTTL  = 10 * time.Minute
	handoffPairPerUser = 16
)

// handoffPairKey is one user's pair of workers (names ordered).
type handoffPairKey struct{ a, b, owner string }

// HandoffPair lists (or, with release, drops) the caller's temporary pair.
func (h *Hub) HandoffPair(c caller, req *pb.HandoffPairRequest) (*pb.HandoffPairResponse, error) {
	if req.A == "" || req.B == "" || req.A == req.B {
		return nil, status.Error(codes.InvalidArgument, "a handoff pair needs two different workers")
	}
	ttl := handoffPairMaxTTL
	if req.Ttl != nil {
		ttl = req.Ttl.AsDuration()
	}
	if !req.Release && (ttl <= 0 || ttl > handoffPairMaxTTL) {
		return nil, status.Errorf(codes.InvalidArgument, "ttl %s: at most %s", ttl, handoffPairMaxTTL)
	}
	owner := c.id.Owner()
	k := handoffPairKey{req.A, req.B, owner}
	if k.b < k.a {
		k.a, k.b = k.b, k.a
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, name := range []string{k.a, k.b} {
		if !h.handoffWorkerLocked(owner, name) {
			// word for word the unknown-name answer: another user's worker
			// stays hidden
			return nil, status.Errorf(codes.NotFound, "no worker %q", name)
		}
	}
	fields := map[string]any{"a": k.a, "b": k.b, "owner": owner}
	if req.Release {
		if _, ok := h.handoffPairs[k]; ok {
			delete(h.handoffPairs, k)
			h.updateRelayPairsLocked()
		}
		fields["release"] = true
		h.auditHub(c.id.String(), "handoff/pair", fields)
		return &pb.HandoffPairResponse{}, nil
	}
	if _, ok := h.handoffPairs[k]; !ok {
		mine := 0
		for o := range h.handoffPairs {
			if o.owner == owner {
				mine++
			}
		}
		if mine >= handoffPairPerUser {
			return nil, status.Errorf(codes.ResourceExhausted, "quota: %d handoff pairs (max %d)", mine, handoffPairPerUser)
		}
	}
	if h.handoffPairs == nil {
		h.handoffPairs = map[handoffPairKey]time.Time{}
	}
	exp := time.Now().Add(ttl)
	_, had := h.handoffPairs[k]
	h.handoffPairs[k] = exp
	fields["expires"] = exp.UTC().Format(time.RFC3339)
	h.auditHub(c.id.String(), "handoff/pair", fields)
	if !had {
		h.updateRelayPairsLocked()
	}
	return &pb.HandoffPairResponse{ExpiresAt: timestamppb.New(exp)}, nil
}

// handoffWorkerLocked: user may hand off to and from the worker - an
// approved worker that is user's own, or an ownerless (fleet) worker user
// may use. A worker of another user never, even when shared with user, and
// for admins neither.
func (h *Hub) handoffWorkerLocked(user, name string) bool {
	n := h.nodes[name]
	if user == "" || n == nil || n.rec.Role != string(pki.RoleWorker) || n.rec.State != stateApproved {
		return false
	}
	if n.rec.Owner != "" {
		return n.rec.Owner == user
	}
	return h.mayUseLocked(user, n)
}

// handoffPairsLocked are the unexpired handoff pairs.
func (h *Hub) handoffPairsLocked(now time.Time) [][2]string {
	var pairs [][2]string
	for k, exp := range h.handoffPairs {
		if exp.After(now) {
			pairs = append(pairs, [2]string{k.a, k.b})
		}
	}
	return pairs
}

// sweepHandoffPairsLocked drops expired pairs, from the relays' lists too.
func (h *Hub) sweepHandoffPairsLocked(now time.Time) {
	changed := false
	for k, exp := range h.handoffPairs {
		if !exp.After(now) {
			delete(h.handoffPairs, k)
			changed = true
		}
	}
	if changed {
		h.updateRelayPairsLocked()
	}
}

func (s *fleetServer) HandoffPair(ctx context.Context, req *pb.HandoffPairRequest) (*pb.HandoffPairResponse, error) {
	return s.h.HandoffPair(callerFrom(ctx), req)
}
