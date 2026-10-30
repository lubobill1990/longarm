package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Join codes (DESIGN §11.2): 6 characters of Crockford base32 (no I L O U;
// 32^6 ~ 1e9 combinations), shown as "K7P-Q2M", 10 minutes, one use. Only a
// hash is stored. Attempts are rate limited per IP.
const codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func newJoinCode() string {
	b := make([]byte, 6)
	rand.Read(b)
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)] // 256 % 32 == 0: unbiased
	}
	return string(b[:3]) + "-" + string(b[3:])
}

// normalizeCode uppercases, drops separators and maps look-alikes.
func normalizeCode(s string) string {
	s = strings.NewReplacer("-", "", " ", "", "O", "0", "I", "1", "L", "1").Replace(strings.ToUpper(s))
	return s
}

func codeHash(code string) string {
	s := sha256.Sum256([]byte("Longarm join code:" + normalizeCode(code)))
	return hex.EncodeToString(s[:])
}

func tokenHash(tok string) string {
	s := sha256.Sum256([]byte("Longarm poll token:" + tok))
	return hex.EncodeToString(s[:])
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateJoinCode makes a join code (by = who asked).
func (h *Hub) CreateJoinCode(by string, req *pb.CreateJoinCodeRequest) (*pb.JoinCode, error) {
	role := req.Role
	if role == "" {
		role = string(pki.RoleWorker)
	}
	r, err := pki.ParseRole(role)
	if err != nil || r == pki.RoleRelay || r == pki.RoleHub || r == pki.RoleRelease { // release: longarm admin issue only
		return nil, status.Errorf(codes.InvalidArgument, "role %q: want worker, client or admin", role)
	}
	if req.Name != "" && !pki.ValidName(req.Name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid name %q", req.Name)
	}
	if req.User != "" {
		if r != pki.RoleClient && r != pki.RoleAdmin {
			return nil, status.Error(codes.InvalidArgument, "only client and admin codes belong to a user")
		}
		if !pki.ValidUser(req.User) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid user name %q", req.User)
		}
		h.mu.Lock()
		refused := h.adminCredentialsRefusedLocked(req.User)
		h.mu.Unlock()
		if refused != nil {
			return nil, refused
		}
	}
	if req.Owner != "" {
		if r != pki.RoleWorker {
			return nil, status.Error(codes.InvalidArgument, "only worker codes have an owner")
		}
		if !pki.ValidUser(req.Owner) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid owner %q", req.Owner)
		}
		h.mu.Lock()
		known := h.tenants[req.Owner] != nil
		h.mu.Unlock()
		if !known { // codes of removed users must die with them: owners are users
			return nil, status.Errorf(codes.NotFound, "no user %q (longarm admin user add %s)", req.Owner, req.Owner)
		}
		// cheap abuse guard: owned workers join without approval
		n, err := h.activeWorkerCodes(req.Owner)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		if n >= maxWorkerCodes {
			return nil, status.Errorf(codes.ResourceExhausted, "%s has %d unused worker codes already (they expire; max %d)", req.Owner, n, maxWorkerCodes)
		}
	}
	if (r == pki.RoleClient || r == pki.RoleAdmin) && req.User == "" && h.TenancyOn() {
		return nil, errUserlessCode
	}
	ttl := 10 * time.Minute
	if req.Ttl != nil {
		ttl = req.Ttl.AsDuration()
	}
	if ttl <= 0 || ttl > 7*24*time.Hour {
		return nil, status.Error(codes.InvalidArgument, "ttl must be between 1s and 7 days")
	}
	if err := sched.ValidateLabels(req.Labels, false); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := sched.CheckUserLabels(req.Labels); err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	if req.Profile != "" {
		h.mu.Lock()
		_, ok := h.profiles[req.Profile]
		h.mu.Unlock()
		if !ok {
			return nil, status.Errorf(codes.NotFound, "no profile %q", req.Profile)
		}
	}
	code := newJoinCode()
	hash := codeHash(code)
	now := time.Now().UTC()
	jc := &store.JoinCode{ID: hash[:8], Role: role, Name: req.Name, Labels: req.Labels, Profile: req.Profile,
		CreatedBy: by, CreatedAt: now, ExpiresAt: now.Add(ttl), User: req.User, Owner: req.Owner}
	if err := h.store.AddJoinCode(hash, jc); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	h.log.Info("join code created", "id", jc.ID, "role", role, "owner", jc.Owner, "by", by, "expires", jc.ExpiresAt)
	if jc.Owner != "" {
		h.auditHub(by, "workers/code", map[string]any{"id": jc.ID, "owner": jc.Owner, "name": jc.Name, "expires": jc.ExpiresAt})
	}
	out := joinCodePB(jc)
	out.Code = code
	return out, nil
}

