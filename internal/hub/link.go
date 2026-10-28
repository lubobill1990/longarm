package hub

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"html/template"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/machine"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Pairing (docs/usage.md "Sign in with your Microsoft account",
// docs/security.md "Pairing"): a machine without a usable az sign-in -
// headless, no Azure CLI, device code flow blocked by policy - enrolls
// through its user's browser. The machine sends its CSR (Enroll.StartLink)
// and prints the panel's /link URL and a user code; the user, signed in to
// the panel with Microsoft, types the code, checks what asks (kind, name,
// host, address, node key) and approves; the machine collects its
// certificate (Enroll.PollLink). Approving is signing in: a client becomes
// the approver's device, a worker the approver's own worker. Kind "both"
// (a machine that is both: `longarmd login --role both`, the installers'
// --role both, the tray) carries two CSRs - the client's key and
// longarmd's - and one approval enrolls both; the page lists both.
// Any kind may carry an admin worker's CSR as well (admin_worker_csr_pem:
// Windows' ADMIN_WORKER, a longarmd running as LocalSystem): the same
// approval enrolls it as the approver's admin worker (store.Node.Admin,
// label longarm.io/admin=true: owner-only, never shared, never placed),
// and the page says in so many words that it runs commands as SYSTEM.
//
// The code is only ever typed, never part of a link: a link that pre-fills
// a code is what a phisher would send. Codes are 8 characters (20^8), live
// 10 minutes, work once, and lookups are rate limited; pending pairings
// are capped globally and per address. They live in memory: a restarted
// hub forgets them (the machine starts over).
//
//	GET  /link         the code form (sign-in first)
//	POST /link         code -> the confirmation page
//	POST /link/decide  approve / deny

const (
	linkClient = "client"
	linkWorker = "worker"
	// linkBoth: a client device and an owned worker of the same machine
	// (two CSRs: longarm's key and longarmd's), one code, one approval
	linkBoth = "both"
	// linkAdmin: an admin worker alone (csr_pem is its CSR)
	linkAdmin = "admin"

	pairingTTL    = 10 * time.Minute
	linkInterval  = 5 * time.Second
	maxLinks      = 200 // pending pairings, all addresses
	maxLinksPerIP = 5
	// consonants only: no words, no look-alikes (0/O, 1/I/L, 5/S ...)
	linkAlphabet = "BCDFGHJKLMNPQRSTVWXZ"
	linkCodeLen  = 8

	entraMarkCookie = "longarm_hub_entra"
	linkKeyMeta     = "link_form_key"
	linkFormTTL     = 10 * time.Minute
	// pairingGrace: how long past its expiry a pairing's result waits for
	// the device (then it is forgotten: NotFound reads as expired)
	pairingGrace = time.Minute
)

// Pairing states (PollLink's JoinResponse.state); "approving" is internal.
const (
	linkPending   = "pending"
	linkApproving = "approving"
	linkApproved  = "approved"
	linkDenied    = "denied"
	linkExpired   = "expired"
)

type pairLink struct {
	id                                string // names the pairing in the confirmation form
	code                              string // normalized
	pollHash                          string
	kind                              string
	csr                               []byte
	name, hostname, os, arch, version string
	ip                                string // as the hub saw it
	keyID                             string // the CSR's end-to-end trust node key, if any
	started                           time.Time
	expires                           time.Time
	lastPoll                          time.Time
	state                             string
	message                           string
	resp                              *pb.JoinResponse // approved, until delivered
	serial                            string           // of resp's certificate
	// kind both: the worker's part (the fields above are the client's)
	workerCSR               []byte
	workerName, workerKeyID string
	workerSerial            string
	workerLabels            map[string]string // kind worker / both
	// admin worker (any kind): its CSR, requested name and node key
	admin                 bool
	adminCSR              []byte
	adminName, adminKeyID string
	adminSerial           string
}

// drop forgets the CSRs (decided or expired).
func (l *pairLink) drop() { l.csr, l.workerCSR, l.adminCSR = nil, nil, nil }

func (l *pairLink) fields() map[string]any {
	f := map[string]any{"id": l.id, "kind": l.kind, "name": l.name, "hostname": l.hostname, "ip": l.ip,
		"os": l.os, "arch": l.arch, "node_key": l.keyID}
	if l.kind == linkBoth {
		f["worker_name"], f["worker_node_key"] = l.workerName, l.workerKeyID
	}
	if len(l.workerLabels) > 0 {
		f["worker_labels"] = l.workerLabels
	}
	if l.admin {
		f["admin_worker"], f["admin_worker_name"], f["admin_worker_node_key"] = true, l.adminName, l.adminKeyID
	}
	return f
}

type pairing struct {
	mu     sync.Mutex
	byCode map[string]*pairLink // pending pairings by code
	byPoll map[string]*pairLink // by poll token hash, until well past expiry
	key    []byte               // form token / Entra mark MAC key (linkKeyMeta)

	pollLimit, codeLimit *ipLimiter
}

func newPairing() *pairing {
	return &pairing{byCode: map[string]*pairLink{}, byPoll: map[string]*pairLink{},
		pollLimit: newIPLimiter(1, 20), codeLimit: newIPLimiter(0.1, 10)}
}

// sweepLocked expires overdue pairings (returned, for the audit) and
// forgets long expired ones.
func (p *pairing) sweepLocked(now time.Time) []*pairLink {
	var out []*pairLink
	for th, l := range p.byPoll {
		if l.state == linkPending && now.After(l.expires) {
			l.state, l.message = linkExpired, "the code expired before it was approved"
			l.drop()
			delete(p.byCode, l.code)
			out = append(out, l)
		}
		if now.After(l.expires.Add(pairingGrace)) { // results too: the device polls every few seconds
			delete(p.byPoll, th)
			if p.byCode[l.code] == l {
				delete(p.byCode, l.code)
			}
		}
	}
	return out
}

