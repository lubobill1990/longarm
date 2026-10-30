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
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Limits of handoff pairs (variables for tests).
var (
	handoffPairMaxTTL  = 10 * time.Minute
	handoffPairPerUser = 16
)

// handoffPairKey is one user's pair of workers (ordered by node ID):
// their IDs and certificate names.
type handoffPairKey struct{ aID, bID, a, b, owner string }

// HandoffPair lists (or, with release, drops) the caller's temporary pair.
// a and b are the workers' certificate names; a_id / b_id (newer clients:
// from the certificates they verified) hold them to node IDs. Each side
// must be a worker the caller may hand off with (handoffWorkerLocked);
// anything else - unknown, another user's, a name that is not that ID's -
// is "no worker", word for word the same.
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
	owner := c.user
	h.mu.Lock()
	defer h.mu.Unlock()
	var ends [2]*node
	for i, side := range [2][2]string{{req.A, req.AId}, {req.B, req.BId}} {
		n := h.handoffWorkerLocked(owner, side[0], side[1])
		if n == nil {
			// word for word the unknown-name answer: another user's worker
			// stays hidden
			return nil, status.Errorf(codes.NotFound, "no worker %q", side[0])
		}
		ends[i] = n
	}
	if ends[0] == ends[1] {
		return nil, status.Error(codes.InvalidArgument, "a handoff pair needs two different workers")
	}
	k := handoffPairKey{ends[0].rec.ID, ends[1].rec.ID, ends[0].rec.DialName(), ends[1].rec.DialName(), owner}
	if k.bID < k.aID {
		k.aID, k.bID, k.a, k.b = k.bID, k.aID, k.b, k.a
	}
	fields := map[string]any{"a": k.a, "b": k.b, "a_id": k.aID, "b_id": k.bID, "owner": owner}
	if req.Release {
		for o := range h.handoffPairs { // the same workers, whatever their names now
			if o.aID == k.aID && o.bID == k.bID && o.owner == owner {
				delete(h.handoffPairs, o)
				h.updateRelayPairsLocked()
			}
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

// handoffWorkerLocked is the worker user may hand off to and from (nil:
// none) - an approved worker that is user's own, or an ownerless (fleet)
// worker user may use; a worker of another user never, even when shared
// with user, and for admins neither. It is found by its node ID (id, when
// given: name must then be the name in its certificate), else by the name
// in its certificate (an older client) among the non-revoked workers.
func (h *Hub) handoffWorkerLocked(user, name, id string) *node {
	var n *node
	if id != "" {
		n = h.nodes[strings.ToLower(id)]
		if n != nil && n.rec.DialName() != name {
			return nil
		}
	} else {
		for _, o := range h.nodes {
			if o.rec.State != stateRevoked && o.rec.DialName() == name {
				n = o
				break
			}
		}
	}
	if user == "" || n == nil || n.rec.Role != string(pki.RoleWorker) || n.rec.State != stateApproved {
		return nil
	}
	if n.rec.Owner != "" {
		if n.rec.Owner != user {
			return nil
		}
		return n
	}
	if !h.mayUseLocked(user, n) {
		return nil
	}
	return n
}

// handoffPairsLocked are the unexpired handoff pairs.
func (h *Hub) handoffPairsLocked(now time.Time) []relay.WorkerPair {
	var pairs []relay.WorkerPair
	for k, exp := range h.handoffPairs {
		if exp.After(now) {
			pairs = append(pairs, relay.WorkerPair{A: k.a, B: k.b, AID: k.aID, BID: k.bID})
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