func joinCodePB(c *store.JoinCode) *pb.JoinCode {
	return &pb.JoinCode{Id: c.ID, Role: c.Role, ExpiresAt: tspb(c.ExpiresAt), Labels: c.Labels, Profile: c.Profile,
		CreatedBy: c.CreatedBy, Used: !c.UsedAt.IsZero(), UsedBy: c.UsedBy, Name: c.Name, User: c.User, Owner: c.Owner}
}

type enrollServer struct {
	pb.UnimplementedEnrollServer
	h *Hub
}

var errBadCode = status.Error(codes.PermissionDenied, "invalid, used or expired join code")

func (s *enrollServer) Join(ctx context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	h := s.h
	c := callerFrom(ctx)
	if !h.joinLimit.allow(c.addr) {
		return nil, status.Error(codes.ResourceExhausted, "too many join attempts; wait a minute")
	}
	code := normalizeCode(req.Code)
	if len(code) != 6 {
		return nil, errBadCode
	}
	if err := checkDevice(req.Hostname, req.Os, req.Arch, req.Version); err != nil {
		return nil, err
	}
	if _, err := pki.ParseCSR(req.CsrPem); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "csr: %v", err)
	}
	hash := codeHash(code)
	now := time.Now().UTC()
	jc, err := h.store.PeekJoinCode(hash, now)
	if errors.Is(err, store.ErrNotFound) {
		h.log.Warn("join with a bad code", "remote", c.addr)
		h.tel().Count("enroll.bad_code", nil, 1)
		return nil, errBadCode
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	name := jc.Name
	if name == "" {
		name = req.Name
	}
	if name == "" {
		name = sanitizeName(req.Hostname)
	}
	if !pki.ValidName(name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid node name %q (use --name)", name)
	}
	h.mu.Lock()
	if n := h.nodes[name]; n != nil && n.rec.State != stateRevoked {
		h.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "node name %q is taken (pick another with --name, or revoke the old node)", name)
	}
	if h.nodeNameReservedLocked(name, jc.Role, jc.User) {
		h.mu.Unlock()
		return nil, errNameReserved(name)
	}
	if jc.Owner != "" && (h.tenants[jc.Owner] == nil || h.userDisabledLocked(jc.Owner)) { // removed or disabled since
		h.mu.Unlock()
		return nil, errBadCode
	}
	if jc.User != "" { // enrolled, signed in, shared with meanwhile: the code was for a new user
		if err := h.adminCredentialsRefusedLocked(jc.User); err != nil {
			h.mu.Unlock()
			return nil, err
		}
	}
	if (jc.Role == string(pki.RoleClient) || jc.Role == string(pki.RoleAdmin)) && jc.User == "" && h.tenancyOnLocked() { // made before users were separated
		h.mu.Unlock()
		return nil, errUserlessCode
	}
	h.mu.Unlock()
	if jc, err = h.store.UseJoinCode(hash, name, now); err != nil {
		return nil, errBadCode
	}
	rec := &store.Node{Name: name, Role: jc.Role, Labels: maps.Clone(jc.Labels), Profile: jc.Profile,
		Hostname: req.Hostname, OS: req.Os, Arch: req.Arch, Version: req.Version, JoinedAt: now, User: jc.User}
	resp := &pb.JoinResponse{Name: name, Role: jc.Role, CaPem: h.ca.CertPEM, HubName: h.cfg.Name, Labels: maps.Clone(jc.Labels)}
	if jc.Role == string(pki.RoleWorker) && jc.Owner != "" {
		// A user's own machine: nobody else may use it (until shared), so
		// there is nothing for an admin to approve.
		rec.Owner, rec.AccessVersion, resp.Owner = jc.Owner, 1, jc.Owner
	}
	if jc.Role == string(pki.RoleWorker) && !h.cfg.AutoApprove && jc.Owner == "" {
		tok := randomToken()
		rec.State, rec.CSR, rec.PollTokenHash = statePending, string(req.CsrPem), tokenHash(tok)
		resp.State, resp.PollToken = statePending, tok
		resp.Message = "waiting for approval: longarm admin approve " + name
	} else {
		cert, err := h.issue(rec, req.CsrPem)
		if err != nil {
			return nil, err
		}
		rec.State, rec.ApprovedAt = stateApproved, now
		resp.State, resp.CertPem = stateApproved, cert
	}
	h.mu.Lock()
	if n := h.nodes[name]; n != nil && n.rec.State != stateRevoked { // raced with another join
		h.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "node name %q is taken", name)
	}
	if err := h.store.PutNode(rec); err != nil {
		h.mu.Unlock()
		return nil, status.Error(codes.Internal, err.Error())
	}
	n := &node{rec: rec}
	h.nodes[name] = n
	h.log.Info("node joined", "node", name, "role", rec.Role, "state", rec.State, "owner", rec.Owner, "code", jc.ID, "remote", c.addr)
	h.event(ctx, "node.joined", map[string]string{"role": rec.Role, "state": rec.State, "os": req.Os, "arch": req.Arch, "node_version": req.Version})
	h.publishNodeLocked(n, "joined ("+rec.State+")")
	if rec.Owner != "" {
		h.checkTenancyFlipLocked() // an owned worker separates users
	}
	h.mu.Unlock()
	if rec.Owner != "" {
		h.auditHub("node/"+name, "workers/join", map[string]any{"worker": name, "owner": rec.Owner, "code": jc.ID, "remote": c.addr})
	}
	h.attachTailnet(ctx, resp) // after the race check, not under h.mu (HTTP to Headscale)
	return resp, nil
}

