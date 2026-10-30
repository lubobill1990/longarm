package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/relay"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The relay set (docs/usage.md "External relays"): the hub's embedded
// longnet relay restarts with the hub, so relayed connections drop with
// every hub redeploy. An external longarm-relay started with --hub (a
// separate deployment) subscribes here (HubNode.RelaySync): it registers its
// public endpoints and receives the revocation list and the worker pairs of
// the active port links, which it enforces on its own while the hub is
// away. The hub advertises every registered relay to workers (HubWelcome /
// HubCommand.relays, external first, the embedded one last); workers listen
// on each, clients race the resulting endpoints.
//
// Registered relays are kept in the database (meta "external_relays"), so a
// restarted hub advertises them at once, before they reconnect. A relay not
// connected for relayForget is dropped from the set; revoking its
// certificate name (longarm admin revoke) drops it at once.

// CapRelaySet is the NodeHello capability of workers that follow the relay
// set (HubWelcome.relays, HubCommand.relays).
const CapRelaySet = "relay-set"

// Relay set timing (variables for tests).
var (
	relayForget       = 7 * 24 * time.Hour
	relayPolicyEvery  = time.Minute
	relayPolicyTTL    = 24 * time.Hour
	relayMetaKey      = "external_relays"
	relaySeenPersist  = 10 * time.Minute // LastSeen is written at most this often while connected
	relayForgetTicker = time.Hour
)

// extRelay is a registered external relay.
type extRelay struct {
	Name       string    `json:"name"` // certificate name
	UDP        string    `json:"udp"`
	TCP        string    `json:"tcp,omitempty"`
	Version    string    `json:"version,omitempty"`
	Registered time.Time `json:"registered"`
	LastSeen   time.Time `json:"last_seen"`

	conns  int    // open RelaySync streams (not persisted)
	remote string // the last stream's peer address
	saved  time.Time
}

func (h *Hub) loadRelaysLocked() {
	h.extRelays = map[string]*extRelay{}
	v, err := h.store.Meta(relayMetaKey)
	if err != nil || v == "" {
		return
	}
	var list []*extRelay
	if err := json.Unmarshal([]byte(v), &list); err != nil {
		h.log.Warn("external relays: unreadable registry; starting empty", "err", err)
		return
	}
	for _, r := range list {
		if r.Name != "" && r.UDP != "" {
			r.saved = r.LastSeen
			h.extRelays[r.Name] = r
		}
	}
}

