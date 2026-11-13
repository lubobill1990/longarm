package hub

// Traffic accounting: what the hub's own egress is made of. A hub in a
// cloud pays for every byte it sends, and two things send them: gRPC
// (the fleet API: job events, logs proxied for the panel, releases, ...)
// and the embedded relay (connections between nodes that found no direct
// path). trafficStats counts gRPC payload bytes by method and by calling
// node (a grpc stats.Handler, so streams count as they flow, not when
// they end); the relay keeps its own per-session counters, which
// trafficReport folds per node, adding the bytes of the sessions that
// already ended. GET /api/traffic (admins) serves the totals since the
// hub started; every trafficLogEvery the hub logs the top talkers of the
// period ("traffic: ..."), so `docker logs` answers "who is it" too.

import (
	"context"
	"crypto/x509"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"

	"longarm/internal/pki"
	"longarm/internal/relay"
	"longarm/internal/tenancy"
)

// trafficLogEvery is how often the hub logs the period's top talkers
// (variable for tests); trafficLogTop is how many rows each list logs.
var (
	trafficLogEvery = 10 * time.Minute
	trafficLogTop   = 8
)

// trafficCounter: payload bytes and messages each way, and streams.
type trafficCounter struct {
	in, out       atomic.Uint64 // bytes (wire length of the messages)
	msgsIn, msgsO atomic.Uint64
	calls         atomic.Uint64 // RPCs started
	active        atomic.Int64  // RPCs running
}

// trafficKey is one row: a method called by a node.
type trafficKey struct {
	method string
	node   string // the caller's identity ("worker/w1", "client/alice-laptop"; "" = no certificate)
	nodeID string // its node ID (certificates before node IDs: "")
}

// unknownMethod is the row of every path the server doesn't serve: they
// come before authentication (anyone may send any path), so they never
// get rows of their own.
const unknownMethod = "(unknown)"

// trafficStats is the hub's grpc stats.Handler.
type trafficStats struct {
	mu   sync.Mutex
	rows map[trafficKey]*trafficCounter
	// known: the methods the server serves (setKnown; nil = not set yet,
	// every path is unknown)
	known atomic.Pointer[map[string]bool]
}

// setKnown records the methods g serves (after their registration).
func (t *trafficStats) setKnown(g *grpc.Server) {
	m := map[string]bool{}
	for svc, info := range g.GetServiceInfo() {
		for _, md := range info.Methods {
			m["/"+svc+"/"+md.Name] = true
		}
	}
	t.known.Store(&m)
}

func newTrafficStats() *trafficStats {
	return &trafficStats{rows: map[trafficKey]*trafficCounter{}}
}

type trafficCtxKey struct{}

func (t *trafficStats) counter(k trafficKey) *trafficCounter {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.rows[k]
	if c == nil {
		c = &trafficCounter{}
		t.rows[k] = c
	}
	return c
}

// TagRPC finds the row of an RPC once: the peer (and its TLS state) is in
// the context the server hands to stats handlers.
// The rows are bounded: methods by the served set, nodes by the hub's
// certificates (a presented certificate is verified against the CA).
func (t *trafficStats) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	method := info.FullMethodName
	if k := t.known.Load(); k == nil || !(*k)[method] {
		method = unknownMethod
	}
	node, nodeID := peerNode(ctx)
	c := t.counter(trafficKey{method: method, node: node, nodeID: nodeID})
	return context.WithValue(ctx, trafficCtxKey{}, c)
}

func (t *trafficStats) HandleRPC(ctx context.Context, s stats.RPCStats) {
	c, _ := ctx.Value(trafficCtxKey{}).(*trafficCounter)
	if c == nil {
		return
	}
	switch s := s.(type) {
	case *stats.Begin:
		c.calls.Add(1)
		c.active.Add(1)
	case *stats.End:
		c.active.Add(-1)
	case *stats.InPayload:
		c.in.Add(uint64(s.WireLength))
		c.msgsIn.Add(1)
	case *stats.OutPayload:
		c.out.Add(uint64(s.WireLength))
		c.msgsO.Add(1)
	}
}

func (t *trafficStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (t *trafficStats) HandleConn(context.Context, stats.ConnStats) {}

// peerNode is the identity of the certificate the peer presented and its
// node ID ("" if none: enrollment, open methods).
func peerNode(ctx context.Context) (string, string) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", ""
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.PeerCertificates) == 0 {
		return "", ""
	}
	return certNode(ti.State.PeerCertificates[0])
}

func certNode(c *x509.Certificate) (string, string) {
	id, err := pki.IdentityFromCert(c)
	if err != nil {
		return "", ""
	}
	return id.String(), id.ID
}

