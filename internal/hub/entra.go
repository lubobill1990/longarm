package hub

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/nodeid"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// maxAutoDeviceName bounds a derived device name (<user>-<host>).
const maxAutoDeviceName = 56

// Sign-in with Microsoft Entra ID (docs/usage.md, docs/security.md): the
// hub vouches for a user after verifying a Microsoft token. `longarm
// login` sends the access token of the user's `az login` (Enroll.EntraJoin)
// and gets a certificate right away; the web panel signs in with OpenID
// Connect (entra_web.go). Either way the Entra identity (its object ID)
// maps to a Longarm user, created on first sign-in.

// AuthConfig is [auth]: sign-in with identity providers - Entra (work
// and school accounts of one tenant) and personal accounts (idp.go).
type AuthConfig struct {
	Entra     EntraConfig `toml:"entra"`
	Microsoft IdPConfig   `toml:"microsoft"` // personal Microsoft accounts
	Google    IdPConfig   `toml:"google"`
	Apple     IdPConfig   `toml:"apple"`
}

func (a *AuthConfig) applyDefaults() error {
	if err := a.Entra.applyDefaults(); err != nil {
		return err
	}
	for name, c := range a.idps() {
		if err := c.applyDefaults(name); err != nil {
			return err
		}
	}
	return nil
}

// EntraConfig is [auth.entra]. Off unless tenant_id and client_id are set.
type EntraConfig struct {
	TenantID string `toml:"tenant_id"` // directory (tenant) ID, a GUID
	// ClientID is the app registration's application ID: the audience of
	// the access tokens and the client of the web sign-in.
	ClientID string `toml:"client_id"`
	// Scope is the delegated permission access tokens must carry (default
	// "longarm.connect"; clients ask for api://<client id>/<scope>).
	Scope string `toml:"scope"`
	// Admins get admin certificates and the panel's admin role; Allowed
	// (empty = everyone in the tenant) may sign in at all. Entries: a UPN
	// "user@example.com", "*@example.com" or an object ID; case-insensitive.
	Admins  []string `toml:"admins"`
	Allowed []string `toml:"allowed"`
	// Apps lets applications sign in with app-only tokens (CI: a GitHub
	// Actions OIDC token federated into a client-credentials token for
	// api://<client id>/.default), as "<app id>=<longarm user>": the app
	// acts as that user (client certificates only; `longarm login`, never
	// the web panel).
	Apps []string `toml:"apps"`
	// Web is the panel's sign-in: "confidential" (authorization code
	// redeemed by the hub with its client credential; Web platform
	// redirect URI <public_url>/auth/callback), "spa" (redeemed by the
	// browser without a credential; SPA platform redirect URI
	// <public_url>/auth/spa-callback) or "off". Default: confidential with
	// a client credential, else spa.
	Web string `toml:"web"`
	// The confidential web sign-in's client credential: "managed-identity"
	// (a token of the hub's managed identity - AZURE_CLIENT_ID - that the
	// app registration trusts as a federated credential) or a client
	// secret read from ClientSecretFile.
	ClientAssertion  string `toml:"client_assertion"`
	ClientSecretFile string `toml:"client_secret_file"`
	// Authority is the Entra endpoint (default
	// https://login.microsoftonline.com; national clouds have their own).
	Authority string `toml:"authority"`

	apps map[string]string // app ID -> user (Apps)
}

// Web sign-in modes ([auth.entra] web).
const (
	EntraWebConfidential = "confidential"
	EntraWebSPA          = "spa"
	EntraWebOff          = "off"
)

// Enabled reports whether Entra sign-in is configured.
func (e *EntraConfig) Enabled() bool { return e.TenantID != "" && e.ClientID != "" }

// FullScope is what clients request: api://<client id>/<scope>.
func (e *EntraConfig) FullScope() string {
	if !e.Enabled() {
		return ""
	}
	return "api://" + e.ClientID + "/" + e.Scope
}

func (e *EntraConfig) issuer() string { return e.Authority + "/" + e.TenantID + "/v2.0" }

func (e *EntraConfig) endpoint(path string) string { return e.Authority + "/" + e.TenantID + path }

var (
	guidRE       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	entraScopeRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	upnEntryRE   = regexp.MustCompile(`^[a-z0-9._%+'-]+@[a-z0-9-]+(\.[a-z0-9-]+)+$`)
	domainRE     = regexp.MustCompile(`^\*@[a-z0-9-]+(\.[a-z0-9-]+)+$`)
)

