package hub

import (
	"context"
	"fmt"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"longarm/internal/pki"
	pb "longarm/proto/longarm/v1"
)

// SecurityConfig is [security]. Modes are recorded, not enforced in v1.
type SecurityConfig struct {
	Stage    int      `toml:"stage"`
	NewUsers string   `toml:"new_users"`
	Pilot    []string `toml:"pilot"`
}

func securityRank(mode string) int {
	switch mode {
	case "", "legacy":
		return 0
	case "shadow":
		return 1
	case "e2e":
		return 2
	default:
		return -1
	}
}

func (c *SecurityConfig) defaults() error {
	if c.Stage < 0 || c.Stage > 2 {
		return fmt.Errorf("security.stage: want 0..2")
	}
	if c.NewUsers == "" {
		c.NewUsers = "e2e"
	}
	if securityRank(c.NewUsers) < 0 {
		return fmt.Errorf("security.new_users: want legacy, shadow or e2e")
	}
	for _, user := range c.Pilot {
		if !pki.ValidUser(user) {
			return fmt.Errorf("security.pilot: invalid user name %q", user)
		}
	}
	return nil
}

func (h *Hub) newUserSecurityMode() string {
	if h.cfg.Security.Stage >= 2 {
		return h.cfg.Security.NewUsers
	}
	return "legacy"
}

func (h *Hub) checkSecurityMode(user, current, mode string) error {
	if mode == "" || securityRank(mode) < 0 {
		return status.Error(codes.InvalidArgument, "security mode must be legacy, shadow or e2e")
	}
	if securityRank(mode) < securityRank(current) {
		return status.Error(codes.FailedPrecondition, "security mode cannot be downgraded by the hub or admins: only the user (later, with the user key) or someone with local access to the workers can downgrade")
	}
	if h.cfg.Security.Stage < 1 && !slices.Contains(h.cfg.Security.Pilot, user) {
		return status.Error(codes.FailedPrecondition, "security upgrades are not offered at hub stage 0 (unless the user is in security.pilot)")
	}
	return nil
}

func (s *fleetServer) SetSecurityMode(ctx context.Context, req *pb.SetSecurityModeRequest) (*pb.TenantUser, error) {
	c, h := callerFrom(ctx), s.h
	h.mu.Lock()
	t := h.tenants[c.user]
	if c.user == "" || t == nil || (c.id.Role != pki.RoleClient && c.id.Role != pki.RoleAdmin) {
		h.mu.Unlock()
		return nil, status.Error(codes.PermissionDenied, "security upgrade needs a client or admin certificate with a Longarm user")
	}
	if h.userDisabledLocked(c.user) {
		h.mu.Unlock()
		return nil, status.Errorf(codes.PermissionDenied, "user %s is disabled", c.user)
	}
	if err := h.checkSecurityMode(c.user, t.u.SecurityMode, req.GetMode()); err != nil {
		h.mu.Unlock()
		return nil, err
	}
	u := proto.Clone(t.u).(*pb.TenantUser)
	previous := u.SecurityMode
	u.SecurityMode = req.GetMode()
	if err := h.saveTenantLocked(u); err != nil {
		h.mu.Unlock()
		return nil, status.Error(codes.Internal, err.Error())
	}
	t.u = u
	resp := h.userInfoLocked(t)
	h.mu.Unlock()
	h.auditHub(c.id.String(), "users/security-mode", map[string]any{"user": u.Name, "from": previous, "to": u.SecurityMode})
	return resp, nil
}
