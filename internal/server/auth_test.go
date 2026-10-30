package server

import (
	"slices"
	"testing"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestReleaseRoleDeniedOnWorkers: the CI's release certificate gets no
// rights on longarmd (exec, jobs, files, sessions, metrics, admin ...); relay
// and hub certificates neither; worker certificates only LinkTunnel.Dial
// (port links, which needs a hub-signed grant: TestLinkTunnel) and
// Handoff.Fetch (a one-time offer of the owner: TestHandoffOfferScope).
func TestReleaseRoleDeniedOnWorkers(t *testing.T) {
	if len(MethodRoles) < 20 {
		t.Fatalf("table has %d methods", len(MethodRoles))
	}
	for m, roles := range MethodRoles {
		for _, r := range []pki.Role{pki.RoleRelease, pki.RoleRelay, pki.RoleHub, pki.RoleWorker} {
			if r == pki.RoleWorker && (m == pb.LinkTunnel_Dial_FullMethodName || m == pb.Handoff_Fetch_FullMethodName) {
				if !slices.Equal(roles, []pki.Role{pki.RoleWorker}) {
					t.Errorf("%s: %v, want workers only", m, roles)
				}
				continue
			}
			if slices.Contains(roles, r) {
				t.Errorf("%s may call %s", r, m)
			}
		}
	}
}