// TrafficRow is one row of GET /api/traffic.
type TrafficRow struct {
	Method   string `json:"method,omitempty"`
	Node     string `json:"node,omitempty"`
	NodeID   string `json:"node_id,omitempty"` // which node the row is (names get reused)
	BytesIn  uint64 `json:"bytes_in"`          // to the hub
	BytesOut uint64 `json:"bytes_out"`         // from the hub: what egress costs
	MsgsIn   uint64 `json:"msgs_in,omitempty"`
	MsgsOut  uint64 `json:"msgs_out,omitempty"`
	Calls    uint64 `json:"calls,omitempty"`
	Active   int64  `json:"active,omitempty"`
	Sessions int    `json:"sessions,omitempty"` // relay: live sessions
}

// TrafficReport is GET /api/traffic: totals since the hub started.
type TrafficReport struct {
	Since time.Time `json:"since"`
	// GRPC is per method and calling node; ByMethod and ByNode fold it.
	GRPC     []TrafficRow `json:"grpc"`
	ByMethod []TrafficRow `json:"grpc_by_method"`
	ByNode   []TrafficRow `json:"grpc_by_node"`
	// Relay is per node: bytes_in = sent by the node into the relay,
	// bytes_out = relayed to it (live sessions plus ended ones).
	Relay      []TrafficRow `json:"relay,omitempty"`
	RelayTotal uint64       `json:"relay_bytes_relayed,omitempty"`
}

// snapshot returns the gRPC rows, largest egress first.
func (t *trafficStats) snapshot() []TrafficRow {
	t.mu.Lock()
	rows := make([]TrafficRow, 0, len(t.rows))
	for k, c := range t.rows {
		rows = append(rows, TrafficRow{Method: k.method, Node: k.node, NodeID: k.nodeID,
			BytesIn: c.in.Load(), BytesOut: c.out.Load(), MsgsIn: c.msgsIn.Load(), MsgsOut: c.msgsO.Load(),
			Calls: c.calls.Load(), Active: c.active.Load()})
	}
	t.mu.Unlock()
	sortTraffic(rows)
	return rows
}

// fold sums rows by key.
func fold(rows []TrafficRow, key func(TrafficRow) TrafficRow) []TrafficRow {
	m := map[TrafficRow]*TrafficRow{}
	var order []TrafficRow
	for _, r := range rows {
		k := key(r)
		a := m[k]
		if a == nil {
			a = &TrafficRow{Method: k.Method, Node: k.Node, NodeID: k.NodeID}
			m[k] = a
			order = append(order, k)
		}
		a.BytesIn += r.BytesIn
		a.BytesOut += r.BytesOut
		a.MsgsIn += r.MsgsIn
		a.MsgsOut += r.MsgsOut
		a.Calls += r.Calls
		a.Active += r.Active
		a.Sessions += r.Sessions
	}
	out := make([]TrafficRow, 0, len(order))
	for _, k := range order {
		out = append(out, *m[k])
	}
	sortTraffic(out)
	return out
}

func sortTraffic(rows []TrafficRow) {
	slices.SortStableFunc(rows, func(a, b TrafficRow) int {
		switch {
		case a.BytesOut != b.BytesOut:
			if a.BytesOut > b.BytesOut {
				return -1
			}
			return 1
		case a.BytesIn != b.BytesIn:
			if a.BytesIn > b.BytesIn {
				return -1
			}
			return 1
		}
		if a.Method+a.Node < b.Method+b.Node {
			return -1
		}
		return 1
	})
}

// relayEnded keeps the byte counts of relay sessions that ended, per
// node, so a node's relay total survives its reconnects.
type relayEnded struct {
	mu   sync.Mutex
	rows map[string]*TrafficRow
}

func (r *relayEnded) add(s relay.SessionStats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rows == nil {
		r.rows = map[string]*TrafficRow{}
	}
	k := s.NodeID + "\x00" + s.Node
	a := r.rows[k]
	if a == nil {
		a = &TrafficRow{Node: s.Node, NodeID: s.NodeID}
		r.rows[k] = a
	}
	a.BytesIn += s.BytesIn
	a.BytesOut += s.BytesOut
}

func (r *relayEnded) snapshot() []TrafficRow {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]TrafficRow, 0, len(r.rows))
	for _, a := range r.rows {
		out = append(out, *a)
	}
	return out
}

// trafficReport is the totals since the hub started.
func (h *Hub) trafficReport() TrafficReport {
	g := h.traffic.snapshot()
	rep := TrafficReport{Since: h.started, GRPC: g,
		ByMethod: fold(g, func(r TrafficRow) TrafficRow { return TrafficRow{Method: r.Method} }),
		ByNode:   fold(g, byNode)}
	if h.relay != nil {
		st := h.relay.Stats()
		rows := h.relayEnded.snapshot()
		for _, s := range st.Sessions {
			rows = append(rows, TrafficRow{Node: s.Node, NodeID: s.NodeID, BytesIn: s.BytesIn, BytesOut: s.BytesOut, Sessions: 1})
		}
		rep.Relay = fold(rows, byNode)
		rep.RelayTotal = st.BytesRelayed
	}
	return rep
}

