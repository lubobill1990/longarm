package hub

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The hub's tailnet (DESIGN §4.2, §4.4): with [tailnet] provider =
// "tailscale" or "headscale" the hub mints a one-time, pre-authorized,
// tagged key for every node it enrolls and on request (Fleet.TailnetKey), so
// the embedded tsnet node of longarmd / longarm joins without anybody touching the
// control plane. Workers get tag:longarm-worker and are never ephemeral (the hub
// advertises their tailnet IP); clients and admins get tag:longarm-client and
// are ephemeral by default.
//
// Nodes learn the hub's tailnet (provider + control URL) from HubWelcome
// (workers) and Whoami (clients). When it differs from theirs - the hub
// switched providers - they fetch a key for the new control plane and run
// their tsnet node from a state dir of that control URL, so switching back
// needs no key while the old state is still valid.

// mintRequest is what a provider needs to mint a key.
type mintRequest struct {
	tags      []string
	ephemeral bool
	ttl       time.Duration
	node      string // for the key's description
}

// tailnetProvider mints keys on one control plane.
type tailnetProvider interface {
	Name() string // tailscale | headscale
	Mint(ctx context.Context, req mintRequest) (string, error)
}

func newTailnetProvider(cfg TailnetConfig) (tailnetProvider, error) {
	switch cfg.Provider {
	case ProviderTailscale:
		return newTailscale(cfg.Tailscale)
	case ProviderHeadscale:
		c, err := newHeadscale(cfg.Headscale)
		if err != nil || c == nil {
			return nil, err
		}
		return c, nil
	}
	return nil, nil
}

// TailnetHostname is the tsnet hostname of a node.
func TailnetHostname(role, name string) string {
	if role == string(pki.RoleWorker) {
		return "longarm-" + name
	}
	return "longarm-client-" + name
}

// tailnetInfo is the hub's tailnet as told to node n (h.mu held or n
// unshared).
func (h *Hub) tailnetInfo(n *node) *pb.TailnetInfo {
	ti := &pb.TailnetInfo{}
	if h.tailnet != nil {
		ti.Provider, ti.ControlUrl = h.tailnet.Name(), h.cfg.Tailnet.ControlURL()
		ti.Rekey = n != nil && n.rec.TailnetRekey
	}
	return ti
}

