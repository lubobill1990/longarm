package hubclient

import (
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// ApplyRevocations installs the hub's revocation list r into l: revoked
// certificate serials, revoked node IDs (any certificate carrying one is
// refused, whatever its serial: docs/security.md "Node identity") and
// legacy name revocations (certificates without a node ID of a revoked or
// renamed node). Workers and external relays (longarm-relay --hub) enforce
// the same list.
func ApplyRevocations(l *pki.RevocationList, r *pb.Revocations) error {
	serials := map[string]time.Time{}
	for _, c := range r.GetCerts() {
		serials[c.Serial] = c.NotAfter.AsTime()
	}
	ids := map[string]time.Time{}
	for _, n := range r.GetNodes() {
		if n.Id != "" {
			ids[n.Id] = n.NotAfter.AsTime()
		}
	}
	names := map[string]time.Time{}
	for _, n := range r.GetNames() {
		if n.Name != "" {
			names[pki.RevokedNameKey(pki.Role(n.Role), n.Name)] = n.NotAfter.AsTime()
		}
	}
	return l.ReplaceAllNames(r.GetVersion(), serials, ids, names)
}
