package server

import (
	"slices"
	"testing"

	"github.com/lubobill1990/longarm/internal/pki"
)

// TestReleaseRoleDeniedOnWorkers: the CI's release certificate gets no
// rights on longarmd (exec, jobs, files, sessions, metrics, admin ...); relay
// and hub certificates neither.
func TestReleaseRoleDeniedOnWorkers(t *testing.T) {
	if len(MethodRoles) < 20 {
		t.Fatalf("table has %d methods", len(MethodRoles))
	}
	for m, roles := range MethodRoles {
		for _, r := range []pki.Role{pki.RoleRelease, pki.RoleRelay, pki.RoleHub, pki.RoleWorker} {
			if slices.Contains(roles, r) {
				t.Errorf("%s may call %s", r, m)
			}
		}
	}
}