// tailnetAuth mints a key for a node (nil, nil without a tailnet provider)
// and records it on the node.
func (h *Hub) tailnetAuth(ctx context.Context, role, name, reason string) (*pb.TailnetAuth, error) {
	if h.tailnet == nil {
		return nil, nil
	}
	t := h.cfg.Tailnet
	tags, eph := t.ClientTags, *t.EphemeralClients
	if role == string(pki.RoleWorker) {
		tags, eph = t.WorkerTags, false
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	key, err := h.tailnet.Mint(ctx, mintRequest{tags: tags, ephemeral: eph, ttl: t.KeyTTL.Duration, node: name})
	if err != nil {
		h.log.Warn("tailnet key", "provider", h.tailnet.Name(), "node", name, "reason", reason, "err", RedactKeys(err.Error()))
		if ctx.Err() == nil || errors.Is(context.Cause(ctx), context.DeadlineExceeded) { // not a caller that went away
			h.tailnetMu.Lock()
			if h.tailnetMintErr == nil {
				h.tailnetMintErr = map[string]mintFailure{}
			}
			h.tailnetMintErr[role] = mintFailure{err: RedactKeys(err.Error()), node: name, at: time.Now().UTC()}
			h.tailnetMu.Unlock()
		}
		return nil, err
	}
	control := t.ControlURL()
	if reason == "" {
		reason = "requested"
	}
	h.log.Info("tailnet key minted", "provider", h.tailnet.Name(), "node", name, "role", role, "tags", tags,
		"ephemeral", eph, "reason", reason)
	h.tailnetMu.Lock()
	delete(h.tailnetMintErr, role) // that role's keys work (again)
	h.tailnetMu.Unlock()
	h.mu.Lock()
	if n := h.nodes[name]; n != nil {
		n.rec.TailnetKeyAt, n.rec.TailnetKeyControl, n.rec.TailnetKeyReason = time.Now().UTC(), control, reason
		n.rec.TailnetRekey = false
		h.store.PutNode(n.rec)
	}
	h.mu.Unlock()
	return &pb.TailnetAuth{ControlUrl: control, AuthKey: key, Hostname: TailnetHostname(role, name),
		Tags: tags, Ephemeral: eph, Provider: h.tailnet.Name()}, nil
}

// attachTailnet adds a tailnet key to an approved join response; a
// provider failure only costs the tailnet leg (logged, noted in message).
func (h *Hub) attachTailnet(ctx context.Context, resp *pb.JoinResponse) {
	if h.tailnet == nil || resp.State != stateApproved || len(resp.CertPem) == 0 {
		return
	}
	ttl := h.cfg.Tailnet.KeyTTL.Duration
	// A pending worker polls JoinStatus until it collects its certificate;
	// every poll after the approval returns the same key (one per
	// enrollment, cached for half its validity).
	key := resp.Name + "\x00" + string(resp.CertPem)
	h.tailnetMu.Lock()
	if c, ok := h.tailnetIssued[key]; ok && time.Since(c.at) < ttl/2 {
		h.tailnetMu.Unlock()
		resp.Tailnet = c.auth
		return
	}
	h.tailnetMu.Unlock()
	ta, err := h.tailnetAuth(ctx, resp.Role, resp.Name, "enrol")
	if err != nil {
		resp.Message = strings.TrimSpace(resp.Message + " (no tailnet key: " + RedactKeys(err.Error()) + ")")
		return
	}
	h.tailnetMu.Lock()
	if h.tailnetIssued == nil {
		h.tailnetIssued = map[string]issuedKey{}
	}
	for k, c := range h.tailnetIssued { // drop stale entries
		if time.Since(c.at) > ttl {
			delete(h.tailnetIssued, k)
		}
	}
	h.tailnetIssued[key] = issuedKey{auth: ta, at: time.Now()}
	h.tailnetMu.Unlock()
	resp.Tailnet = ta
}

func (s *fleetServer) TailnetKey(ctx context.Context, req *pb.TailnetKeyRequest) (*pb.TailnetAuth, error) {
	c := callerFrom(ctx)
	if s.h.tailnet == nil {
		return nil, errNoTailnet
	}
	// Workers are never ephemeral: every key can become a device that stays
	// until an admin deletes it, so their budget is tight. Clients' nodes
	// are ephemeral by default and re-key whenever theirs was removed.
	// a provider that can't mint yet (a Headscale sidecar still starting)
	// costs the node no rate limit: it retries
	if r, ok := s.h.tailnet.(interface{ Ready() error }); ok {
		if err := r.Ready(); err != nil {
			return nil, status.Errorf(codes.Unavailable, "%s", err)
		}
	}
	lim := s.h.tailnetClientLimit
	if c.id.Role == pki.RoleWorker {
		lim = s.h.tailnetWorkerLimit
	}
	if !lim.allow(c.id.Name) {
		s.h.log.Warn("tailnet key refused: rate limit", "node", c.id.Name, "role", c.id.Role)
		return nil, status.Error(codes.ResourceExhausted, "too many tailnet keys for this node; try again later")
	}
	reason := cleanReason(req.Reason)
	ta, err := s.h.tailnetAuth(ctx, string(c.id.Role), c.id.Name, reason)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "%s", RedactKeys(err.Error()))
	}
	return ta, nil
}