// otherUsers is the node of every row about a machine the viewer may not
// see: like FleetSummary, the panel shows the rest of the fleet only as
// an anonymous total.
const otherUsers = "(other users)"

// trafficReportFor is trafficReport as v may see it.
func (h *Hub) trafficReportFor(v tenancy.Caller) TrafficReport {
	rep := h.trafficReport()
	if v.Admin {
		return rep
	}
	h.mu.Lock()
	seen := map[[2]string]bool{}
	// visible: the row's node by its ID (a name may have been another
	// node's: revoked, renamed), by name only for certificates without one
	visible := func(r TrafficRow) bool {
		key := [2]string{r.NodeID, r.Node}
		if ok, done := seen[key]; done {
			return ok
		}
		var n *node
		if r.NodeID != "" {
			n = h.nodes[r.NodeID]
		} else {
			_, name, _ := strings.Cut(r.Node, "/")
			n = h.nodeLocked(name)
		}
		ok := n != nil && h.visibleNodeLocked(v, n)
		seen[key] = ok
		return ok
	}
	cut := func(rows []TrafficRow, byMethod bool) []TrafficRow {
		for i := range rows {
			if rows[i].Node != "" && !visible(rows[i]) {
				rows[i].Node, rows[i].NodeID = otherUsers, ""
			}
		}
		if byMethod {
			return fold(rows, func(r TrafficRow) TrafficRow { return TrafficRow{Method: r.Method, Node: r.Node, NodeID: r.NodeID} })
		}
		return fold(rows, byNode)
	}
	rep.GRPC = cut(rep.GRPC, true)
	rep.ByNode = cut(rep.ByNode, false)
	rep.Relay = cut(rep.Relay, false)
	h.mu.Unlock()
	return rep
}

// apiTraffic serves GET /api/traffic (admins; other users' machines only
// as a total, trafficReportFor).
func (h *Hub) apiTraffic(w http.ResponseWriter, _ *http.Request, user string) {
	writeJSON(w, h.trafficReportFor(h.panelViewer(user)))
}

// runTrafficLog logs, every trafficLogEvery, the egress of the period by
// gRPC method+node and by relay node (rows that sent nothing are left out).
func (h *Hub) runTrafficLog(ctx context.Context) {
	tk := time.NewTicker(trafficLogEvery)
	defer tk.Stop()
	prev := TrafficReport{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		cur := h.trafficReport()
		h.logTrafficDelta("grpc", delta(cur.GRPC, prev.GRPC))
		h.logTrafficDelta("relay", delta(cur.Relay, prev.Relay))
		prev = cur
	}
}

// byNode is fold's key for per-node totals.
func byNode(r TrafficRow) TrafficRow { return TrafficRow{Node: r.Node, NodeID: r.NodeID} }

// delta is cur minus prev, row by row (same method and node), each counter
// at least 0: a relay node's sum can dip for a moment (a session that
// ends between the live and the ended snapshot counts twice once), and
// that must not log its whole total as one period's.
func delta(cur, prev []TrafficRow) []TrafficRow {
	type k struct{ m, n, id string }
	p := map[k]TrafficRow{}
	for _, r := range prev {
		p[k{r.Method, r.Node, r.NodeID}] = r
	}
	sub := func(a, b uint64) uint64 {
		if a < b {
			return 0
		}
		return a - b
	}
	out := make([]TrafficRow, 0, len(cur))
	for _, r := range cur {
		o := p[k{r.Method, r.Node, r.NodeID}]
		d := r
		d.BytesOut, d.BytesIn = sub(r.BytesOut, o.BytesOut), sub(r.BytesIn, o.BytesIn)
		d.MsgsOut, d.MsgsIn, d.Calls = sub(r.MsgsOut, o.MsgsOut), sub(r.MsgsIn, o.MsgsIn), sub(r.Calls, o.Calls)
		if d.BytesOut == 0 && d.BytesIn == 0 {
			continue
		}
		out = append(out, d)
	}
	sortTraffic(out)
	return out
}

func (h *Hub) logTrafficDelta(kind string, rows []TrafficRow) {
	var in, out uint64
	for _, r := range rows {
		in += r.BytesIn
		out += r.BytesOut
	}
	if in == 0 && out == 0 {
		return
	}
	h.log.Info("traffic: period", "kind", kind, "period", trafficLogEvery.String(), "bytes_out", out, "bytes_in", in, "rows", len(rows))
	for i, r := range rows {
		if i == trafficLogTop {
			break
		}
		args := []any{"kind", kind, "rank", i + 1, "bytes_out", r.BytesOut, "bytes_in", r.BytesIn, "node", r.Node}
		if r.Method != "" {
			args = append(args, "method", r.Method, "msgs_out", r.MsgsOut, "calls", r.Calls, "active", r.Active)
		}
		h.log.Info("traffic: top", args...)
	}
}