func (s *enrollServer) JoinStatus(ctx context.Context, req *pb.JoinStatusRequest) (*pb.JoinResponse, error) {
	h := s.h
	if !h.joinLimit.allow(callerFrom(ctx).addr) {
		return nil, status.Error(codes.ResourceExhausted, "too many attempts; slow down")
	}
	th := tokenHash(req.PollToken)
	h.mu.Lock()
	resp := h.joinStatusLocked(th)
	h.mu.Unlock()
	if resp == nil {
		return nil, status.Error(codes.NotFound, "unknown poll token")
	}
	h.attachTailnet(ctx, resp)
	return resp, nil
}

func (h *Hub) joinStatusLocked(th string) *pb.JoinResponse {
	for _, n := range h.nodes {
		if n.rec.PollTokenHash == "" || n.rec.PollTokenHash != th {
			continue
		}
		resp := &pb.JoinResponse{Name: n.rec.Name, Role: n.rec.Role, CaPem: h.ca.CertPEM, HubName: h.cfg.Name, State: n.rec.State,
			Owner: n.rec.Owner}
		switch n.rec.State {
		case stateApproved:
			resp.CertPem = []byte(n.rec.IssuedCert)
		case stateRevoked:
			resp.State, resp.Message = "rejected", "the enrollment was rejected"
		default:
			resp.Message = "waiting for approval: longarm admin approve " + n.rec.Name
		}
		return resp
	}
	return nil
}

// issue signs csr for rec's identity and records the certificate.
func (h *Hub) issue(rec *store.Node, csr []byte) ([]byte, error) {
	return h.issueHosts(rec, csr, nil)
}

// issueHosts is issue with DNS/IP SANs (a relay's renewal keeps its names).
func (h *Hub) issueHosts(rec *store.Node, csr []byte, hosts []string) ([]byte, error) {
	ttl := h.cfg.TTL(rec.Role)
	cert, err := h.ca.SignCSR(csr, pki.IssueOptions{Role: pki.Role(rec.Role), Name: rec.Name, Validity: ttl,
		Hosts: hosts, User: rec.User})
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "sign: %v", err)
	}
	leaf, err := pki.ParseCertPEM(cert)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := h.store.AddCert(store.Cert{Serial: pki.Serial(leaf), Node: rec.Name, NotAfter: leaf.NotAfter, IssuedAt: time.Now()}); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	h.mu.Lock()
	h.knownSerials[pki.Serial(leaf)] = true
	h.mu.Unlock()
	rec.CertSerial, rec.CertNotAfter = pki.Serial(leaf), leaf.NotAfter
	return cert, nil
}

