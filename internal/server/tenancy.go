package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/sched"
	"github.com/lubobill1990/longarm/internal/supervisor"
	"github.com/lubobill1990/longarm/internal/tenancy"
	"github.com/lubobill1990/longarm/internal/userspace"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Multi-tenancy on the worker (docs/security.md). Ownership is enforced
// when the worker isolates users ([isolation] mode = "per-user") or the
// hub separates them (its tenancy policy is enabled); otherwise - one
// person's fleet - every client acts on everything, as before. A worker
// access policy ([access]) enforces it, too, for everybody but its owners.

// enforce reports whether ownership checks apply because users are
// separated (isolation or the hub's policy; quotas come with it).
func (s *Server) enforce() bool {
	// a user-owned worker shared with others separates them, too
	return s.perUser() || (s.Policy != nil && s.Policy.Enabled()) || s.WorkerAccess.Owner() != ""
}

// enforceFor reports whether ownership checks apply to the caller: always
// when users are separated; with only an access policy, to everybody but the
// worker's owners - they keep the one-person-fleet view they had (their
// devices' plain certificates act on each other's jobs and sessions).
func (s *Server) enforceFor(ctx context.Context) bool {
	return s.enforce() || (s.Access.Active() && !s.Access.IsOwner(IdentityFrom(ctx).Owner()))
}

// perUser reports per-user OS isolation (never inside the user helper,
// which already runs as the user).
func (s *Server) perUser() bool { return s.Spaces != nil && s.Spaces.PerUser() }

// ws is the workspace store of shared isolation (or of the user helper).
func (s *Server) ws() *workspace.Store {
	if s.helperWS != nil {
		return s.helperWS
	}
	return s.Spaces.Shared().WS
}

// callerOf is the access-control view of the authenticated caller.
func (s *Server) callerOf(ctx context.Context) tenancy.Caller {
	id := IdentityFrom(ctx)
	var teams []string
	if s.Policy != nil {
		teams = s.Policy.Teams(id.Owner())
	}
	c := tenancy.CallerOf(id, teams)
	c.Admin = c.Admin && s.Access.AdminRights(id)
	if o := s.WorkerAccess.Owner(); o != "" && o != c.User {
		c.Admin = false // sharing a worker shares it, not the owner's jobs and sessions
	}
	if !s.enforceFor(ctx) {
		c.Admin = true // single-user fleet: no separation
	}
	return c
}

func (s *Server) quota(user string) *pb.Quota {
	if s.Policy == nil {
		return nil
	}
	return s.Policy.Quota(user)
}

// errNotYours hides other users' items: NotFound, as if they did not exist.
func errNotYours(kind, id string) error {
	return status.Errorf(codes.NotFound, "%s %s not found", kind, id)
}

// findJob looks up a job the caller may read (control: may kill).
func (s *Server) findJob(ctx context.Context, id string, control bool) (*userspace.Space, supervisor.Job, error) {
	c := s.callerOf(ctx)
	var sp *userspace.Space
	var j supervisor.Job
	var err error
	if s.perUser() && !c.Admin && !control {
		// own space first; team-shared jobs live in other spaces
		if own := s.Spaces.Lookup(c.User); own != nil {
			if j, err = own.Jobs.Get(id); err == nil {
				return own, j, nil
			}
		}
	}
	sp, j, err = s.Spaces.FindJob(id)
	if err != nil {
		return nil, j, MgrErr(err)
	}
	owner := j.Spec.OwnerName()
	if control && !c.CanControl(owner) || !control && !c.CanRead(owner, j.Spec.ShareTeams) {
		return nil, supervisor.Job{}, errNotYours("job", id)
	}
	return sp, j, nil
}

// targetOwner is whose session/data a call is about: the caller's own, or -
// admins only - the owner they asked for.
func (s *Server) targetOwner(ctx context.Context, asked string) (string, error) {
	own := IdentityFrom(ctx).Owner()
	if asked == "" || asked == own {
		return own, nil
	}
	if s.callerOf(ctx).Admin {
		return asked, nil
	}
	return "", status.Error(codes.PermissionDenied, "only admins act on other users' sessions")
}

// checkJobQuota enforces the owner's per-worker quota for a new job of
// memBytes (after defaults) with scheduler label memLabel.
func (s *Server) checkJobQuota(sp *userspace.Space, owner string, q *pb.Quota, memBytes int64, memLabel string) error {
	if q == nil || (q.MaxJobsPerWorker == 0 && q.MaxMemGbPerWorker == 0) {
		return nil
	}
	n, gb := 0, 0.0
	for _, j := range sp.Jobs.List(nil, 0) {
		if j.Status.State.Terminal() || j.Spec.OwnerName() != owner {
			continue
		}
		n++
		gb += tenancy.MemGB(j.Spec.MemLimitBytes, j.Spec.Labels[sched.LabelMemGB], 0)
	}
	if q.MaxJobsPerWorker > 0 && n+1 > int(q.MaxJobsPerWorker) {
		return status.Errorf(codes.ResourceExhausted, "quota: %d active jobs of yours on this worker (max %d)", n, q.MaxJobsPerWorker)
	}
	newGB := tenancy.MemGB(memBytes, memLabel, 0)
	if q.MaxMemGbPerWorker > 0 && gb+newGB > q.MaxMemGbPerWorker+1e-9 {
		return status.Errorf(codes.ResourceExhausted, "quota: %.1f GB reserved by your jobs on this worker + %.1f GB > %.1f GB", gb, newGB, q.MaxMemGbPerWorker)
	}
	return nil
}

