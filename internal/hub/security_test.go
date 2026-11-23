package hub

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"longarm/internal/hub/store"
	"longarm/internal/pki"
	pb "longarm/proto/longarm/v1"
)

func TestSecurityConfig(t *testing.T) {
	c := &Config{}
	if err := c.applyDefaults(); err != nil || c.Security.Stage != 0 || c.Security.NewUsers != "e2e" || len(c.Security.Pilot) != 0 {
		t.Fatalf("defaults: %+v, %v", c.Security, err)
	}
	for _, cfg := range []SecurityConfig{
		{Stage: -1}, {Stage: 3}, {NewUsers: "unknown"}, {Pilot: []string{"not a user"}},
	} {
		if err := cfg.defaults(); err == nil {
			t.Errorf("accepted %+v", cfg)
		}
	}
	t.Setenv(EnvSecurityStage, "2")
	t.Setenv(EnvSecurityNewUsers, "shadow")
	t.Setenv(EnvSecurityPilot, "alice, bob")
	c, err := LoadConfig("")
	if err != nil || c.Security.Stage != 2 || c.Security.NewUsers != "shadow" || strings.Join(c.Security.Pilot, ",") != "alice,bob" {
		t.Fatalf("env: %+v, %v", c, err)
	}
	for _, v := range []string{"-1", "3", "1.5", "bad"} {
		t.Setenv(EnvSecurityStage, v)
		if _, err := LoadConfig(""); err == nil {
			t.Errorf("accepted stage %q", v)
		}
	}
}

func TestSecurityModeOrder(t *testing.T) {
	h := &Hub{cfg: &Config{Security: SecurityConfig{Stage: 1}}}
	for i, current := range []string{"", "legacy", "shadow", "e2e"} {
		for j, mode := range []string{"legacy", "shadow", "e2e"} {
			err := h.checkSecurityMode("alice", current, mode)
			want := codes.OK
			if j < i-1 {
				want = codes.FailedPrecondition
			}
			if status.Code(err) != want {
				t.Errorf("%q -> %q: %v, want %s", current, mode, err, want)
			}
			if want == codes.FailedPrecondition && (!strings.Contains(err.Error(), "user key") || !strings.Contains(err.Error(), "local access")) {
				t.Errorf("downgrade guidance: %v", err)
			}
		}
	}
	for _, mode := range []string{"", "unknown", "E2E"} {
		if err := h.checkSecurityMode("alice", "legacy", mode); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%q: %v", mode, err)
		}
	}
}