func (e *EntraConfig) applyDefaults() error {
	e.TenantID, e.ClientID = strings.TrimSpace(e.TenantID), strings.TrimSpace(e.ClientID)
	if e.TenantID == "" && e.ClientID == "" {
		return nil
	}
	// GUIDs, not domain names: tokens carry the tenant's ID in iss and tid.
	if !guidRE.MatchString(e.TenantID) || !guidRE.MatchString(e.ClientID) {
		return errors.New("auth.entra: tenant_id and client_id must both be set, as GUIDs")
	}
	e.TenantID, e.ClientID = strings.ToLower(e.TenantID), strings.ToLower(e.ClientID)
	if e.Scope == "" {
		e.Scope = "longarm.connect"
	}
	if !entraScopeRE.MatchString(e.Scope) {
		return fmt.Errorf("auth.entra: invalid scope %q (just its name, e.g. longarm.connect)", e.Scope)
	}
	for _, l := range [][]string{e.Admins, e.Allowed} {
		for _, x := range l {
			if !validEntraEntry(x) {
				return fmt.Errorf("auth.entra: invalid entry %q (user@domain, *@domain or an object ID)", x)
			}
		}
	}
	if e.ClientAssertion != "" && e.ClientAssertion != "managed-identity" {
		return fmt.Errorf("auth.entra: client_assertion %q: want \"managed-identity\" or nothing", e.ClientAssertion)
	}
	if e.ClientAssertion != "" && e.ClientSecretFile != "" {
		return errors.New("auth.entra: client_assertion and client_secret_file exclude each other")
	}
	if e.Web == "" {
		e.Web = EntraWebSPA
		if e.ClientAssertion != "" || e.ClientSecretFile != "" {
			e.Web = EntraWebConfidential
		}
	}
	if e.Web != EntraWebConfidential && e.Web != EntraWebSPA && e.Web != EntraWebOff {
		return fmt.Errorf("auth.entra: web %q: want confidential, spa or off", e.Web)
	}
	e.apps = map[string]string{}
	for _, a := range e.Apps {
		id, user, ok := strings.Cut(strings.TrimSpace(a), "=")
		if id, user = strings.ToLower(strings.TrimSpace(id)), strings.TrimSpace(user); !ok || !guidRE.MatchString(id) || !pki.ValidUser(user) {
			return fmt.Errorf("auth.entra: apps entry %q: want <app id>=<longarm user>", a)
		}
		e.apps[id] = user
	}
	e.Authority = strings.TrimRight(strings.TrimSpace(e.Authority), "/")
	if e.Authority == "" {
		e.Authority = "https://login.microsoftonline.com"
	}
	u, err := url.Parse(e.Authority)
	if err != nil || u.Host == "" || u.Path != "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname()))) {
		return fmt.Errorf("auth.entra: authority %q: want https://<host>", e.Authority)
	}
	return nil
}

// isLoopbackHost: plain http authorities are for local tests only.
func isLoopbackHost(h string) bool {
	ip := net.ParseIP(h)
	return h == "localhost" || ip != nil && ip.IsLoopback()
}

// validEntraEntry: "user@domain", "*@domain" or an object ID (GUID).
func validEntraEntry(s string) bool {
	e := strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.HasPrefix(e, "*@"):
		return domainRE.MatchString(e)
	case strings.Contains(e, "@"):
		return upnEntryRE.MatchString(e)
	}
	return guidRE.MatchString(e)
}

// entraMatch reports whether a user (UPN, object ID) matches an admins /
// allowed entry.
func entraMatch(entry, upn, oid string) bool {
	e := strings.ToLower(strings.TrimSpace(entry))
	u, o := strings.ToLower(upn), strings.ToLower(oid)
	switch {
	case e == "":
		return false
	case strings.HasPrefix(e, "*@"):
		return len(e) > 2 && u != "" && strings.HasSuffix(u, e[1:])
	case strings.Contains(e, "@"):
		return u == e
	}
	return o != "" && o == e
}

func entraMatchAny(list []string, upn, oid string) bool {
	return slices.ContainsFunc(list, func(e string) bool { return entraMatch(e, upn, oid) })
}

