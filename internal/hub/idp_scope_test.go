package hub

import (
	"crypto/ed25519"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestIdPAccountsScope: accounts of every provider (Google, personal
// Microsoft, Apple) link a client device and a worker each at /link. Every
// account is one user - the same e-mail address at two providers is two
// users - and every account, admins included, sees and uses only its own
// machines: by its device certificate, its panel session and the admin
// summary (counts only). Worker certificates act as nobody, and the audit
// never attributes a record by e-mail address.
func TestIdPAccountsScope(t *testing.T) {
	g, ms, ap := newFakeIdP(t), newFakeIdP(t), newFakeIdP(t)
	keyFile, _ := appleKeyFile(t)
	const appleAdmin = "000111.aaaa.0001"
	h := newIdPHub(t, func(a *AuthConfig) {
		a.Google = g.config(googleClient)
		a.Google.Admins = []string{"root@example.com"}
		a.Microsoft = ms.config(msaClient)
		a.Microsoft.Admins = []string{"ops@example.org"}
		a.Apple = ap.config(appleClient)
		a.Apple.ClientSecretFile, a.Apple.TeamID, a.Apple.KeyID, a.Apple.PrivateKeyFile = "", "ABCDE12345", "KEY1234567", keyFile
		a.Apple.Admins = []string{"sub:" + appleAdmin}
	})
	ec := enrollClient(t, h)
	msIss := "https://login.microsoftonline.com/" + msaTenant + "/v2.0"

	type account struct {
		provider, sub, email string
		admin                bool
		user                 string // the Longarm user it becomes
		pc, wk               string // its machines
		web                  *web
		conn                 *grpc.ClientConn // its device's certificate
		worker               pki.Identity
	}
	accts := []*account{
		{provider: idpGoogle, sub: "g-root", email: "root@example.com", admin: true, user: "root"},
		{provider: idpGoogle, sub: "g-alice", email: "alice@example.com", user: "alice"},
		{provider: idpMicrosoft, sub: "m-alice", email: "alice@example.com", user: "alice2"}, // same address, another user
		{provider: idpMicrosoft, sub: "m-ops", email: "ops@example.org", admin: true, user: "ops"},
		{provider: idpApple, sub: appleAdmin, admin: true, user: "apple"}, // no e-mail address
	}
	for i, a := range accts {
		a.pc, a.wk = "pc"+string(rune('a'+i)), "wk"+string(rune('a'+i))
		a.web = newWeb(t, h)
		q := a.web.idpStart(a.provider, "/link")
		f, claims := g, map[string]any(nil)
		switch a.provider {
		case idpGoogle:
			claims = idClaimsFor("https://accounts.google.com", googleClient, a.sub, q.Get("nonce"), map[string]any{"email": a.email, "email_verified": true})
		case idpMicrosoft:
			f, claims = ms, idClaimsFor(msIss, msaClient, a.sub, q.Get("nonce"), map[string]any{"tid": msaTenant, "preferred_username": a.email})
		case idpApple:
			f, claims = ap, idClaimsFor("https://appleid.apple.com", appleClient, a.sub, q.Get("nonce"), nil)
		}
		f.mu.Lock()
		f.codes["c-"+a.sub] = claims
		f.mu.Unlock()
		if code, body := a.web.idpCallback(a.provider, q, "c-"+a.sub); code != 200 || !strings.Contains(body, "Signed in as "+a.user+".") {
			t.Fatalf("%s sign-in of %s: %d %s", a.provider, a.sub, code, body)
		}
		nk, wk := make([]byte, 32), make([]byte, 32)
		nk[0], wk[0] = byte(2*i+1), byte(2*i+2)
		d, wkeyPEM, err := startBoth(t, ec, a.pc, a.wk, ed25519.NewKeyFromSeed(nk), ed25519.NewKeyFromSeed(wk))
		if err != nil {
			t.Fatal(err)
		}
		_, confirm := a.web.lookup(d.st.UserCode)
		if c, body := a.web.decide(confirm, "approve"); c != 200 {
			t.Fatalf("%s approves: %d %s", a.user, c, body)
		}
		r := poll(t, h, ec, d)
		id, _ := identityOf(t, r)
		role := pki.RoleClient
		if a.admin {
			role = pki.RoleAdmin
		}
		if r.State != linkApproved || r.Name != a.pc || id.Role != role || id.User != a.user || r.Worker == nil || r.Worker.Name != a.wk || r.Worker.Owner != a.user {
			t.Fatalf("%s's machines: %v %v", a.user, r, id)
		}
		a.worker, _ = identityOf(t, r.Worker)
		if wa := connectWorker(t, h, r.Worker, wkeyPEM); wa.Owner != a.user {
			t.Fatalf("%s's worker access: %v", a.user, wa)
		}
		b := writeTestBundle(t, h, &hubclient.JoinResult{Resp: r, KeyPEM: d.keyPEM})
		if a.conn, err = grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b)))); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.conn.Close() })
	}

	// everything a reply may carry about others' machines
	others := func(a *account, s string) []string {
		var bad []string
		for _, o := range accts {
			if o == a {
				continue
			}
			for _, x := range []string{o.pc, o.wk, o.sub} {
				if strings.Contains(s, x) {
					bad = append(bad, x)
				}
			}
		}
		return bad
	}
	ctx := tctx(t)
	for _, a := range accts {
		// the device certificate
		r, err := pb.NewFleetClient(a.conn).ListNodes(ctx, &pb.ListNodesRequest{All: true})
		if err != nil {
			t.Fatalf("%s lists nodes: %v", a.user, err)
		}
		mine := map[string]bool{}
		for _, n := range r.Nodes {
			mine[n.Name] = true
		}
		b, _ := protojson.Marshal(r)
		if !mine[a.pc] || !mine[a.wk] || len(others(a, string(b))) > 0 {
			t.Errorf("%s's certificate lists %v", a.user, mine)
		}
		// the panel session
		for _, path := range []string{"/api/nodes?all=1", "/api/history", "/api/audit?limit=2000", "/api/links", "/api/joincodes", "/api/admin/summary?rows=1", "/api/users", "/api/tenants", "/api/me"} {
			resp, body := a.web.do("GET", path, nil, nil)
			if bad := others(a, body); len(bad) > 0 {
				t.Errorf("%s's panel %s (%d) names %v", a.user, path, resp.StatusCode, bad)
			}
			if path == "/api/admin/summary?rows=1" && (resp.StatusCode == http.StatusOK) != a.admin {
				t.Errorf("%s's panel %s: %d", a.user, path, resp.StatusCode)
			}
		}
		// using another user's worker: never
		h.mu.Lock()
		for _, o := range accts {
			if o != a && h.mayUseLocked(a.user, h.nodes[o.wk]) {
				t.Errorf("%s may use %s's worker", a.user, o.user)
			}
		}
		// a worker certificate acts as nobody: not as its owner, not as an admin
		if u := h.userOfLocked(a.worker); u != "" {
			t.Errorf("%s's worker acts as %q", a.user, u)
		}
		v := h.viewerLocked(h.userOfLocked(a.worker), false)
		if h.visibleNodeLocked(v, h.nodes[a.pc]) {
			t.Errorf("%s's worker sees its owner's device", a.user)
		}
		h.mu.Unlock()
		if !a.admin {
			continue
		}
		// admins: the summary is counts, operations on others' machines "not found"
		s, err := pb.NewHubAdminClient(a.conn).FleetSummary(ctx, &pb.FleetSummaryRequest{Rows: true})
		if err != nil {
			t.Fatalf("%s's summary: %v", a.user, err)
		}
		if b, _ := protojson.Marshal(s); len(others(a, string(b))) > 0 {
			t.Errorf("%s's summary names %v", a.user, others(a, string(b)))
		}
		for _, o := range accts {
			if o == a {
				continue
			}
			if _, err := pb.NewHubAdminClient(a.conn).SetDrain(ctx, &pb.SetDrainRequest{Name: o.wk, Draining: true}); status.Code(err) != codes.NotFound {
				t.Errorf("%s drains %s's worker: %v", a.user, o.user, err)
			}
			if _, err := pb.NewFleetClient(a.conn).ShareWorker(ctx, &pb.ShareWorkerRequest{Worker: o.wk, AddUsers: []string{a.user}}); status.Code(err) != codes.NotFound {
				t.Errorf("%s shares %s's worker with itself: %v", a.user, o.user, err)
			}
			if resp, _ := a.web.do("POST", "/api/nodes/"+o.wk+"/drain", nil, map[string]string{"Content-Type": "application/json"}); resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s's panel drains %s's worker: %d", a.user, o.user, resp.StatusCode)
			}
		}
	}

	// the audit: a sign-in names its provider, and an e-mail address or
	// subject never stands for a user (two users share alice@example.com)
	for _, who := range []string{"google/alice@example.com", "microsoft/alice@example.com", "apple/" + appleAdmin, "entra/alice@example.com"} {
		if u := h.auditActorUser(who); u != "" {
			t.Errorf("audit actor %s is user %q", who, u)
		}
	}
	rows, err := h.store.Audit("hub", tsTime(nil), 1000)
	if err != nil {
		t.Fatal(err)
	}
	approvals := map[string]string{}
	for _, r := range rows {
		if r.Method == "link/approve" {
			approvals[r.Who] = r.SubjectUser
		}
	}
	for _, a := range accts {
		who := a.provider + "/" + cmpOr(a.email, a.sub)
		if approvals[who] != a.user {
			t.Errorf("approval by %s: %v", who, approvals)
		}
	}
	h.mu.Lock()
	alice2 := h.viewerLocked("alice2", false)
	h.mu.Unlock()
	res, err := h.AuditFor(alice2, "test", &pb.AuditRequest{Limit: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := protojson.Marshal(res); strings.Contains(string(b), accts[1].pc) || strings.Contains(string(b), accts[1].wk) || strings.Contains(string(b), `"user":"alice"`) {
		t.Errorf("alice2 reads alice's audit records: %s", b)
	}
}