// Approve admits a pending node (or re-approves one).
func (h *Hub) Approve(by, name string) (*pb.NodeInfo, error) {
	h.certMu.Lock()
	defer h.certMu.Unlock()
	h.mu.Lock()
	n := h.nodes[name]
	if n == nil {
		h.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "no node %q", name)
	}
	if n.rec.State == stateApproved {
		info := h.nodeInfoLocked(n)
		h.mu.Unlock()
		return info, nil
	}
	if n.rec.State != statePending || n.rec.CSR == "" {
		h.mu.Unlock()
		return nil, status.Errorf(codes.FailedPrecondition, "node %s is %s", name, n.rec.State)
	}
	rec := *n.rec
	h.mu.Unlock()
	cert, err := h.issue(&rec, []byte(rec.CSR))
	if err != nil {
		return nil, err
	}
	rec.State, rec.ApprovedAt, rec.IssuedCert, rec.CSR = stateApproved, time.Now().UTC(), string(cert), ""
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.store.PutNode(&rec); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	n.rec = &rec
	h.log.Info("node approved", "node", name, "by", by)
	h.event(context.Background(), "node.approved", map[string]string{"role": rec.Role})
	h.publishNodeLocked(n, "approved by "+by)
	return h.nodeInfoLocked(n), nil
}

// Revoke revokes a node and all its certificates and pushes the new
// revocation list to every connected worker.
func (h *Hub) Revoke(by, name string) (*pb.NodeInfo, error) {
	h.certMu.Lock()
	defer h.certMu.Unlock()
	h.mu.Lock()
	n := h.nodes[name]
	if n == nil {
		h.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "no node %q", name)
	}
	rec := *n.rec
	rec.State, rec.CSR, rec.IssuedCert = stateRevoked, "", "" // the poll token stays: JoinStatus says "rejected"
	if _, err := h.store.RevokeNodeCerts(name, time.Now()); err != nil {
		h.mu.Unlock()
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := h.store.PutNode(&rec); err != nil {
		h.mu.Unlock()
		return nil, status.Error(codes.Internal, err.Error())
	}
	n.rec = &rec
	h.revVersion++
	h.store.SetMeta("revocation_version", itoa(h.revVersion))
	if err := h.rebuildRevocations(); err != nil {
		h.log.Warn("rebuild revocations", "err", err)
	}
	if n.sess != nil {
		n.sess.cancel()
		h.setOfflineLocked(n, n.sess.id)
	}
	h.removePortLinksLocked(func(l *pb.PortLink) bool { return l.From == name || l.To == name }, by, "node revoked")
	role := n.rec.Role
	h.log.Info("node revoked", "node", name, "by", by, "revocation_version", h.revVersion)
	h.event(context.Background(), "node.revoked", map[string]string{"role": role})
	h.publishNodeLocked(n, "revoked by "+by)
	info := h.nodeInfoLocked(n)
	if role == string(pki.RoleRelay) {
		h.forgetRelaysLocked(name) // out of the relay set at once
	}
	h.mu.Unlock()
	h.broadcast(h.revocationsMsg())
	h.relayPolicyNotify() // external relays get the new list
	// End what already runs: hub streams of the node, its relay sessions.
	closed := h.conns.CloseIf(h.revoked.Revoked)
	kicked := 0
	if h.relay != nil {
		kicked = h.relay.Kick(func(id pki.Identity) bool { return id.Name == name && string(id.Role) == role }, "revoked")
	}
	h.log.Info("revocation enforced", "node", name, "hub_conns_closed", closed, "relay_sessions_closed", kicked)
	return info, nil
}

// renew issues a fresh certificate for an authenticated caller.
func (h *Hub) renew(c caller, csr []byte) (*pb.RenewResponse, error) {
	if !h.renewLimit.allow(c.id.String()) {
		return nil, status.Error(codes.ResourceExhausted, "too many renewals")
	}
	h.certMu.Lock()
	defer h.certMu.Unlock()
	h.mu.Lock()
	n := h.nodes[c.id.Name]
	if n == nil || n.rec.State != stateApproved {
		h.mu.Unlock()
		return nil, status.Errorf(codes.FailedPrecondition, "node %s is not approved", c.id.Name)
	}
	rec := *n.rec
	h.mu.Unlock()
	var hosts []string
	if c.id.Role == pki.RoleRelay && c.cert != nil { // nodes may check the relay's name: keep it
		hosts = append(hosts, c.cert.DNSNames...)
		for _, ip := range c.cert.IPAddresses {
			hosts = append(hosts, ip.String())
		}
	}
	cert, err := h.issueHosts(&rec, csr, hosts)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	n.rec.CertSerial, n.rec.CertNotAfter = rec.CertSerial, rec.CertNotAfter
	err = h.store.PutNode(n.rec)
	h.mu.Unlock()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	h.log.Info("certificate renewed", "node", c.id.String(), "serial", rec.CertSerial, "not_after", rec.CertNotAfter)
	h.event(context.Background(), "cert.renewed", map[string]string{"role": string(c.id.Role)})
	return &pb.RenewResponse{CertPem: cert, CaPem: h.ca.CertPEM}, nil
}