func constantEqual(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// entraState is the hub's Entra machinery (nil when not configured).
type entraState struct {
	keys *jwks

	mu sync.Mutex
	mi cachedToken // managed identity token (client assertion)
}

func newEntraState(cfg *EntraConfig) *entraState {
	if !cfg.Enabled() {
		return nil
	}
	return &entraState{keys: newJWKS(cfg.endpoint("/discovery/v2.0/keys"))}
}

// oidcWeb is the web sign-ins' state, whatever the provider.
type oidcWeb struct {
	mu        sync.Mutex
	usedState map[string]time.Time // consumed web sign-in states (single use)
	cookieKey []byte               // web sign-in state MAC key (oidcKey)
}

// entraIdentity is a verified user of an identity provider: an Entra user
// (provider "": oid is its object ID, upn its sign-in name) or a personal
// account (provider "google", "apple", "microsoft": oid is the provider's
// subject, upn its verified e-mail address or "").
type entraIdentity struct {
	provider string
	oid, upn string
}

func (c *entraClaims) identity() entraIdentity {
	return entraIdentity{oid: c.Oid, upn: c.SignInName()}
}

// verifyAccessToken checks an access token for this hub's app: a user's
// (with the scope) or a listed app's (app-only).
func (h *Hub) verifyAccessToken(ctx context.Context, token string) (*entraClaims, error) {
	e := &h.cfg.Auth.Entra
	return verifyEntraToken(ctx, h.entra.keys, token, tokenCheck{issuer: e.issuer(), tenant: e.TenantID,
		audiences: []string{e.ClientID, "api://" + e.ClientID}, scope: e.Scope, apps: e.apps, appAud: e.ClientID}, time.Now())
}

// entraAppUser is the Longarm user an app signs in as ([auth.entra] apps),
// created on first use; refused when disabled or when the name belongs to
// something else.
func (h *Hub) entraAppUser(app, oid string) (string, error) {
	user := h.cfg.Auth.Entra.apps[app]
	h.mu.Lock()
	if t := h.tenants[user]; t != nil {
		disabled := h.userDisabledLocked(user)
		h.mu.Unlock()
		if disabled {
			return "", status.Errorf(codes.PermissionDenied, "user %s is disabled", user)
		}
		return user, nil
	}
	if h.freeUserNameLocked(user) != user {
		h.mu.Unlock()
		return "", status.Errorf(codes.FailedPrecondition, "auth.entra apps: the name %s is taken (a worker, device or panel account)", user)
	}
	u := &pb.TenantUser{Name: user, CreatedAt: tspb(time.Now().UTC()), CreatedBy: "entra-app"}
	if err := h.saveTenantLocked(u); err != nil {
		h.mu.Unlock()
		return "", status.Error(codes.Internal, err.Error())
	}
	h.tenants[user] = &tenant{u: u}
	h.tenancyChangedLocked()
	h.mu.Unlock()
	h.auditHub("entra-app/"+app, "users/add", map[string]any{"user": user, "app": app, "oid": oid})
	h.log.Info("user created by entra app sign-in", "user", user, "app", app)
	return user, nil
}

// entraOIDKey is the meta key mapping an identity key (an Entra object ID,
// "<provider>:<subject>": entraIdentity.key) to its user; entraUserKey
// binds the user to that identity (the user record's immutable identity,
// kept next to the protojson record in the store). A sign-in only lands
// on a user when both agree. (Named after Entra, the first provider.)
func entraOIDKey(key string) string {
	if !strings.Contains(key, ":") { // object IDs; subjects are case-sensitive
		key = strings.ToLower(key)
	}
	return "entra_oid:" + key
}
func entraUserKey(user string) string { return "entra_user:" + user }

// entraForgetLocked drops user's Entra binding (RemoveUser): its name may
// go to someone else later, never back through a stale mapping.
func (h *Hub) entraForgetLocked(user string) {
	if oid, _ := h.store.Meta(entraUserKey(user)); oid != "" {
		if u, _ := h.store.Meta(entraOIDKey(oid)); u == user {
			h.store.DeleteMeta(entraOIDKey(oid))
		}
		h.store.DeleteMeta(entraUserKey(user))
	}
}

// entraSignIn maps a verified user of an identity provider to its Longarm
// user - created on first sign-in, named after the UPN / e-mail address -
// and says whether it is one of the provider's admins. Refused: users
// outside the provider's allowed list, and disabled users. by names the
// path ("longarm login", "web") for the audit.
func (h *Hub) entraSignIn(id entraIdentity, by string) (user string, admin bool, err error) {
	admin, allowed := h.idpPolicy(id)
	if !allowed {
		h.log.Warn("sign-in refused: not in the allowed list", "provider", cmpOr(id.provider, "entra"), "upn", id.upn, "oid", id.oid, "via", by)
		return "", false, status.Errorf(codes.PermissionDenied, "%s may not sign in to this hub", cmpOr(id.upn, id.who()))
	}
	key := id.key()
	h.mu.Lock()
	user, err = h.store.Meta(entraOIDKey(key))
	if err != nil {
		h.mu.Unlock()
		return "", false, status.Error(codes.Internal, err.Error())
	}
	if user != "" {
		bound, err := h.store.Meta(entraUserKey(user))
		if err != nil {
			h.mu.Unlock()
			return "", false, status.Error(codes.Internal, err.Error())
		}
		if h.tenants[user] == nil || bound != key { // removed, or its name went to someone else
			h.log.Warn("sign-in: stale user mapping dropped", "user", user, "identity", key, "via", by)
			h.store.DeleteMeta(entraOIDKey(key))
			user = ""
		}
	}
	created := false
	if t := h.tenants[user]; user != "" && t != nil {
		if h.userDisabledLocked(user) {
			h.mu.Unlock()
			h.log.Warn("entra sign-in refused: user disabled", "user", user, "upn", id.upn, "via", by)
			return "", false, status.Errorf(codes.PermissionDenied, "user %s is disabled", user)
		}
		if id.upn != "" && t.u.Upn != id.upn { // renamed in Entra
			u := proto.Clone(t.u).(*pb.TenantUser)
			u.Upn = id.upn
			if err := h.saveTenantLocked(u); err == nil {
				t.u = u
			}
		}
	} else {
		// First sign-in, or the user was removed since (a new user then).
		base := entraUserName(id.upn)
		if id.upn == "" && id.provider != "" {
			base = id.provider // no e-mail address to name it after
		}
		user = h.freeUserNameLocked(base)
		u := &pb.TenantUser{Name: user, CreatedAt: tspb(time.Now().UTC()), CreatedBy: cmpOr(id.provider, "entra"), Upn: id.upn}
		if err := h.saveTenantLocked(u); err != nil {
			h.mu.Unlock()
			return "", false, status.Error(codes.Internal, err.Error())
		}
		if err := h.store.SetMeta(entraUserKey(user), key); err != nil {
			h.mu.Unlock()
			return "", false, status.Error(codes.Internal, err.Error())
		}
		if err := h.store.SetMeta(entraOIDKey(key), user); err != nil {
			h.mu.Unlock()
			return "", false, status.Error(codes.Internal, err.Error())
		}
		h.tenants[user] = &tenant{u: u}
		h.tenancyChangedLocked()
		created = true
	}
	h.mu.Unlock()
	if created {
		h.auditHub(id.who(), "users/add", map[string]any{"user": user, "upn": id.upn, "oid": id.oid, "provider": cmpOr(id.provider, "entra"), "via": by})
		h.log.Info("user created by sign-in", "user", user, "provider", cmpOr(id.provider, "entra"), "upn", id.upn, "oid", id.oid, "via", by)
	}
	return user, admin, nil
}

// entraUserName derives a user name from a UPN's local part: lower case,
// [a-z0-9_-], starting with a letter, at most 17 characters (pki.ValidUser).
func entraUserName(upn string) string {
	local, _, _ := strings.Cut(strings.ToLower(upn), "@")
	var b strings.Builder
	for _, r := range local {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		s = "user"
	}
	if s[0] < 'a' || s[0] > 'z' {
		s = "u" + s
	}
	if len(s) > 17 {
		s = s[:17]
	}
	return s
}

// freeUserNameLocked is base, or base2, base3 ... (shortened to stay a
// valid user name): the first name no user, purged user, worker, device
// identity or other panel account uses - a legacy client certificate named
// alice acts as user alice, and the user's panel login gets its name.
func (h *Hub) freeUserNameLocked(base string) string {
	// Taken: a live or purged user, an Entra binding, any panel account,
	// and any node of that name or user - revoked ones too: a name with
	// another person's history is never handed out again.
	taken := func(n string) bool {
		if h.tenants[n] != nil || slices.Contains(h.purged, n) {
			return true
		}
		if oid, _ := h.store.Meta(entraUserKey(n)); oid != "" {
			return true
		}
		if _, err := h.store.GetUser(n); err == nil {
			return true
		}
		for _, x := range h.nodes {
			if x.rec.Name == n || x.rec.Role != string(pki.RoleWorker) && nodeUser(x.rec) == n {
				return true
			}
		}
		return false
	}
	if !taken(base) {
		return base
	}
	for i := 2; ; i++ {
		s := strconv.Itoa(i)
		n := base[:min(len(base), 17-len(s))] + s
		if !taken(n) {
			return n
		}
	}
}

// ---------------------------------------------------------------- gRPC

func (s *enrollServer) AuthInfo(context.Context, *pb.AuthInfoRequest) (*pb.AuthInfoResponse, error) {
	e := &s.h.cfg.Auth.Entra
	if !e.Enabled() {
		return &pb.AuthInfoResponse{}, nil
	}
	return &pb.AuthInfoResponse{EntraTenantId: e.TenantID, EntraClientId: e.ClientID, EntraScope: e.FullScope()}, nil
}

// EntraJoin enrolls a device of an Entra user (`longarm login`): the access
// token replaces the join code, the certificate is issued right away. A
// device name the same user already has (signing in again on the same
// machine) is taken over: the old device's certificates are revoked. With
// kind "worker" (`longarmd login --az`) the machine becomes a worker owned
// by the token's user instead, like one joining with the user's worker code.
func (s *enrollServer) EntraJoin(ctx context.Context, req *pb.EntraJoinRequest) (*pb.JoinResponse, error) {
	h := s.h
	c := callerFrom(ctx)
	if !h.joinLimit.allow(c.addr) {
		return nil, status.Error(codes.ResourceExhausted, "too many join attempts; wait a minute")
	}
	if h.entra == nil {
		return nil, status.Error(codes.FailedPrecondition, "this hub has no Microsoft Entra sign-in ([auth.entra] in hub.toml)")
	}
	if req.Kind != "" && req.Kind != linkClient && req.Kind != linkWorker {
		return nil, status.Errorf(codes.InvalidArgument, "kind %q: want client or worker", req.Kind)
	}
	if err := checkDevice(req.Hostname, req.Os, req.Arch, req.Version); err != nil {
		return nil, err
	}
	if _, err := pki.ParseCSR(req.CsrPem); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "csr: %v", err)
	}
	claims, err := h.verifyAccessToken(ctx, req.AccessToken)
	if err != nil {
		h.log.Warn("entra join: token rejected", "err", err, "remote", c.addr)
		return nil, status.Errorf(codes.Unauthenticated, "access token rejected: %v", err)
	}
	id := claims.identity()
	who := "entra/" + id.upn
	dev := deviceRequest{csr: req.CsrPem, name: req.Name, hostname: req.Hostname, os: req.Os, arch: req.Arch, version: req.Version,
		labels: req.Labels}
	if err := checkWorkerLabels(req.Kind == linkWorker, req.Labels); err != nil {
		return nil, err
	}
	if req.Kind == linkWorker {
		// A worker runs other people's jobs once shared: a person registers
		// it, never a CI app acting as a user.
		if claims.App != "" {
			h.log.Warn("entra join: app-only token for a worker refused", "app", claims.App, "remote", c.addr)
			return nil, status.Error(codes.PermissionDenied, "app-only tokens may not enroll workers (sign in as a user)")
		}
		user, _, err := h.entraSignIn(id, "longarmd login")
		if err != nil {
			return nil, err
		}
		resp, err := h.enrollOwnedWorker(who, user, dev)
		if err != nil {
			return nil, err
		}
		h.log.Info("worker joined by entra sign-in", "node", resp.Name, "owner", user, "oid", id.oid, "remote", c.addr)
		h.auditHub(who, "entra/worker", map[string]any{"user": user, "upn": id.upn, "oid": id.oid, "worker": resp.Name, "remote": c.addr})
		h.attachTailnet(ctx, resp)
		return resp, nil
	}
	var user string
	admin := false
	if claims.App != "" { // CI: an app acts as its configured user, never as an admin
		who = "entra-app/" + claims.App
		user, err = h.entraAppUser(claims.App, id.oid)
	} else {
		user, admin, err = h.entraSignIn(id, "longarm login")
	}
	if err != nil {
		return nil, err
	}
	resp, replaced, err := h.enrollDevice(who, user, admin, dev)
	if err != nil {
		return nil, err
	}
	h.log.Info("node joined by entra sign-in", "node", resp.Name, "role", resp.Role, "user", user, "who", who, "oid", id.oid, "remote", c.addr)
	h.auditHub(who, "entra/login", map[string]any{"user": user, "upn": id.upn, "oid": id.oid, "app": claims.App,
		"device": resp.Name, "role": resp.Role, "replaced": replaced, "remote": c.addr})
	h.attachTailnet(ctx, resp)
	return resp, nil
}