func (h *Hub) auditExpired(ls []*pairLink) {
	for _, l := range ls {
		h.auditHub("link/"+l.ip, "link/expire", l.fields())
		h.log.Info("pairing expired", "id", l.id, "kind", l.kind, "remote", l.ip)
	}
}

func newLinkCode() string {
	b := make([]byte, 0, linkCodeLen)
	limit := 256 - 256%len(linkAlphabet) // unbiased: drop the remainder
	for len(b) < linkCodeLen {
		var r [16]byte
		rand.Read(r[:])
		for _, x := range r {
			if int(x) < limit && len(b) < linkCodeLen {
				b = append(b, linkAlphabet[int(x)%len(linkAlphabet)])
			}
		}
	}
	return string(b)
}

func formatLinkCode(c string) string { return c[:4] + "-" + c[4:] }

// normalizeLinkCode accepts "bcdf-ghjk", "BCDF GHJK" ...
func normalizeLinkCode(s string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == '-' || r == ' ' || r == '\t':
		case r < 128 && strings.ContainsRune(linkAlphabet, r):
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	return b.String(), b.Len() == linkCodeLen
}

func ipOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// linkUnavailable: pairing needs a panel the user can sign in to - with
// Microsoft when the hub has Entra sign-in.
func (h *Hub) linkUnavailable() error {
	if h.entra != nil && h.entraWeb() == "" {
		return status.Error(codes.FailedPrecondition, "pairing needs the panel's Microsoft sign-in, which is off on this hub ([auth.entra] web); use az (--az) or a join code")
	}
	return nil
}

// linkURL is the /link page as the device should open it: public_url,
// else the HTTP listener on the host the device dialed.
func (h *Hub) linkURL(ctx context.Context) string {
	if h.cfg.PublicURL != "" {
		return h.cfg.PublicURL + "/link"
	}
	_, port, err := net.SplitHostPort(h.Addr("http"))
	if err != nil {
		return ""
	}
	host := "localhost"
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if a := md.Get(":authority"); len(a) > 0 && hostRE.MatchString(a[0]) {
			host = ipOnly(a[0])
		}
	}
	return "http://" + net.JoinHostPort(strings.Trim(host, "[]"), port) + "/link"
}

// ---------------------------------------------------------------- gRPC

// linkCSR checks a pairing's CSR and requested name; it returns the CSR
// as parsed (re-encoded: nothing else of what was sent is kept) and the
// end-to-end trust node key it binds, if any. what prefixes errors.
func linkCSR(csrPEM []byte, name, what string) ([]byte, string, error) {
	csr, err := pki.ParseCSR(csrPEM) // bounded: pki.MaxCSR
	if err != nil {
		return nil, "", status.Errorf(codes.InvalidArgument, "%scsr: %v", what, err)
	}
	keyID := ""
	if t, err := pki.TrustOfCSR(csr); err != nil {
		return nil, "", status.Errorf(codes.InvalidArgument, "%scsr: %v", what, err)
	} else if t != nil {
		keyID = pki.KeyID(t.NodeKey)
	}
	if name != "" && !pki.ValidName(name) {
		return nil, "", status.Errorf(codes.InvalidArgument, "invalid %sname %q", what, name)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr.Raw}), keyID, nil
}

