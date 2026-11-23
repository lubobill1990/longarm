package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestPanelLogin(t *testing.T) {
	th, _ := ownedHub(t)
	h := th.h
	fleet := pb.NewFleetClient(th.alice)
	first, err := fleet.PanelLogin(tctx(t), &pb.PanelLoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	u, err := h.store.GetUser("alice")
	if err != nil || u.Admin() || u.Tenant != "alice" || !checkPassword(u.PassHash, first.Password) ||
		first.Login != "alice" || first.TotpSecret != u.TOTPSecret || first.Otpauth != TOTPURI("alice", u.TOTPSecret) {
		t.Fatalf("panel login: %v %v", u, err)
	}
	browser := panelLogin(t, h, "alice", first.Password, first.TotpSecret)
	bob, err := pb.NewFleetClient(th.bob).PanelLogin(tctx(t), &pb.PanelLoginRequest{})
	if err != nil || bob.Login != "bob" {
		t.Fatalf("certificate's user, not device name: %v %v", bob, err)
	}
	other := panelLogin(t, h, "bob", bob.Password, bob.TotpSecret)
	if _, err := fleet.PanelLogin(tctx(t), &pb.PanelLoginRequest{}); status.Code(err) != codes.AlreadyExists || !strings.Contains(err.Error(), "--reset") {
		t.Fatalf("existing login: %v", err)
	}
	next, err := fleet.PanelLogin(tctx(t), &pb.PanelLoginRequest{Reset_: true})
	if err != nil || next.Password == first.Password || next.TotpSecret == first.TotpSecret {
		t.Fatalf("reset: %v", err)
	}
	u, err = h.store.GetUser("alice")
	if err != nil || checkPassword(u.PassHash, first.Password) || !checkPassword(u.PassHash, next.Password) {
		t.Fatalf("password after reset: %v", err)
	}
	for _, tc := range []struct {
		browser *http.Client
		want    int
	}{{browser, http.StatusUnauthorized}, {other, http.StatusOK}} {
		resp, err := tc.browser.Get("http://" + h.Addr("http") + "/api/me")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("session after reset: %d, want %d", resp.StatusCode, tc.want)
		}
	}
	panelLogin(t, h, "alice", next.Password, next.TotpSecret)
	rows, err := h.store.Audit("", "hub", time.Time{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	var audits int
	for _, row := range rows {
		if row.Method == "users/panel-login" && row.SubjectUser == "alice" {
			audits++
			for _, secret := range []string{first.Password, first.TotpSecret, next.Password, next.TotpSecret} {
				if strings.Contains(row.Line, secret) {
					t.Fatal("panel credential in audit")
				}
			}
		}
	}
	if audits != 2 {
		t.Fatalf("panel-login audit records: %d", audits)
	}
}

func TestPanelLoginCallers(t *testing.T) {
	th := newTenancyHub(t)
	admin := th.dial(pki.IssueOptions{Role: pki.RoleAdmin, Name: "carol-admin", User: "carol"})
	if _, err := pb.NewFleetClient(admin).PanelLogin(tctx(t), &pb.PanelLoginRequest{}); err != nil {
		t.Fatal(err)
	}
	u, err := th.h.store.GetUser("carol")
	if err != nil || u.Role != "user" || u.Tenant != "carol" {
		t.Fatalf("admin certificate minted a non-user login: %v %v", u, err)
	}
	for _, role := range []pki.Role{pki.RoleWorker, pki.RoleRelay} {
		conn := th.dial(pki.IssueOptions{Role: role, Name: "node-" + string(role)})
		if _, err := pb.NewFleetClient(conn).PanelLogin(tctx(t), &pb.PanelLoginRequest{}); status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s panel login: %v", role, err)
		}
	}
	if _, err := pb.NewFleetClient(th.root).PanelLogin(tctx(t), &pb.PanelLoginRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("certificate without a Longarm user: %v", err)
	}
	if _, err := th.h.PutUser("test", &pb.PutUserRequest{Name: "bob", SetDisabled: true, Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := pb.NewFleetClient(th.bob).PanelLogin(tctx(t), &pb.PanelLoginRequest{Reset_: true}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("disabled user: %v", err)
	}
}

func TestPanelLoginIdentityProvider(t *testing.T) {
	for _, provider := range []string{"entra", "microsoft", "google", "apple"} {
		t.Run(provider, func(t *testing.T) {
			h := &Hub{tenants: map[string]*tenant{"alice": {u: &pb.TenantUser{Name: "alice"}}}}
			if provider == "entra" {
				h.entra = &entraState{}
			} else {
				h.idps = map[string]*idProvider{provider: {}}
			}
			ctx := context.WithValue(tctx(t), callerKey{}, caller{id: pki.Identity{Role: pki.RoleClient, Name: "alice"}, user: "alice"})
			for _, reset := range []bool{false, true} {
				_, err := (&fleetServer{h: h}).PanelLogin(ctx, &pb.PanelLoginRequest{Reset_: reset})
				if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "sign in with the provider instead") {
					t.Fatalf("identity provider: %v", err)
				}
			}
		})
	}
}

func TestPanelAdminNames(t *testing.T) {
	for _, kind := range []string{"tenant", "owner", "device"} {
		t.Run(kind, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			switch kind {
			case "tenant":
				err = st.PutTenant("alice", []byte(`{"name":"alice"}`))
			case "owner":
				err = st.PutNode(&store.Node{ID: "worker-id", Name: "worker-1", Role: "worker", Owner: "alice", State: stateRevoked})
			case "device":
				err = st.PutNode(&store.Node{ID: "device-id", Name: "alice", Role: "client", State: stateApproved})
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := AddPanelUser(st, "alice", "admin-password-1", "admin", "", false); err == nil || !strings.Contains(err.Error(), "longarm panel-login") {
				t.Fatalf("admin named like %s: %v", kind, err)
			}
			// A login made by an older hub cannot be reset into service.
			if err := st.PutUser(&store.User{Name: "alice", PassHash: "old"}); err != nil {
				t.Fatal(err)
			}
			if _, err := AddUser(st, "alice", "admin-password-2", true); err == nil {
				t.Fatal("legacy colliding admin reset")
			}
			u, err := st.GetUser("alice")
			if err != nil || u.PassHash != "old" {
				t.Fatalf("refused reset changed credentials: %v %v", u, err)
			}
		})
	}
	th := newTenancyHub(t)
	if _, err := AddUser(th.h.store, "admin1", "admin-password-1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := th.h.PutUser("test", &pb.PutUserRequest{Name: "admin1", Create: true}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("user named like a panel admin: %v", err)
	}
	if u, err := th.h.store.GetUser("admin1"); err != nil || !u.Admin() {
		t.Fatalf("admin login changed: %v %v", u, err)
	}
}

func TestLegacyPanelAdminIsNotUser(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	if err := h.store.PutUser(&store.User{Name: "alice"}); err != nil {
		t.Fatal(err)
	}
	tok := randomToken()
	if err := h.store.AddWebSession(sessionHash(tok), "alice", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "http://example.com/link", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	if _, err := h.linkApproverOf(req); err == nil || !strings.Contains(err.Error(), "not a Longarm user") {
		t.Fatalf("legacy admin acts as alice: %v", err)
	}
	v := h.panelViewer("alice")
	if v.User != "" || v.Admin || len(v.Teams) != 0 {
		t.Fatalf("legacy admin sees alice's view: %+v", v)
	}
	if _, err := pb.NewFleetClient(th.alice).PanelLogin(tctx(t), &pb.PanelLoginRequest{Reset_: true}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("user replaces a legacy admin login: %v", err)
	}
	if err := h.store.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if v := h.panelViewer("alice"); v.User != "" || v.Admin {
		t.Fatalf("vanished login acts as alice: %+v", v)
	}
}
