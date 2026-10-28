// Package machine pairs the workers that run on one machine: a user-session
// worker (runs as its user) and its admin worker (runs as SYSTEM, label
// longarm.io/admin=true). They are two hub nodes, but one machine to the
// people looking at the list: the tray, longarm ls and the hub panel show
// them as one entry with an "admin commands" capability.
//
// The pairing: the hub records it at enrollment (store.Node.Companion, both
// ways) and reports it as NodeInfo.companion / NodeInfo.machine. Workers
// enrolled before that (or by an older hub) are paired by Match: an admin
// worker named <worker>-admin with the same owner and host name.
package machine

import (
	"strings"

	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// AdminSuffix is the default admin worker name's suffix (<worker>-admin).
const AdminSuffix = "-admin"

// Worker is what pairing looks at.
type Worker struct {
	Name, Owner, Hostname string
	Admin                 bool   // an admin worker (longarm.io/admin=true)
	Companion             string // the hub's explicit pairing ("" = none recorded)
	Revoked               bool
	Worker                bool // role worker (clients and admins never pair)
}

// Candidate is the name of the worker that may pair with w: its recorded
// companion, else (by name) <w>-admin for a session worker and the name
// without -admin for an admin worker. "" = none.
func Candidate(w Worker) string {
	if w.Companion != "" {
		return w.Companion
	}
	if w.Admin {
		if base, ok := strings.CutSuffix(w.Name, AdminSuffix); ok && base != "" {
			return base
		}
		return ""
	}
	return w.Name + AdminSuffix
}

// Match reports whether session and admin are one machine's session worker
// and admin worker: both live workers, exactly one an admin worker, the
// same owner (admin workers always have one) and - when both are known -
// the same host name (its short form, case-insensitive).
func Match(session, admin Worker) bool {
	if !session.Worker || !admin.Worker || session.Revoked || admin.Revoked {
		return false
	}
	if session.Admin || !admin.Admin || session.Name == admin.Name {
		return false
	}
	if admin.Owner == "" || session.Owner != admin.Owner {
		return false
	}
	if a, b := shortHost(session.Hostname), shortHost(admin.Hostname); a != "" && b != "" && a != b {
		return false
	}
	return true
}

func shortHost(h string) string {
	h, _, _ = strings.Cut(h, ".")
	return strings.ToLower(h)
}

// FromNode is the pairing view of a hub NodeInfo.
func FromNode(n *pb.NodeInfo) Worker {
	return Worker{Name: n.GetName(), Owner: n.GetOwner(), Hostname: n.GetHostname(),
		Admin: n.GetAdminWorker() || sched.IsAdminWorker(n.GetLabels()), Companion: n.GetCompanion(),
		Revoked: n.GetState() == "revoked", Worker: n.GetRole() == "" || n.GetRole() == "worker"}
}

// Pairs pairs nodes: admin worker name -> session worker name, for every
// admin worker whose session worker is among nodes. The hub's companion
// (NodeInfo.companion) wins; nodes from an older hub pair by Match.
func Pairs(nodes []*pb.NodeInfo) map[string]string {
	by := map[string]*pb.NodeInfo{}
	for _, n := range nodes {
		by[n.GetName()] = n
	}
	out := map[string]string{}
	taken := map[string]bool{}
	for _, n := range nodes {
		a := FromNode(n)
		if !a.Admin {
			continue
		}
		s := by[Candidate(a)]
		if s == nil || taken[s.GetName()] || !Match(FromNode(s), a) {
			continue
		}
		out[a.Name] = s.GetName()
		taken[s.GetName()] = true
	}
	return out
}