func (s *enrollServer) StartLink(ctx context.Context, req *pb.StartLinkRequest) (*pb.StartLinkResponse, error) {
	h := s.h
	c := callerFrom(ctx)
	if !h.joinLimit.allow(c.addr) {
		return nil, status.Error(codes.ResourceExhausted, "too many join attempts; wait a minute")
	}
	if req.Kind != linkClient && req.Kind != linkWorker && req.Kind != linkBoth && req.Kind != linkAdmin {
		return nil, status.Errorf(codes.InvalidArgument, "kind %q: want client, worker, both or admin", req.Kind)
	}
	if req.Kind == linkAdmin && (len(req.AdminWorkerCsrPem) > 0 || req.AdminWorkerName != "" || len(req.WorkerLabels) > 0) {
		return nil, status.Error(codes.InvalidArgument, "kind admin: csr_pem and name are the admin worker's (no admin_worker_* fields, no labels)")
	}
	if (req.Kind == linkBoth) != (len(req.WorkerCsrPem) > 0) {
		return nil, status.Error(codes.InvalidArgument, "worker_csr_pem goes with kind both (and only with it)")
	}
	if err := h.linkUnavailable(); err != nil {
		return nil, err
	}
	if err := checkDevice(req.Hostname, req.Os, req.Arch, req.Version); err != nil {
		return nil, err
	}
	if err := checkWorkerLabels(req.Kind == linkWorker || req.Kind == linkBoth, req.WorkerLabels); err != nil {
		return nil, err
	}
	csr, keyID, err := linkCSR(req.CsrPem, req.Name, "")
	if err != nil {
		return nil, err
	}
	var wcsr []byte
	var wkeyID string
	if req.Kind == linkBoth {
		if wcsr, wkeyID, err = linkCSR(req.WorkerCsrPem, req.WorkerName, "worker "); err != nil {
			return nil, err
		}
		if req.WorkerName != "" && req.WorkerName == req.Name {
			return nil, status.Errorf(codes.InvalidArgument, "the device and the worker can't both be named %q", req.Name)
		}
	}
	var acsr []byte
	var akeyID string
	if len(req.AdminWorkerCsrPem) > 0 {
		if acsr, akeyID, err = linkCSR(req.AdminWorkerCsrPem, req.AdminWorkerName, "admin worker "); err != nil {
			return nil, err
		}
		if n := req.AdminWorkerName; n != "" && (n == req.Name || n == req.WorkerName) {
			return nil, status.Errorf(codes.InvalidArgument, "the admin worker can't share the name %q with the device or the worker", n)
		}
	} else if req.AdminWorkerName != "" {
		return nil, status.Error(codes.InvalidArgument, "admin_worker_name goes with admin_worker_csr_pem")
	}
	u := h.linkURL(ctx)
	if u == "" {
		return nil, status.Error(codes.FailedPrecondition, "this hub has no web panel (http) to approve pairings at")
	}
	now := time.Now()
	tok := randomToken()
	// Kept: the CSR as parsed (re-encoded, nothing else of what was sent).
	l := &pairLink{id: randomB64(12), pollHash: tokenHash(tok), kind: req.Kind, name: req.Name, csr: csr,
		hostname: req.Hostname, os: req.Os, arch: req.Arch, version: req.Version,
		ip: ipOnly(c.addr), keyID: keyID, started: now, expires: now.Add(pairingTTL), state: linkPending,
		workerCSR: wcsr, workerName: req.WorkerName, workerKeyID: wkeyID, workerLabels: req.WorkerLabels,
		admin: acsr != nil, adminCSR: acsr, adminName: req.AdminWorkerName, adminKeyID: akeyID}
	if l.kind == linkAdmin {
		l.admin, l.adminName, l.adminKeyID = true, l.name, l.keyID
	}
	p := h.pair
	p.mu.Lock()
	expired := p.sweepLocked(now)
	// Counted: every pairing still holding a CSR (pending or being approved).
	live, perIP := 0, 0
	for _, x := range p.byPoll {
		if x.csr == nil {
			continue
		}
		live++
		if x.ip == l.ip {
			perIP++
		}
	}
	if live >= maxLinks || perIP >= maxLinksPerIP {
		p.mu.Unlock()
		h.auditExpired(expired)
		h.log.Warn("pairing refused: too many pending", "remote", l.ip, "pending", live, "from_ip", perIP)
		return nil, status.Error(codes.ResourceExhausted, "too many pending pairings; finish or wait for the others (10 minutes)")
	}
	for l.code == "" || p.byCode[l.code] != nil {
		l.code = newLinkCode()
	}
	p.byCode[l.code], p.byPoll[l.pollHash] = l, l
	p.mu.Unlock()
	h.auditExpired(expired)
	h.log.Info("pairing started", "id", l.id, "kind", l.kind, "name", l.name, "hostname", l.hostname, "admin_worker", l.admin, "remote", l.ip)
	h.auditHub("link/"+l.ip, "link/start", l.fields())
	return &pb.StartLinkResponse{UserCode: formatLinkCode(l.code), PollToken: tok, VerificationUrl: u,
		ExpiresAt: tspb(l.expires), Interval: durationpb.New(linkInterval)}, nil
}

func (s *enrollServer) PollLink(ctx context.Context, req *pb.PollLinkRequest) (*pb.JoinResponse, error) {
	h := s.h
	c := callerFrom(ctx)
	if !h.pair.pollLimit.allow(c.addr) {
		return nil, status.Error(codes.ResourceExhausted, "too many attempts; slow down")
	}
	now := time.Now()
	p := h.pair
	p.mu.Lock()
	expired := p.sweepLocked(now)
	th := tokenHash(req.PollToken)
	l := p.byPoll[th]
	var resp *pb.JoinResponse
	var serial, wserial, aserial string
	var err error
	switch {
	case l == nil:
		err = status.Error(codes.NotFound, "unknown or expired pairing")
	case (l.state == linkPending || l.state == linkApproving) && now.Sub(l.lastPoll) < linkInterval-time.Second:
		err = status.Errorf(codes.ResourceExhausted, "slow down: poll every %s", linkInterval)
	case l.state == linkApproved:
		// Delivered once: the poll token is spent with it.
		resp, serial, wserial, aserial = l.resp, l.serial, l.workerSerial, l.adminSerial
		delete(p.byPoll, th)
	case l.state == linkDenied || l.state == linkExpired:
		resp = &pb.JoinResponse{State: l.state, Message: l.message}
	default:
		resp = &pb.JoinResponse{State: linkPending, Message: "waiting for approval"}
	}
	if l != nil && err == nil {
		l.lastPoll = now
	}
	p.mu.Unlock()
	h.auditExpired(expired)
	if err != nil {
		return nil, err
	}
	if serial != "" {
		// Revoked (or replaced) since the approval: no certificate, and no
		// tailnet key for it.
		h.mu.Lock()
		liveNode := func(name, serial string) bool {
			n := h.nodes[name]
			return n != nil && n.rec.State == stateApproved && n.rec.CertSerial == serial
		}
		live := liveNode(resp.Name, serial) && (resp.Worker == nil || liveNode(resp.Worker.Name, wserial)) &&
			(resp.AdminWorker == nil || liveNode(resp.AdminWorker.Name, aserial))
		h.mu.Unlock()
		if !live {
			h.log.Warn("pairing result withheld: the node changed since the approval", "node", resp.Name, "remote", c.addr)
			resp = &pb.JoinResponse{State: linkDenied, Message: "the device was removed after the approval"}
		}
	}
	if resp.CaPem == nil {
		resp.CaPem, resp.HubName = h.ca.CertPEM, h.cfg.Name
	}
	h.attachTailnet(ctx, resp) // approved and live only
	if resp.Worker != nil {
		h.attachTailnet(ctx, resp.Worker)
	}
	if resp.AdminWorker != nil {
		h.attachTailnet(ctx, resp.AdminWorker)
	}
	return resp, nil
}