func TestSecurityModeRPC(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	fleet := pb.NewFleetClient(th.alice)
	admin := pb.NewHubAdminClient(th.root)
	put := func(mode string) error {
		_, err := admin.PutUser(tctx(t), &pb.PutUserRequest{Name: "alice", SetSecurityMode: true, SecurityMode: mode})
		return err
	}
	upgrade := func(mode string) error {
		_, err := fleet.SetSecurityMode(tctx(t), &pb.SetSecurityModeRequest{Mode: mode})
		return err
	}
	for _, set := range []func(string) error{put, upgrade} {
		if err := set("shadow"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("stage 0 upgrade: %v", err)
		}
	}
	h.mu.Lock()
	h.cfg.Security.Pilot = []string{"alice"}
	h.mu.Unlock()
	if err := put("shadow"); err != nil {
		t.Fatal(err)
	}
	if err := upgrade("e2e"); err != nil {
		t.Fatal(err)
	}
	for _, set := range []func(string) error{put, upgrade} {
		if err := set("shadow"); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("downgrade: %v", err)
		}
	}
	who, err := fleet.Whoami(tctx(t), &pb.WhoamiRequest{})
	if err != nil || who.SecurityMode != "e2e" || who.SecurityStage != 0 {
		t.Fatalf("whoami: %v, %v", who, err)
	}
	h.mu.Lock()
	h.cfg.Security.Stage = 1
	h.cfg.Security.Pilot = nil
	h.mu.Unlock()
	bob := pb.NewFleetClient(th.bob)
	who, err = bob.Whoami(tctx(t), &pb.WhoamiRequest{})
	if err != nil || who.SecurityMode != "legacy" || who.SecurityStage != 1 {
		t.Fatalf("legacy whoami: %v, %v", who, err)
	}
	if _, err := bob.SetSecurityMode(tctx(t), &pb.SetSecurityModeRequest{Mode: "shadow"}); err != nil {
		t.Fatal(err)
	}
	adminDevice := th.dial(pki.IssueOptions{Role: pki.RoleAdmin, Name: "bob-admin", User: "bob"})
	if _, err := pb.NewFleetClient(adminDevice).SetSecurityMode(tctx(t), &pb.SetSecurityModeRequest{Mode: "e2e"}); err != nil {
		t.Fatal(err)
	}
	worker := th.dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "worker-1"})
	if _, err := pb.NewFleetClient(worker).SetSecurityMode(tctx(t), &pb.SetSecurityModeRequest{Mode: "e2e"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("worker upgrade: %v", err)
	}
	if _, err := pb.NewFleetClient(th.root).SetSecurityMode(tctx(t), &pb.SetSecurityModeRequest{Mode: "e2e"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("no user: %v", err)
	}
	if _, err := admin.PutUser(tctx(t), &pb.PutUserRequest{Name: "bob", SetDisabled: true, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	// disabling ends bob's open connections: ask on a new one
	bob = pb.NewFleetClient(th.dial(pki.IssueOptions{Role: pki.RoleClient, Name: "bob-laptop", User: "bob"}))
	if _, err := bob.SetSecurityMode(tctx(t), &pb.SetSecurityModeRequest{Mode: "e2e"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("disabled: %v", err)
	}
	rows, err := h.store.Audit("", "hub", time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var audits int
	for _, row := range rows {
		if row.Method == "users/security-mode" {
			audits++
		}
	}
	if audits != 4 {
		t.Fatalf("security-mode audit records: %d", audits)
	}
}

func TestSecurityNewUsers(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	for i, cfg := range []SecurityConfig{
		{Stage: 0, NewUsers: "e2e"}, {Stage: 1, NewUsers: "e2e"},
		{Stage: 2, NewUsers: "legacy"}, {Stage: 2, NewUsers: "shadow"}, {Stage: 2, NewUsers: "e2e"},
	} {
		h.mu.Lock()
		h.cfg.Security = cfg
		h.mu.Unlock()
		want := "legacy"
		if cfg.Stage == 2 {
			want = cfg.NewUsers
		}
		r, err := h.PutUser("test", &pb.PutUserRequest{Name: fmt.Sprintf("user-%d", i), Create: true})
		if err != nil || r.User.SecurityMode != want {
			t.Fatalf("%+v: %v, %v", cfg, r, err)
		}
	}
	// A stage change or an unrelated edit must not upgrade existing users.
	r, err := h.PutUser("test", &pb.PutUserRequest{Name: "alice", SetCanShare: true})
	if err != nil || r.User.SecurityMode != "legacy" {
		t.Fatalf("existing user: %v, %v", r, err)
	}
}

func TestSecurityIdentityProviderNewUsers(t *testing.T) {
	h := newEntraHub(t, newFakeEntra(t), nil)
	h.mu.Lock()
	h.cfg.Security.Stage = 2
	h.cfg.Security.NewUsers = "shadow"
	h.mu.Unlock()
	user, _, err := h.entraSignIn(entraIdentity{oid: "aaaaaaaa-0000-4000-8000-00000000000a", upn: "alice@example.com"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	mode := h.tenants[user].u.SecurityMode
	h.cfg.Auth.Entra.apps = map[string]string{"app": "bob"}
	h.mu.Unlock()
	if mode != "shadow" {
		t.Fatalf("first sign-in mode: %s", mode)
	}
	user, err = h.entraAppUser("app", "bbbbbbbb-0000-4000-8000-00000000000b")
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	mode = h.tenants[user].u.SecurityMode
	h.mu.Unlock()
	if mode != "shadow" {
		t.Fatalf("app first sign-in mode: %s", mode)
	}
}

func TestSecurityModeStorage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.PutTenant("alice", []byte(`{"name":"alice"}`)); err != nil {
		t.Fatal(err)
	}
	h := &Hub{store: st, tenants: map[string]*tenant{}}
	if err := h.loadTenants(); err != nil {
		t.Fatal(err)
	}
	if mode := h.tenants["alice"].u.SecurityMode; mode != "" {
		t.Fatalf("old row changed: %q", mode)
	}
	for _, mode := range []string{"shadow", "e2e"} {
		if err := h.saveTenantLocked(&pb.TenantUser{Name: "alice", SecurityMode: mode}); err != nil {
			t.Fatal(err)
		}
		if err := h.loadTenants(); err != nil {
			t.Fatal(err)
		}
		if got := h.tenants["alice"].u.SecurityMode; got != mode {
			t.Fatalf("reloaded %q, want %q", got, mode)
		}
	}
}