// deviceRequest is what a machine says about itself when it enrolls.
type deviceRequest struct {
	csr                               []byte
	name, hostname, os, arch, version string
	labels                            map[string]string // owned workers: as a worker code's (longarmd login --label)
	// admin: an admin worker (a pairing's admin_worker_csr_pem): marked
	// (store.Node.Admin, label longarm.io/admin=true), owner-only, never placed
	admin bool
	// companion: the ID of the worker an admin worker is enrolled with (the
	// same machine's, same pairing): the two get linked (store.Node.Companion)
	companion string
	// carry: the worker this one replaces on its machine, of the same
	// owner (replaceLinkNodes): its sharing, profile, labels and companion
	// carry over
	carry *store.Node
}

// checkWorkerLabels validates the labels a sign-in asks for: workers only,
// the rules of every label.
func checkWorkerLabels(worker bool, labels map[string]string) error {
	if len(labels) == 0 {
		return nil
	}
	if !worker {
		return status.Error(codes.InvalidArgument, "labels go with a worker's sign-in only")
	}
	if err := sched.ValidateLabels(labels, false); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if err := sched.CheckUserLabels(labels); err != nil {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return nil
}

// enrollDevice issues a client certificate (admin: an admin certificate)
// for a device of user, approved right away (who: the sign-in, for the
// node's history). Without a requested name the device is <user>-<short
// host>; a name the same user's device already has is taken over (signing
// in again on the same machine): that device's certificates are revoked.
func (h *Hub) enrollDevice(who, user string, admin bool, d deviceRequest) (resp *pb.JoinResponse, replaced bool, err error) {
	role := pki.RoleClient
	if admin {
		role = pki.RoleAdmin
	}
	name, explicit := d.name, d.name != ""
	if !explicit {
		name = user
		// the short host name: CI runners and cloud VMs have long FQDNs
		host, _, _ := strings.Cut(d.hostname, ".")
		if host := sanitizeName(host); host != "" {
			name = user + "-" + host
		}
		name = nodeid.SafeName(name, autoNamePrefix(string(role)))
		// room for a collision suffix (-2, -3, ...) within pki.ValidName's 63
		if len(name) > maxAutoDeviceName {
			name = strings.TrimRight(name[:maxAutoDeviceName], "-._")
		}
	}
	if err := checkNewName(name); err != nil {
		return nil, false, status.Errorf(codes.InvalidArgument, "%s (use --name)", status.Convert(err).Message())
	}
	mine := func(n *node) bool { return n.rec.Role != string(pki.RoleWorker) && nodeUser(n.rec) == user }
	h.mu.Lock()
	var old *node // the same user's device of that name: taken over
	for i, base := 2, name; ; i++ {
		n := h.nameTakenLocked(name, "")
		reserved := h.nodeNameReservedLocked(name, string(role), user)
		if !reserved && n == nil {
			break
		}
		if !reserved && mine(n) && n.rec.Name == name {
			replaced, old = true, n
			break
		}
		if explicit {
			h.mu.Unlock()
			return nil, false, status.Errorf(codes.AlreadyExists, "node name %q is taken (pick another with --name)", name)
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	h.mu.Unlock()
	if err := checkNewName(name); err != nil {
		return nil, false, status.Errorf(codes.InvalidArgument, "%s (use --name)", status.Convert(err).Message())
	}
	if replaced {
		if _, err := h.Revoke(who+" (signed in again)", "id:"+old.rec.ID); err != nil {
			return nil, false, err
		}
	}
	now := time.Now().UTC()
	rec := &store.Node{ID: nodeid.New(), Name: name, Role: string(role), Hostname: d.hostname, OS: d.os, Arch: d.arch,
		Version: d.version, JoinedAt: now, User: user}
	cert, err := h.issue(rec, d.csr)
	if err != nil {
		return nil, false, err
	}
	rec.State, rec.ApprovedAt = stateApproved, now
	h.mu.Lock()
	if n := h.nameTakenLocked(name, ""); n != nil { // raced with another join
		h.mu.Unlock()
		return nil, false, status.Errorf(codes.AlreadyExists, "node name %q is taken", name)
	}
	if err := h.store.PutNode(rec); err != nil {
		h.mu.Unlock()
		return nil, false, putNodeErr(err)
	}
	n := &node{rec: rec}
	h.nodes[rec.ID] = n
	h.publishNodeLocked(n, "joined (sign-in of "+who+")")
	h.mu.Unlock()
	return &pb.JoinResponse{State: stateApproved, Name: name, Role: string(role), CaPem: h.ca.CertPEM, CertPem: cert,
		HubName: h.cfg.Name, Message: "signed in as " + user, Id: rec.ID}, replaced, nil
}

// enrollOwnedWorker issues a worker certificate for a machine its owner
// registers (`longarmd login`): like a worker joining with the owner's
// worker code - approved right away, usable by the owner only until
// shared, never more than a worker. The name is the requested one or the
// host name; a taken name is refused, as for worker codes.
func (h *Hub) enrollOwnedWorker(who, owner string, d deviceRequest) (*pb.JoinResponse, error) {
	name := d.name
	if name == "" {
		name = nodeid.SafeName(sanitizeName(d.hostname), "w-")
	}
	if err := checkNewName(name); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%s (use --name)", status.Convert(err).Message())
	}
	h.mu.Lock()
	if h.tenants[owner] == nil || h.userDisabledLocked(owner) {
		h.mu.Unlock()
		return nil, status.Errorf(codes.PermissionDenied, "user %s is disabled or unknown", owner)
	}
	if n := h.nameTakenLocked(name, ""); n != nil {
		h.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "node name %q is taken (pick another with --name, or remove the old worker)", name)
	}
	if h.nodeNameReservedLocked(name, string(pki.RoleWorker), owner) {
		h.mu.Unlock()
		return nil, errNameReserved(name)
	}
	h.mu.Unlock()
	now := time.Now().UTC()
	rec := &store.Node{ID: nodeid.New(), Name: name, Role: string(pki.RoleWorker), Hostname: d.hostname, OS: d.os, Arch: d.arch,
		Version: d.version, JoinedAt: now, Owner: owner, AccessVersion: 1, Labels: maps.Clone(d.labels), Admin: d.admin,
		Companion: d.companion}
	if c := d.carry; c != nil && c.Owner == owner && !d.admin {
		// the companion (the machine's admin worker, by ID) is linked
		// back to the new worker below (pairLocked)
		rec.Shares, rec.Profile, rec.Companion, rec.AccessVersion = c.Shares, c.Profile, c.Companion, c.AccessVersion+1
		if len(rec.Labels) == 0 {
			rec.Labels = maps.Clone(c.Labels)
		}
	}
	cert, err := h.issue(rec, d.csr)
	if err != nil {
		return nil, err
	}
	rec.State, rec.ApprovedAt = stateApproved, now
	h.mu.Lock()
	if n := h.nameTakenLocked(name, ""); n != nil { // raced with another join
		h.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "node name %q is taken", name)
	}
	if err := h.store.PutNode(rec); err != nil {
		h.mu.Unlock()
		return nil, putNodeErr(err)
	}
	n := &node{rec: rec}
	h.nodes[rec.ID] = n
	h.pairLocked(n) // an admin worker enrolled with its machine's worker
	h.event(context.Background(), "node.joined", map[string]string{"role": rec.Role, "state": rec.State, "os": d.os, "arch": d.arch, "node_version": d.version})
	h.publishNodeLocked(n, "joined (owned by "+owner+", sign-in of "+who+")")
	h.checkTenancyFlipLocked() // an owned worker separates users
	h.mu.Unlock()
	h.auditHub("node/"+name, "workers/join", map[string]any{"worker": name, "id": rec.ID, "owner": owner, "via": who, "labels": d.labels, "admin_worker": d.admin, "companion": d.companion})
	labels, msg := maps.Clone(d.labels), "registered as a worker of "+owner
	if d.admin {
		if labels == nil {
			labels = map[string]string{}
		}
		labels[sched.AdminLabel] = "true"
		msg = "registered as the admin worker of " + owner + " (owner-only, never shared, never placed automatically)"
	}
	return &pb.JoinResponse{State: stateApproved, Name: name, Role: rec.Role, CaPem: h.ca.CertPEM, CertPem: cert,
		HubName: h.cfg.Name, Owner: owner, Message: msg, Labels: labels, Id: rec.ID}, nil
}