// ---------------------------------------------------------------- web

func (h *Hub) linkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /link", h.linkPage)
	mux.HandleFunc("POST /link", h.linkLookup)
	mux.HandleFunc("POST /link/decide", h.linkDecide)
}

// linkKey is the MAC key of the /link form tokens and the Entra session
// mark, made once and kept in the database.
func (h *Hub) linkKey() ([]byte, error) {
	p := h.pair
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.key != nil {
		return p.key, nil
	}
	v, err := h.store.Meta(linkKeyMeta)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(v)
	if err != nil || len(key) != 32 {
		key = make([]byte, 32)
		rand.Read(key)
		if err := h.store.SetMeta(linkKeyMeta, hex.EncodeToString(key)); err != nil {
			return nil, err
		}
	}
	p.key = key
	return key, nil
}

func (h *Hub) linkMAC(parts ...string) []byte {
	key, err := h.linkKey()
	if err != nil {
		return nil
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(strings.Join(parts, "\x00")))
	return m.Sum(nil)
}

// setEntraMark marks a panel session as opened by a Microsoft sign-in:
// /link approves only such sessions (when the hub has Entra sign-in), not
// password logins. Bound to the session, so another login replaces it.
func (h *Hub) setEntraMark(w http.ResponseWriter, sessHash string, maxAge int) {
	val := ""
	if maxAge > 0 {
		mac := h.linkMAC("longarm-hub entra session", sessHash)
		if mac == nil {
			return
		}
		val = base64.RawURLEncoding.EncodeToString(mac)
	}
	http.SetCookie(w, &http.Cookie{Name: entraMarkCookie, Value: val, Path: "/", HttpOnly: true,
		Secure: !h.cfg.Web.InsecureCookie, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

func (h *Hub) entraMarked(r *http.Request, sessHash string) bool {
	c, err := r.Cookie(entraMarkCookie)
	if err != nil {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(c.Value)
	want := h.linkMAC("longarm-hub entra session", sessHash)
	return err == nil && want != nil && hmac.Equal(got, want)
}

// linkFormToken binds a form to the session, a step and a pairing: a
// page of another session (or a forged form) can't approve.
func (h *Hub) linkFormToken(sessHash, step, id string) string {
	exp := strconv.FormatInt(time.Now().Add(linkFormTTL).Unix(), 10)
	return exp + "." + base64.RawURLEncoding.EncodeToString(h.linkMAC("longarm-hub link form", sessHash, step, id, exp))
}

func (h *Hub) checkLinkFormToken(tok, sessHash, step, id string) bool {
	exp, mac, ok := strings.Cut(tok, ".")
	sec, err := strconv.ParseInt(exp, 10, 64)
	if !ok || err != nil || time.Now().Unix() > sec {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(mac)
	want := h.linkMAC("longarm-hub link form", sessHash, step, id, exp)
	return err == nil && want != nil && hmac.Equal(got, want)
}

// sameSitePost: a POST from the hub's own pages - an Origin that is the
// hub's, and the browser's fetch metadata (when sent) same-origin.
func (h *Hub) sameSitePost(r *http.Request) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	return r.Header.Get("Origin") != "" && h.sameOrigin(r) && (site == "" || site == "same-origin")
}

// linkApprover is who decides at /link.
type linkApprover struct {
	login    string        // panel account
	user     string        // Longarm user
	admin    bool          // non-Entra hubs: a panel administrator
	id       entraIdentity // Entra sign-ins
	sessHash string
}

var (
	errLinkNoSession = errors.New("sign in first")
	errLinkEntra     = errors.New("linking a device needs a Microsoft sign-in")
)

// linkApproverOf is the signed-in user behind r: with Entra sign-in on the
// hub, only sessions a Microsoft sign-in opened (a password login could be
// a shared admin account, and the approver becomes the owner); without,
// any panel login of a Longarm user.
func (h *Hub) linkApproverOf(r *http.Request) (*linkApprover, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, errLinkNoSession
	}
	a := &linkApprover{sessHash: sessionHash(c.Value)}
	if a.login, err = h.store.WebSession(a.sessHash, time.Now()); err != nil {
		return nil, errLinkNoSession
	}
	if h.panelDisabled(a.login) {
		return nil, errors.New("this user is disabled")
	}
	u, err := h.store.GetUser(a.login)
	if err != nil {
		return nil, errLinkNoSession
	}
	if h.entra != nil {
		if u.Entra == "" || !h.entraMarked(r, a.sessHash) {
			return nil, errLinkEntra
		}
		a.user, a.id.oid = u.Tenant, u.Entra
		h.mu.Lock()
		if t := h.tenants[a.user]; t != nil {
			a.id.upn = t.u.Upn
		}
		h.mu.Unlock()
		return a, nil
	}
	a.user, a.admin = u.Tenant, u.Admin()
	h.mu.Lock()
	if a.user == "" && h.tenants[a.login] != nil { // an administrator's login named like a user
		a.user = a.login
	}
	known := h.tenants[a.user] != nil
	h.mu.Unlock()
	if a.user == "" || !known {
		return nil, errors.New("the panel account " + a.login + " is not a Longarm user: sign in as the user the device is for (longarm admin user add <name> --panel)")
	}
	return a, nil
}

// approveLink enrolls l for a: as a's device or as a's own worker.
func (h *Hub) approveLink(a *linkApprover, l *pairLink) (*pb.JoinResponse, error) {
	who, user, admin := "web/"+a.login, a.user, a.admin
	if h.entra != nil {
		// A sign-in's rules, applied now: allowed list, disabled users, admins.
		u, adm, err := h.entraSignIn(a.id, "link")
		if err != nil {
			return nil, err
		}
		if u != user {
			return nil, status.Error(codes.PermissionDenied, "your sign-in no longer matches this panel session; sign in again")
		}
		who, admin = "entra/"+a.id.upn, adm
	}
	h.mu.Lock()
	ok := h.tenants[user] != nil && !h.userDisabledLocked(user)
	h.mu.Unlock()
	if !ok {
		return nil, status.Errorf(codes.PermissionDenied, "user %s is disabled or unknown", user)
	}
	dev := deviceRequest{csr: l.csr, name: l.name, hostname: l.hostname, os: l.os, arch: l.arch, version: l.version}
	if l.kind == linkAdmin {
		if dev.name == "" {
			dev.name = adminWorkerName(l, nil)
		}
		dev.admin = true
		aw, err := h.enrollOwnedWorker(who, user, dev)
		if err == nil {
			h.pairAdminWorker(aw.Name, "")
		}
		return aw, err
	}
	resp, err := h.approveLinkParts(who, user, admin, l, dev)
	if err != nil || !l.admin {
		return resp, err
	}
	// The admin worker last, all or nothing: a failure takes the others back.
	aname := l.adminName
	if aname == "" {
		aname = adminWorkerName(l, resp)
	}
	adev := dev
	adev.csr, adev.name, adev.labels, adev.admin = l.adminCSR, aname, nil, true
	aw, err := h.enrollOwnedWorker(who, user, adev)
	if err != nil {
		for _, r := range []*pb.JoinResponse{resp.Worker, resp} {
			if r == nil {
				continue
			}
			if _, rerr := h.Revoke(who+" (pairing failed)", r.Name); rerr != nil {
				h.log.Warn("pairing: revoking a node of a failed approval", "node", r.Name, "err", rerr)
			}
		}
		return nil, status.Errorf(status.Code(err), "the admin worker: %s", errMessage(err))
	}
	resp.AdminWorker = aw
	base := ""
	switch {
	case resp.Worker != nil:
		base = resp.Worker.Name
	case l.kind == linkWorker:
		base = resp.Name
	}
	h.pairAdminWorker(aw.Name, base)
	return resp, nil
}

// pairAdminWorker records the admin worker admin and the session worker of
// its machine as companions (store.Node.Companion, both ways): worker, the
// one enrolled with it, else the owner's live worker on the same host (by
// name <worker>-admin first). Nothing is recorded when none fits.
func (h *Hub) pairAdminWorker(admin, worker string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.nodes[admin]
	if a == nil || !a.rec.Admin {
		return
	}
	aw := machineWorker(a.rec)
	fits := func(n *node) bool {
		return n != nil && n.rec.Companion == "" && machine.Match(machineWorker(n.rec), aw) && a.rec.Hostname != "" &&
			strings.EqualFold(n.rec.Hostname, a.rec.Hostname)
	}
	var s *node
	switch {
	case worker != "":
		if s = h.nodes[worker]; s == nil || !machine.Match(machineWorker(s.rec), aw) {
			return
		}
	case fits(h.nodes[strings.TrimSuffix(admin, machine.AdminSuffix)]):
		s = h.nodes[strings.TrimSuffix(admin, machine.AdminSuffix)]
	default:
		var names []string
		for name, n := range h.nodes {
			if fits(n) {
				names = append(names, name)
			}
		}
		if len(names) != 1 { // none, or several: ambiguous
			return
		}
		s = h.nodes[names[0]]
	}
	for _, p := range [][2]*node{{a, s}, {s, a}} {
		rec := *p[0].rec
		rec.Companion = p[1].rec.Name
		if err := h.store.PutNode(&rec); err != nil {
			h.log.Warn("pairing: recording the companion worker", "node", rec.Name, "err", err)
			return
		}
		p[0].rec = &rec
	}
	// no names: the session worker's event reaches whoever may use it
	// (eventFilter); its admin worker is its owner's business
	h.publishNodeLocked(s, "paired with its machine's admin worker")
	h.publishNodeLocked(a, "paired with its machine's worker")
}

// adminWorkerName is an admin worker's default name: <the machine's
// worker name, else its host name>-admin.
func adminWorkerName(l *pairLink, resp *pb.JoinResponse) string {
	base := ""
	switch {
	case resp != nil && resp.Worker != nil:
		base = resp.Worker.Name
	case resp != nil && l.kind == linkWorker:
		base = resp.Name
	default:
		base = sanitizeName(l.hostname)
	}
	const suffix = "-admin"
	if len(base)+len(suffix) > 63 {
		base = strings.TrimRight(base[:63-len(suffix)], "-._")
	}
	return base + suffix
}

// approveLinkParts enrolls the device and / or worker of l.
func (h *Hub) approveLinkParts(who, user string, admin bool, l *pairLink, dev deviceRequest) (*pb.JoinResponse, error) {
	switch l.kind {
	case linkWorker:
		dev.labels = l.workerLabels
		return h.enrollOwnedWorker(who, user, dev) // never an admin
	case linkBoth:
		// The worker first: its name may be taken (the device's never is,
		// a device of the same user takes its name over).
		wdev := dev
		wdev.csr, wdev.name, wdev.labels = l.workerCSR, l.workerName, l.workerLabels
		w, err := h.enrollOwnedWorker(who, user, wdev)
		if err != nil {
			return nil, err
		}
		resp, _, err := h.enrollDevice(who, user, admin, dev)
		if err != nil {
			// all or nothing: the machine never collects the worker alone
			if _, rerr := h.Revoke(who+" (pairing failed)", w.Name); rerr != nil {
				h.log.Warn("pairing: revoking the worker of a failed approval", "node", w.Name, "err", rerr)
			}
			return nil, err
		}
		resp.Worker = w
		return resp, nil
	}
	resp, _, err := h.enrollDevice(who, user, admin, dev)
	return resp, err
}

const linkCSP = "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

type linkView struct {
	User, UPN, Error, Done, Token string
	SignIn                        string // a sign-in link to offer
	L                             *linkDetails
}

type linkDetails struct {
	ID, Kind, Name, Hostname, OSArch, Version, IP, KeyID, Ago, Left string
	Worker, Admin                                                   bool
	// Both: a client device and a worker; Name / KeyID are the device's
	Both                    bool
	WorkerName, WorkerKeyID string
	WorkerLabels            string // "k=v, k2=v2"
	// AdminWorker: the pairing includes an admin worker (runs as SYSTEM)
	AdminWorker                 bool
	AdminWorkerName, AdminKeyID string
}

func (h *Hub) renderLink(w http.ResponseWriter, code int, v linkView) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", linkCSP)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	linkTmpl.Execute(w, v)
}

// linkSignIn answers a /link request without a usable session: off to
// Microsoft (back to /link afterwards), or the password login.
func (h *Hub) linkSignIn(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case !errors.Is(err, errLinkNoSession) && !errors.Is(err, errLinkEntra):
		h.renderLink(w, http.StatusForbidden, linkView{Error: err.Error()})
	case h.entraWeb() != "" && r.Method == http.MethodGet && topLevelNavigation(r):
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, "/auth/login?next=/link", http.StatusFound)
	case h.entraWeb() != "":
		h.renderLink(w, http.StatusUnauthorized, linkView{Error: errLinkEntra.Error() + ".", SignIn: "/auth/login?next=/link"})
	case h.entra != nil:
		h.renderLink(w, http.StatusForbidden, linkView{Error: "This hub's Microsoft sign-in for the panel is off: link devices with az (--az) or a join code."})
	default:
		h.renderLink(w, http.StatusUnauthorized, linkView{Error: "Sign in to the panel first, then come back to /link.", SignIn: "/status"})
	}
}