// checkSessionQuota enforces max_sessions_per_worker.
func (s *Server) checkSessionQuota(sp *userspace.Space, owner string) error {
	q := s.quota(owner)
	if q == nil || q.MaxSessionsPerWorker == 0 {
		return nil
	}
	n := 0
	for _, j := range sp.Sessions.List([]jobs.State{jobs.Preparing, jobs.Running}, 0) {
		if j.Spec.OwnerName() == owner {
			n++
		}
	}
	if n+1 > int(q.MaxSessionsPerWorker) {
		return status.Errorf(codes.ResourceExhausted, "quota: %d sessions of yours on this worker (max %d)", n, q.MaxSessionsPerWorker)
	}
	return nil
}

// spaceFor returns the caller's space for creating things, refusing
// disabled users.
func (s *Server) spaceFor(owner string) (*userspace.Space, error) {
	sp, err := s.Spaces.For(owner)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "user space: %v", err)
	}
	return sp, nil
}

// Forward target policies (longarmd.toml [forward] targets).
const (
	// ForwardLoopback: forwards (Forward.Open, the built-in sshd's
	// direct-tcpip, port links) reach the worker's own loopback ports only
	// (the default).
	ForwardLoopback = "loopback"
	// ForwardAny: any host:port the worker can reach (other machines on its
	// LAN, other workers) - opt-in.
	ForwardAny = "any"
)

// ParseForwardTargets validates a [forward] targets value ("" = loopback).
func ParseForwardTargets(s string) (string, error) {
	switch s {
	case "", ForwardLoopback:
		return ForwardLoopback, nil
	case ForwardAny:
		return ForwardAny, nil
	}
	return "", fmt.Errorf("[forward] targets %q: want %q or %q", s, ForwardLoopback, ForwardAny)
}

// loopbackHost reports whether host names this machine's loopback.
func loopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// targetAllowed applies [forward] targets to every caller (admins too).
func (s *Server) targetAllowed(host string) error {
	if s.ForwardTargets == ForwardAny || loopbackHost(host) {
		return nil
	}
	if !s.forwardWarned.Swap(true) && s.Log != nil {
		// Forwards to other hosts were allowed before [forward] targets
		// existed: say once what turns them back on.
		s.Log.Warn(`forward to a non-loopback target refused: this worker forwards to its own loopback ports only; set [forward] targets = "any" in longarmd.toml to allow other hosts (the behaviour before this setting existed; logged once)`,
			"target", host)
	}
	return status.Errorf(codes.PermissionDenied, "this worker forwards to its own loopback ports only, not to %s ([forward] targets = %q)", host, ForwardLoopback)
}

// forwardAllowed: the worker's [forward] targets for everybody; with
// per-user isolation a non-admin may only reach a loopback port a process
// of its own account listens on.
func (s *Server) forwardAllowed(ctx context.Context, target string) error {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.targetAllowed(host); err != nil {
		return err
	}
	c := s.callerOf(ctx)
	if !s.perUser() || c.Admin {
		return nil
	}
	if !loopbackHost(host) {
		return status.Errorf(codes.PermissionDenied, "per-user isolation: forwards only reach loopback ports of your own processes (not %s)", host)
	}
	p, _ := strconv.Atoi(port)
	return s.portOwnedBy(c.User, p)
}

// portOwnedBy checks that a process of user's account (and nobody else's)
// listens on loopback port p (per-user isolation).
func (s *Server) portOwnedBy(user string, p int) error {
	sp := s.Spaces.Lookup(user)
	if sp == nil || sp.Account == nil {
		return status.Errorf(codes.PermissionDenied, "port %d is not listened on by a process of yours", p)
	}
	mine, others, err := platform.PortOwners(p, sp.Account)
	if err != nil {
		return status.Errorf(codes.Internal, "port owner: %v", err)
	}
	if !mine || others {
		return status.Errorf(codes.PermissionDenied, "port %d is not listened on by a process of yours", p)
	}
	return nil
}

// ForwardCheck is forwardAllowed for the built-in sshd's direct-tcpip.
func (s *Server) ForwardCheck(id pki.Identity, host string, port int) error {
	ctx := context.WithValue(context.Background(), idKey{}, id)
	return s.forwardAllowed(ctx, net.JoinHostPort(host, strconv.Itoa(port)))
}

// ForwardTargetCheck applies [forward] targets alone (the built-in sshd's
// direct-tcpip in shared isolation, also on direct listeners).
func (s *Server) ForwardTargetCheck(host string, _ int) error { return s.targetAllowed(host) }

// AccountFor returns the OS account a caller's ssh session runs as (nil =
// longarmd's), creating it on first use.
func (s *Server) AccountFor(id pki.Identity) (*userspace.Space, error) {
	if !s.perUser() {
		return nil, nil
	}
	if s.Policy != nil && s.Policy.Disabled(id.Owner()) {
		return nil, errors.New("user disabled")
	}
	sp, err := s.Spaces.For(id.Owner())
	if err != nil {
		return nil, fmt.Errorf("user space: %w", err)
	}
	return sp, nil
}