// cleanReason keeps a node-supplied reason printable: ASCII letters,
// digits, space and -_., at most 40 characters.
func cleanReason(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 40 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(" -_.", r) {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

var errNoTailnet = status.Error(codes.FailedPrecondition, "this hub has no tailnet provider ([tailnet] provider)")

// mintFailure is the last key the hub failed to mint (for the status).
type mintFailure struct {
	err  string
	node string
	at   time.Time
}

// issuedKey is a minted tailnet key, reused for repeated polls of one
// enrollment.
type issuedKey struct {
	auth *pb.TailnetAuth
	at   time.Time
}

// applyTailnetLocked records a worker's reported tailnet state.
func (h *Hub) applyTailnetLocked(n *node, st *pb.TailnetState) {
	n.tailnet = st
	n.rec.Tailnet = nil
	if st != nil {
		if b, err := protojson.Marshal(st); err == nil {
			n.rec.Tailnet = b
		}
	}
}

func (h *Hub) tailnetNodeStatusLocked(n *node) *pb.TailnetNodeStatus {
	r := n.rec
	ns := &pb.TailnetNodeStatus{Name: r.Name, Role: r.Role, Online: n.sess != nil, State: n.tailnet,
		KeyControlUrl: r.TailnetKeyControl, KeyReason: r.TailnetKeyReason, RekeyPending: r.TailnetRekey}
	if !r.TailnetKeyAt.IsZero() {
		ns.KeyMintedAt = tspb(r.TailnetKeyAt)
	}
	return ns
}

// TailnetStatus implements HubAdmin.TailnetStatus.
func (a *hubAdminServer) TailnetStatus(ctx context.Context, _ *pb.TailnetStatusRequest) (*pb.TailnetStatusResponse, error) {
	h := a.h
	t := h.cfg.Tailnet
	resp := &pb.TailnetStatusResponse{Provider: t.Provider, ControlUrl: t.ControlURL(), WorkerTags: t.WorkerTags,
		ClientTags: t.ClientTags, EphemeralClients: *t.EphemeralClients, KeyTtl: durationpb.New(t.KeyTTL.Duration)}
	if h.tailnet == nil {
		resp.Provider = ProviderNone
	}
	if t.Provider == ProviderTailscale {
		resp.Tailnet = t.Tailscale.Tailnet
	}
	if st, detail := h.headscaleHealth(); st != "" {
		resp.ControlHealth = st
		if detail != "" {
			resp.ControlHealth += ": " + detail
		}
	}
	h.tailnetMu.Lock()
	var last mintFailure // the most recent of the roles' failures
	for _, f := range h.tailnetMintErr {
		if f.at.After(last.at) {
			last = f
		}
	}
	h.tailnetMu.Unlock()
	if last.err != "" {
		resp.LastMintError, resp.LastMintErrorAt, resp.LastMintErrorNode = last.err, tspb(last.at), last.node
	}
	v := h.viewer(callerFrom(ctx))
	h.mu.Lock()
	for _, n := range h.nodes {
		if n.rec.State != stateApproved || !h.visibleNodeLocked(v, n) { // other users' machines: not listed
			continue
		}
		resp.Nodes = append(resp.Nodes, h.tailnetNodeStatusLocked(n))
	}
	if resp.LastMintErrorNode != "" && !h.visibleNameLocked(v, resp.LastMintErrorNode) {
		resp.LastMintErrorNode = hiddenNode
	}
	resp.LastMintError = h.projector(v).text(resp.LastMintError)
	h.mu.Unlock()
	sort.Slice(resp.Nodes, func(i, j int) bool {
		a, b := resp.Nodes[i], resp.Nodes[j]
		if (a.Role == "worker") != (b.Role == "worker") {
			return a.Role == "worker"
		}
		return a.Name < b.Name
	})
	return resp, nil
}

// TailnetRekey implements HubAdmin.TailnetRekey.
func (a *hubAdminServer) TailnetRekey(ctx context.Context, req *pb.NodeRequest) (*pb.TailnetNodeStatus, error) {
	h := a.h
	if h.tailnet == nil {
		return nil, errNoTailnet
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.nodes[req.Name]
	if n == nil || n.rec.State != stateApproved {
		return nil, status.Errorf(codes.NotFound, "no approved node %q", req.Name)
	}
	n.rec.TailnetRekey = true
	if err := h.store.PutNode(n.rec); err != nil {
		return nil, status.Errorf(codes.Internal, "save node: %v", err)
	}
	h.sendLocked(n, &pb.HubCommand{Msg: &pb.HubCommand_Tailnet{Tailnet: h.tailnetInfo(n)}})
	h.log.Info("tailnet re-key requested", "node", n.rec.Name, "by", callerFrom(ctx).id.String(), "online", n.sess != nil)
	ns := h.tailnetNodeStatusLocked(n)
	if !h.visibleNodeLocked(h.viewerLocked(callerFrom(ctx).user, false), n) { // by name: no more than the admin knew
		ns = &pb.TailnetNodeStatus{Name: ns.Name, Role: ns.Role, Online: ns.Online, RekeyPending: ns.RekeyPending}
	}
	return ns, nil
}

// callerTailnet is the hub's tailnet for a node (Whoami).
func (h *Hub) callerTailnet(name string) *pb.TailnetInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tailnetInfo(h.nodes[name])
}