func (h *Hub) linkPage(w http.ResponseWriter, r *http.Request) {
	a, err := h.linkApproverOf(r)
	if err != nil {
		h.linkSignIn(w, r, err)
		return
	}
	// A code in the URL is ignored on purpose: it must be typed.
	h.renderLink(w, http.StatusOK, linkView{User: a.user, UPN: a.id.upn, Token: h.linkFormToken(a.sessHash, "enter", "")})
}

// linkPost checks a /link form post: from the hub's own page, of a
// signed-in approver, with the form's token for step / id.
func (h *Hub) linkPost(w http.ResponseWriter, r *http.Request, step string, id func() string) (*linkApprover, bool) {
	if !h.sameSitePost(r) {
		h.renderLink(w, http.StatusForbidden, linkView{Error: "Cross-origin request refused."})
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	a, err := h.linkApproverOf(r)
	if err != nil {
		h.linkSignIn(w, r, err)
		return nil, false
	}
	if !h.checkLinkFormToken(r.PostFormValue("token"), a.sessHash, step, id()) {
		h.renderLink(w, http.StatusForbidden, linkView{User: a.user, Error: "This form expired or was not made for you. Start again at /link."})
		return nil, false
	}
	return a, true
}

func (h *Hub) linkLookup(w http.ResponseWriter, r *http.Request) {
	a, ok := h.linkPost(w, r, "enter", func() string { return "" })
	if !ok {
		return
	}
	v := linkView{User: a.user, UPN: a.id.upn, Token: h.linkFormToken(a.sessHash, "enter", "")}
	// per address and per user: guessing codes would enroll others' machines as one's own
	if !h.pair.codeLimit.allow(h.clientIP(r)) || !h.pair.codeLimit.allow("user/"+a.user) {
		v.Error = errTooMany.Error()
		h.renderLink(w, http.StatusTooManyRequests, v)
		return
	}
	code, valid := normalizeLinkCode(r.PostFormValue("code")) // the form only, never the URL
	now := time.Now()
	p := h.pair
	p.mu.Lock()
	expired := p.sweepLocked(now)
	l := p.byCode[code]
	var d *linkDetails
	if valid && l != nil && l.state == linkPending {
		d = &linkDetails{ID: l.id, Kind: l.kind, Name: l.name, Hostname: l.hostname, OSArch: l.os + "/" + l.arch,
			Version: l.version, IP: l.ip, KeyID: l.keyID, Ago: now.Sub(l.started).Round(time.Second).String(),
			Left: l.expires.Sub(now).Round(time.Second).String(), Worker: l.kind == linkWorker,
			Both: l.kind == linkBoth, WorkerName: l.workerName, WorkerKeyID: l.workerKeyID,
			WorkerLabels: labelList(l.workerLabels), AdminWorker: l.admin, AdminWorkerName: l.adminName, AdminKeyID: l.adminKeyID}
	}
	p.mu.Unlock()
	h.auditExpired(expired)
	if d == nil {
		h.log.Info("pairing lookup: no such code", "user", a.user, "remote", h.clientIP(r))
		v.Error = "No device is waiting with this code. Check it, or start again on the device (codes last 10 minutes)."
		h.renderLink(w, http.StatusNotFound, v)
		return
	}
	if d.Name == "" {
		d.Name = "(default, from the host name)"
	}
	if d.Both && d.WorkerName == "" {
		d.WorkerName = "(default: the host name)"
	}
	if d.AdminWorker && d.AdminWorkerName == "" {
		d.AdminWorkerName = "(default: <worker name>-admin)"
		if d.Kind == linkAdmin {
			d.AdminWorkerName = "(default: <host name>-admin)"
		}
	}
	if d.Kind == linkAdmin {
		d.Name = d.AdminWorkerName
	}
	// an admin certificate goes to client devices only
	d.Admin = !d.Worker && d.Kind != linkAdmin && h.linkAdmin(a)
	v.L, v.Token = d, h.linkFormToken(a.sessHash, "decide", d.ID)
	h.renderLink(w, http.StatusOK, v)
}

// linkAdmin: would a client approved by a get an admin certificate?
func (h *Hub) linkAdmin(a *linkApprover) bool {
	if h.entra != nil {
		return entraMatchAny(h.cfg.Auth.Entra.Admins, a.id.upn, a.id.oid)
	}
	return a.admin
}

func (h *Hub) linkDecide(w http.ResponseWriter, r *http.Request) {
	a, ok := h.linkPost(w, r, "decide", func() string { return r.PostFormValue("id") })
	if !ok {
		return
	}
	id, decision := r.PostFormValue("id"), r.PostFormValue("decision")
	v := linkView{User: a.user, UPN: a.id.upn}
	if decision != "approve" && decision != "deny" {
		v.Error = "Choose Approve or Deny."
		h.renderLink(w, http.StatusBadRequest, v)
		return
	}
	p := h.pair
	p.mu.Lock()
	expired := p.sweepLocked(time.Now())
	var l *pairLink
	for _, x := range p.byPoll {
		if x.id == id {
			l = x
		}
	}
	if l == nil || l.state != linkPending {
		p.mu.Unlock()
		h.auditExpired(expired)
		v.Error = "This device is no longer waiting (the code expired, or it was approved or denied already)."
		h.renderLink(w, http.StatusGone, v)
		return
	}
	delete(p.byCode, l.code) // single use, whatever the outcome
	if decision == "deny" {
		l.state, l.message = linkDenied, "the pairing was denied"
		l.drop()
	} else {
		l.state = linkApproving
	}
	p.mu.Unlock()
	h.auditExpired(expired)
	who := "web/" + a.login
	if a.id.upn != "" {
		who = "entra/" + a.id.upn
	}
	fields := l.fields()
	fields["user"] = a.user
	fields["remote"] = h.clientIP(r)
	if decision == "deny" {
		h.auditHub(who, "link/deny", fields)
		h.log.Info("pairing denied", "id", l.id, "kind", l.kind, "by", a.user, "remote", l.ip)
		v.Done = "Denied: the device was told so."
		h.renderLink(w, http.StatusOK, v)
		return
	}
	resp, err := h.approveLink(a, l)
	p.mu.Lock()
	if err != nil {
		l.state, l.message = linkDenied, "the approval failed: "+errMessage(err)
		l.drop()
	} else {
		l.state, l.resp = linkApproved, resp
		l.drop()
		if c, err := pki.ParseCertPEM(resp.CertPem); err == nil {
			l.serial = pki.Serial(c)
		}
		if resp.Worker != nil {
			if c, err := pki.ParseCertPEM(resp.Worker.CertPem); err == nil {
				l.workerSerial = pki.Serial(c)
			}
		}
		if resp.AdminWorker != nil {
			if c, err := pki.ParseCertPEM(resp.AdminWorker.CertPem); err == nil {
				l.adminSerial = pki.Serial(c)
			}
		}
	}
	p.mu.Unlock()
	fields["error"] = errString(err)
	if resp != nil {
		fields["node"], fields["role"] = resp.Name, resp.Role
		if resp.Worker != nil {
			fields["worker"] = resp.Worker.Name
		}
		if resp.AdminWorker != nil {
			fields["admin_worker"] = resp.AdminWorker.Name
		} else if l.kind == linkAdmin {
			fields["admin_worker"] = resp.Name
		}
	}
	h.auditHub(who, "link/approve", fields)
	if err != nil {
		h.log.Warn("pairing approval failed", "id", l.id, "kind", l.kind, "by", a.user, "err", err)
		v.Error = "The device could not be enrolled: " + errMessage(err)
		h.renderLink(w, http.StatusConflict, v)
		return
	}
	h.log.Info("pairing approved", "id", l.id, "kind", l.kind, "node", resp.Name, "role", resp.Role, "by", a.user, "remote", l.ip)
	switch l.kind {
	case linkWorker:
		v.Done = "Approved: worker " + resp.Name + " is yours. Only you can use it until you share it."
	case linkAdmin:
		v.Done = "Approved:"
	case linkBoth:
		v.Done = "Approved: device " + resp.Name + " is signed in as " + a.user + " (" + resp.Role + "), and worker " +
			resp.Worker.Name + " is yours. Only you can use the worker until you share it."
	default:
		v.Done = "Approved: device " + resp.Name + " is signed in as " + a.user + " (" + resp.Role + ")."
	}
	aw := resp.AdminWorker
	if l.kind == linkAdmin {
		aw = resp
	}
	if aw != nil {
		v.Done += " Admin worker " + aw.Name + " is yours alone: it runs your commands as SYSTEM (an administrator of " +
			l.hostname + "), only you can use it, it can't be shared and jobs are never placed on it automatically."
	}
	h.renderLink(w, http.StatusOK, v)
}

var linkTmpl = template.Must(template.New("link").Parse(`<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width"><title>longarm-hub: link a device</title>
<style>body{font:15px system-ui,sans-serif;margin:2em;max-width:42em}td,th{padding:4px 12px 4px 0;text-align:left;vertical-align:top}
.err{color:#b00}.warn{border:1px solid #c80;background:#fff4e0;color:#000;padding:8px 12px}code,input{font-family:ui-monospace,monospace}
input[name=code]{font-size:1.4em;letter-spacing:.1em;width:9em;text-transform:uppercase}button{font-size:1em;padding:6px 16px;margin-right:8px}</style>
</head><body><h1>Link a device</h1>
{{if .User}}<p>Signed in as <b>{{.User}}</b>{{if .UPN}} ({{.UPN}}){{end}}.</p>{{end}}
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if .SignIn}}<p><a href="{{.SignIn}}">Sign in</a></p>{{end}}
{{if .Done}}<p><b>{{.Done}}</b></p><p>You can close this page.</p>
{{else if .L}}{{with .L}}<p>A device asks to be linked to your account. Check that it is the one in front of you:</p>
{{if .AdminWorker}}<p class="warn"><b>This includes an ADMINISTRATOR worker</b>, <code>{{.AdminWorkerName}}</code>: it runs commands as <b>SYSTEM</b> (a local administrator) on <code>{{.Hostname}}</code> - it can install software and change anything on that machine. Only you will be able to use it; it can't be shared and jobs are never placed on it automatically.</p>{{end}}
{{if .Both}}<p>Approving does <b>two</b> things for this machine: it becomes your <b>client device</b> (acts as you) <b>and</b> your own <b>worker</b> (runs your jobs).</p>{{end}}
<table><tr><th>kind</th><td>{{if eq .Kind "admin"}}<b>admin worker</b> (runs commands as SYSTEM){{else if .Both}}client device (acts as you) + worker (runs jobs){{else if .Worker}}worker (runs jobs){{else}}client device (acts as you){{end}}{{if and .AdminWorker (ne .Kind "admin")}} + <b>admin worker</b> (runs commands as SYSTEM){{end}}</td></tr>
<tr><th>{{if .Both}}device name{{else}}name{{end}}</th><td><code>{{.Name}}</code></td></tr>
{{if .Both}}<tr><th>worker name</th><td><code>{{.WorkerName}}</code></td></tr>{{end}}
{{if .AdminWorker}}<tr><th>admin worker (SYSTEM)</th><td><code>{{.AdminWorkerName}}</code></td></tr>{{end}}
{{if .WorkerLabels}}<tr><th>worker labels</th><td><code>{{.WorkerLabels}}</code></td></tr>{{end}}
<tr><th>host name</th><td><code>{{.Hostname}}</code></td></tr>
<tr><th>system</th><td>{{.OSArch}}, version {{.Version}}</td></tr>
<tr><th>address</th><td><code>{{.IP}}</code> (as the hub saw it)</td></tr>
<tr><th>{{if .Both}}device node key{{else}}node key{{end}}</th><td>{{if .KeyID}}<code>{{.KeyID}}</code> (compare: longarm trust show{{if not .Both}} / longarmd trust status{{end}}){{else}}none{{end}}</td></tr>
{{if .Both}}<tr><th>worker node key</th><td>{{if .WorkerKeyID}}<code>{{.WorkerKeyID}}</code> (compare: longarmd trust status){{else}}none{{end}}</td></tr>{{end}}
{{if .AdminWorker}}<tr><th>admin worker node key</th><td>{{if .AdminKeyID}}<code>{{.AdminKeyID}}</code>{{else}}none{{end}}</td></tr>{{end}}
<tr><th>started</th><td>{{.Ago}} ago (expires in {{.Left}})</td></tr></table>
{{if .Both}}<p class="warn">Only confirm if this is your device: it gets a certificate that acts as you{{if .Admin}} - an <b>administrator</b> certificate{{end}}, and it becomes your worker: jobs you run may be placed on it.</p>
{{else if .Worker}}<p class="warn">Only confirm if this is your machine: jobs you run may be placed on it.</p>
{{else if eq .Kind "admin"}}
{{else}}<p class="warn">Only confirm if this is your device: it gets a certificate that acts as you{{if .Admin}} - an <b>administrator</b> certificate{{end}}.</p>{{end}}
{{if .AdminWorker}}<p class="warn">Approve only if you set this machine up yourself and want to run <b>administrator</b> commands on it remotely.</p>{{end}}
{{end}}<form method="post" action="/link/decide"><input type="hidden" name="token" value="{{.Token}}"><input type="hidden" name="id" value="{{.L.ID}}">
<p><button name="decision" value="approve">Approve</button><button name="decision" value="deny">Deny</button></p></form>
{{else if .Token}}<p>Enter the code that <code>longarm login</code> or <code>longarmd login</code> shows on the device.</p>
<form method="post" action="/link"><input type="hidden" name="token" value="{{.Token}}">
<p><input name="code" placeholder="XXXX-XXXX" maxlength="12" autocomplete="off" autocapitalize="characters" spellcheck="false" required autofocus>
<button>Continue</button></p></form>
<p>Only enter a code from a device you are setting up yourself - never one someone sent you.</p>{{end}}
</body></html>`))

// labelList renders labels for the /link page: "k=v, k2=v2", sorted.
func labelList(m map[string]string) string {
	var out []string
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