func (h *Hub) saveRelaysLocked() {
	list := make([]*extRelay, 0, len(h.extRelays))
	for _, r := range h.extRelays {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	b, _ := json.Marshal(list)
	if err := h.store.SetMeta(relayMetaKey, string(b)); err != nil {
		h.log.Warn("external relays: save registry", "err", err)
	}
}

// advertisedRelaysLocked are the registered relays still in the set, by
// preference: registration order (oldest first), then name.
func (h *Hub) advertisedRelaysLocked(now time.Time) []*extRelay {
	var out []*extRelay
	for _, r := range h.extRelays {
		if r.conns > 0 || now.Sub(r.LastSeen) < relayForget {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Registered.Equal(out[j].Registered) {
			return out[i].Registered.Before(out[j].Registered)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// relaySetLocked is the relay set of welcomes and RelaySet pushes.
func (h *Hub) relaySetLocked() []*pb.RelayEndpoint {
	var set []*pb.RelayEndpoint
	for _, r := range h.advertisedRelaysLocked(time.Now()) {
		set = append(set, &pb.RelayEndpoint{Udp: r.UDP, Tcp: r.TCP, Name: r.Name})
	}
	if a := h.relayAdvert(); a != "" {
		set = append(set, &pb.RelayEndpoint{Udp: a, Name: h.cfg.Name, Embedded: true})
	}
	return set
}

// relaySetChangedLocked pushes the set to the sessions of workers that
// follow it, when it differs from the last push.
func (h *Hub) relaySetChangedLocked() {
	set := h.relaySetLocked()
	msg := &pb.RelaySet{Relays: set}
	if h.lastRelaySet != nil && proto.Equal(h.lastRelaySet, msg) {
		return
	}
	h.lastRelaySet = msg
	h.log.Info("relay set changed", "relays", relaySetString(set))
	for _, n := range h.nodes {
		if n.sess != nil && slices.Contains(n.caps, CapRelaySet) {
			h.sendLocked(n, &pb.HubCommand{Msg: &pb.HubCommand_Relays{Relays: proto.Clone(msg).(*pb.RelaySet)}})
		}
	}
}

func relaySetString(set []*pb.RelayEndpoint) []string {
	var out []string
	for _, r := range set {
		s := r.Udp
		if r.Tcp != "" {
			s += " (tcp " + r.Tcp + ")"
		}
		if r.Embedded {
			s += " embedded"
		}
		out = append(out, s)
	}
	return out
}

// relayPolicyNotify wakes the RelaySync streams (pairs or revocations
// changed). Safe with or without h.mu.
func (h *Hub) relayPolicyNotify() {
	h.relayPolMu.Lock()
	if h.relayPolCh != nil {
		close(h.relayPolCh)
	}
	h.relayPolCh = make(chan struct{})
	h.relayPolMu.Unlock()
}

func (h *Hub) relayPolicyChanged() <-chan struct{} {
	h.relayPolMu.Lock()
	defer h.relayPolMu.Unlock()
	if h.relayPolCh == nil {
		h.relayPolCh = make(chan struct{})
	}
	return h.relayPolCh
}

// relayPolicyLocked is the policy external relays enforce.
func (h *Hub) relayPolicyLocked() *pb.RelayPolicy {
	p := &pb.RelayPolicy{Time: timestamppb.Now(), Ttl: durationpb.New(relayPolicyTTL),
		Revocations: h.revocationsMsgLocked().GetRevocations(),
		RenewBefore: durationpb.New(h.cfg.RenewBefore(string(pki.RoleRelay)))}
	seen := map[relay.WorkerPair]bool{}
	for _, k := range h.relayWorkerPairsLocked(time.Now()) {
		if k.B < k.A {
			k.A, k.B, k.AID, k.BID = k.B, k.A, k.BID, k.AID
		}
		if !seen[k] {
			seen[k] = true
			p.WorkerPairs = append(p.WorkerPairs, &pb.WorkerPair{A: k.A, B: k.B, AId: k.AID, BId: k.BID})
		}
	}
	sort.Slice(p.WorkerPairs, func(i, j int) bool {
		if p.WorkerPairs[i].A != p.WorkerPairs[j].A {
			return p.WorkerPairs[i].A < p.WorkerPairs[j].A
		}
		return p.WorkerPairs[i].B < p.WorkerPairs[j].B
	})
	return p
}

// validRelayAddr checks a registered "host:port" (host required).
func validRelayAddr(a string) error {
	host, port, err := net.SplitHostPort(a)
	if err != nil {
		return err
	}
	if host == "" || len(host) > 253 {
		return fmt.Errorf("%q: a host name or IP is required", a)
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("%q: bad port", a)
	}
	return nil
}

// RelaySync serves an external relay's subscription (see the proto).
func (s *hubNodeServer) RelaySync(req *pb.RelaySyncRequest, stream pb.HubNode_RelaySyncServer) error {
	h := s.h
	c := callerFrom(stream.Context())
	if err := validRelayAddr(req.Udp); err != nil {
		return status.Errorf(codes.InvalidArgument, "udp: %v", err)
	}
	if req.Tcp != "" {
		if err := validRelayAddr(req.Tcp); err != nil {
			return status.Errorf(codes.InvalidArgument, "tcp: %v", err)
		}
	}
	if len(req.Version) > 64 {
		return status.Error(codes.InvalidArgument, "version too long")
	}
	tcp := req.Tcp
	if tcp == req.Udp {
		tcp = ""
	}
	now := time.Now()
	h.mu.Lock()
	r := h.extRelays[c.id.Name]
	if r == nil {
		r = &extRelay{Name: c.id.Name, Registered: now.UTC()}
		h.extRelays[c.id.Name] = r
		h.log.Info("external relay registered", "relay", c.id.Name, "udp", req.Udp, "tcp", tcp, "version", req.Version, "remote", c.addr)
	} else if r.UDP != req.Udp || r.TCP != tcp {
		h.log.Info("external relay endpoints changed", "relay", c.id.Name, "udp", req.Udp, "tcp", tcp, "was_udp", r.UDP, "was_tcp", r.TCP)
	}
	r.UDP, r.TCP, r.Version, r.LastSeen, r.remote = req.Udp, tcp, req.Version, now.UTC(), c.addr
	r.conns++
	r.saved = now
	h.saveRelaysLocked()
	h.relaySetChangedLocked()
	h.mu.Unlock()
	h.event(context.Background(), "relay.connected", map[string]string{"relay_version": req.Version})
	defer func() {
		h.mu.Lock()
		if cur := h.extRelays[c.id.Name]; cur == r {
			r.conns--
			r.LastSeen = time.Now().UTC()
			h.saveRelaysLocked()
		}
		h.mu.Unlock()
		h.log.Info("external relay disconnected", "relay", c.id.Name)
	}()

	tick := time.NewTicker(relayPolicyEvery)
	defer tick.Stop()
	for {
		changed := h.relayPolicyChanged()
		h.mu.Lock()
		if h.extRelays[c.id.Name] != r { // revoked meanwhile
			h.mu.Unlock()
			return status.Error(codes.PermissionDenied, "relay removed")
		}
		p := h.relayPolicyLocked()
		if now := time.Now(); now.Sub(r.saved) >= relaySeenPersist {
			r.LastSeen, r.saved = now.UTC(), now
			h.saveRelaysLocked()
		}
		h.mu.Unlock()
		if err := stream.Send(p); err != nil {
			return err
		}
		select {
		case <-stream.Context().Done():
			return nil
		case <-changed:
		case <-tick.C:
		}
	}
}

// forgetRelaysLocked drops relays not seen for relayForget and the relay
// named name (revoked; "" = none); pushes the set when it changed.
func (h *Hub) forgetRelaysLocked(name string) {
	now := time.Now()
	changed := false
	for k, r := range h.extRelays {
		if k == name || (r.conns == 0 && now.Sub(r.LastSeen) >= relayForget) {
			h.log.Info("external relay removed from the relay set", "relay", k, "last_seen", r.LastSeen, "revoked", k == name)
			delete(h.extRelays, k)
			changed = true
		}
	}
	if changed {
		h.saveRelaysLocked()
	}
	h.relaySetChangedLocked()
}

// runRelayForget expires relays not seen for relayForget.
func (h *Hub) runRelayForget(ctx context.Context) {
	t := time.NewTicker(relayForgetTicker)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.mu.Lock()
			h.forgetRelaysLocked("")
			h.mu.Unlock()
		}
	}
}

// Relays is HubAdmin.Relays.
func (h *Hub) Relays() *pb.RelaysResponse {
	h.mu.Lock()
	defer h.mu.Unlock()
	resp := &pb.RelaysResponse{LegacyVersions: map[string]int32{}}
	for _, r := range h.advertisedRelaysLocked(time.Now()) {
		resp.Relays = append(resp.Relays, &pb.RelayStatus{
			Endpoint:  &pb.RelayEndpoint{Udp: r.UDP, Tcp: r.TCP, Name: r.Name},
			Connected: r.conns > 0, LastSeen: timestamppb.New(r.LastSeen), Version: r.Version, Remote: r.remote})
	}
	if a := h.relayAdvert(); a != "" {
		resp.Relays = append(resp.Relays, &pb.RelayStatus{Endpoint: &pb.RelayEndpoint{Udp: a, Name: h.cfg.Name, Embedded: true},
			Connected: true, Version: "hub"})
	}
	for _, n := range h.nodes {
		if n.sess == nil || n.rec.Role != string(pki.RoleWorker) {
			continue
		}
		if slices.Contains(n.caps, CapRelaySet) {
			resp.WorkersRelaySet++
		} else {
			resp.WorkersLegacy++
			v := n.rec.Version
			if v == "" {
				v = "unknown"
			}
			resp.LegacyVersions[v]++
		}
	}
	return resp
}

func (s *hubAdminServer) Relays(ctx context.Context, _ *pb.RelaysRequest) (*pb.RelaysResponse, error) {
	return s.h.Relays(), nil
}