// sanitizeName turns a hostname into a node name.
func sanitizeName(host string) string {
	host = strings.ToLower(strings.SplitN(host, ".", 2)[0])
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-_")
}

func renewBeforePB(d time.Duration) *durationpb.Duration { return durationpb.New(d) }

// checkDevice bounds what an enrolling machine says about itself - anyone
// may send it; it is stored, shown in the panel and on /link.
func checkDevice(hostname, os, arch, version string) error {
	for _, f := range []struct {
		name, v string
		max     int
	}{{"hostname", hostname, 255}, {"os", os, 32}, {"arch", arch, 32}, {"version", version, 64}} {
		if len(f.v) > f.max || !utf8.ValidString(f.v) || strings.ContainsFunc(f.v, unicode.IsControl) {
			return status.Errorf(codes.InvalidArgument, "invalid %s (at most %d printable characters)", f.name, f.max)
		}
	}
	return nil
}

// userHasMachinesLocked reports whether user has a machine that is not
// revoked: a device (client, admin) of the user's or a worker it owns.
func (h *Hub) userHasMachinesLocked(user string) bool {
	for _, n := range h.nodes {
		if n.rec.State == stateRevoked {
			continue
		}
		if n.rec.Role == string(pki.RoleWorker) {
			if n.rec.Owner == user {
				return true
			}
			continue
		}
		if userNodeRole(n.rec.Role) && nodeUser(n.rec) == user {
			return true
		}
	}
	return false
}

// errUserHasMachines refuses an admin's credentials for a user (a join code
// bound to the user: user add / user code / join-code --user; a panel
// login: --panel) once the user has machines. They bootstrap a new user
// only: after that, a device or login of the user's comes from the user's
// own sign-in (longarm login, a pairing link the user approves) - otherwise
// an admin could mint itself credentials of the user and act as the user,
// on the user's machines.
func errUserHasMachines(user string) error {
	return status.Errorf(codes.FailedPrecondition,
		"%s has machines already: an admin's join code or panel login for a user bootstraps a new user only; further devices come from %s's own sign-in (longarm login, or a pairing link %s approves)", user, user, user)
}

// adminCredentialsRefusedLocked says why an admin may not make credentials
// that act as user (a join code bound to the user: user add / user code /
// join-code --user; a panel login: --panel), or nil. They bootstrap a new
// user only - one with nothing of its own and no access to anybody else's:
// otherwise an admin could mint itself credentials of the user and act as
// the user, on the user's machines or on the workers and jobs others share
// with the user by name. Refused for a user who has machines, who signs in
// with an identity provider (Entra, Google, Apple, a personal Microsoft
// account: its devices come from its own sign-in) or who has another
// user's worker shared with it. (Teams are the admins' to administer:
// docs/security.md "What admins see".)
func (h *Hub) adminCredentialsRefusedLocked(user string) error {
	if h.userHasMachinesLocked(user) {
		return errUserHasMachines(user)
	}
	if id, _ := h.store.Meta(entraUserKey(user)); id != "" {
		return status.Errorf(codes.FailedPrecondition,
			"%s signs in with an identity provider: its devices and logins come from its own sign-in (longarm login, or a pairing link %s approves), not from an admin", user, user)
	}
	if h.sharedWithLocked(user) {
		return status.Errorf(codes.FailedPrecondition,
			"%s has workers shared with it: an admin's join code or panel login for a user bootstraps a new user only; %s's devices come from its own sign-in (longarm login, or a pairing link %s approves)", user, user, user)
	}
	return nil
}

// sharedWithLocked reports whether another user's live worker is shared
// with user by name.
func (h *Hub) sharedWithLocked(user string) bool {
	for _, n := range h.nodes {
		if n.rec.Role == string(pki.RoleWorker) && n.rec.State != stateRevoked && n.rec.Owner != user &&
			n.rec.Shares != nil && slices.Contains(n.rec.Shares.Users, user) {
			return true
		}
	}
	return false
}

// errUserlessCode refuses a client or admin join code bound to no user once
// users are separated: its device would act as the user its name names
// (userOfLocked) - a way for an admin to become another user.
var errUserlessCode = status.Error(codes.FailedPrecondition,
	"with users separated, a client or admin code belongs to a user (longarm admin user add / user code, or join-code --user)")
