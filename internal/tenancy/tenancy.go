// Package tenancy holds what the hub and the workers share about
// multi-tenancy (docs/security.md): who owns what, who may read or control
// it, the users' OS account names on workers, quotas, and the tenancy
// policy the hub pushes to workers.
//
// Users are identified by the user of their certificate (pki.Identity.Owner):
// a person with several devices has one user and several certificates.
// Everything a user creates - jobs, sessions, files, forwards, mirrors -
// belongs to that user. Admins act on everything; a client only on its own;
// members of a team a job is shared with may read it (state and logs).
package tenancy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Worker isolation modes (longarmd.toml [isolation] mode).
const (
	// Shared: every workload runs as longarmd's own account (single-user
	// setups; the default).
	Shared = "shared"
	// PerUser: longarmd runs as LocalSystem / root and runs each user's
	// workloads, file access and shells as that user's OS account.
	PerUser = "per-user"
)

// DefaultAccountPrefix prefixes the OS accounts of users on workers.
const DefaultAccountPrefix = "la-"

// maxAccountName is Windows' limit for local account names (Linux allows 32).
const maxAccountName = 20

var accountRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)

// AccountName maps a user to its OS account on workers: prefix+user when
// that is a valid account name of at most 20 characters, else prefix + a
// cleaned-up start of the user name + "-" + 8 hex digits of its hash (older
// certificates have names like "Alice.Laptop"). Deterministic: every
// worker picks the same name. Two users whose names collide that way
// cannot both use a worker (the second is refused: the space's owner file
// names the first), but never share an account.
func AccountName(prefix, user string) string {
	if prefix == "" {
		prefix = DefaultAccountPrefix
	}
	name := prefix + user
	if len(name) <= maxAccountName && accountRE.MatchString(name) {
		return name
	}
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return -1
	}, user)
	sum := sha256.Sum256([]byte(user))
	keep := maxAccountName - len(prefix) - 9
	if keep < 0 {
		keep = 0
	}
	if len(clean) > keep {
		clean = clean[:keep]
	}
	return prefix + clean + "-" + hex.EncodeToString(sum[:4])
}

// OwnerOf returns the owner of a job spec: its owner field, else (jobs
// from before multi-tenancy) the name in submitted_by ("client/alice").
func OwnerOf(spec *pb.JobSpec) string {
	if o := spec.GetOwner(); o != "" {
		return o
	}
	return OwnerFromBy(spec.GetSubmittedBy())
}

// OwnerFromBy turns "role/name" into name.
func OwnerFromBy(by string) string {
	if _, name, ok := strings.Cut(by, "/"); ok {
		return name
	}
	return by
}

// Caller is who calls, as far as access decisions go.
type Caller struct {
	User  string
	Admin bool
	Teams []string
}

// CallerOf builds a Caller from a certificate identity and the teams of its
// user.
func CallerOf(id pki.Identity, teams []string) Caller {
	return Caller{User: id.Owner(), Admin: id.Role == pki.RoleAdmin, Teams: teams}
}

// CanRead: the owner, admins and members of a team the item is shared with.
func (c Caller) CanRead(owner string, shareTeams []string) bool {
	if c.Admin || (owner != "" && owner == c.User) {
		return true
	}
	for _, t := range shareTeams {
		if slices.Contains(c.Teams, t) {
			return true
		}
	}
	return false
}

// CanControl (kill, attach, write): the owner and admins.
func (c Caller) CanControl(owner string) bool {
	return c.Admin || (owner != "" && owner == c.User)
}

var teamRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,31}$`)

// ValidTeam reports whether name is a valid team name.
func ValidTeam(name string) bool { return teamRE.MatchString(name) }

// ResolveShare turns SubmitRequest.share ("team" = all of the owner's teams,
// "team:<name>" = that one) into team names, all of which the owner must
// belong to.
func ResolveShare(share, ownerTeams []string) ([]string, error) {
	var out []string
	for _, s := range share {
		switch {
		case s == "" || s == "private" || s == "none":
		case s == "team":
			if len(ownerTeams) == 0 {
				return nil, fmt.Errorf("--share team: you are in no team")
			}
			out = append(out, ownerTeams...)
		case strings.HasPrefix(s, "team:") || strings.HasPrefix(s, "team="):
			t := s[5:]
			if !slices.Contains(ownerTeams, t) {
				return nil, fmt.Errorf("--share %s: you are not in team %q", s, t)
			}
			out = append(out, t)
		default:
			return nil, fmt.Errorf("bad share %q (want team or team:<name>)", s)
		}
	}
	sort.Strings(out)
	return slices.Compact(out), nil
}

// MergeQuota returns def with every non-zero field of over applied.
func MergeQuota(def, over *pb.Quota) *pb.Quota {
	q := &pb.Quota{}
	if def != nil {
		q = proto.Clone(def).(*pb.Quota)
	}
	if over == nil {
		return q
	}
	if over.MaxJobs != 0 {
		q.MaxJobs = over.MaxJobs
	}
	if over.MaxMemGb != 0 {
		q.MaxMemGb = over.MaxMemGb
	}
	if over.MaxJobsPerWorker != 0 {
		q.MaxJobsPerWorker = over.MaxJobsPerWorker
	}
	if over.MaxMemGbPerWorker != 0 {
		q.MaxMemGbPerWorker = over.MaxMemGbPerWorker
	}
	if over.MaxSessionsPerWorker != 0 {
		q.MaxSessionsPerWorker = over.MaxSessionsPerWorker
	}
	if over.JobMemGb != 0 {
		q.JobMemGb = over.JobMemGb
	}
	if over.MaxJobMemGb != 0 {
		q.MaxJobMemGb = over.MaxJobMemGb
	}
	if over.JobCpus != 0 {
		q.JobCpus = over.JobCpus
	}
	if over.MaxJobCpus != 0 {
		q.MaxJobCpus = over.MaxJobCpus
	}
	return q
}

// QuotaEmpty reports whether q limits nothing.
func QuotaEmpty(q *pb.Quota) bool { return q == nil || proto.Equal(q, &pb.Quota{}) }

const gib = 1 << 30

// JobLimits applies a quota's per-job defaults and caps to a job's memory
// limit (bytes) and CPU limit (cores).
func JobLimits(q *pb.Quota, memBytes int64, cpus float64) (int64, float64, error) {
	if q == nil {
		return memBytes, cpus, nil
	}
	if memBytes <= 0 && q.JobMemGb > 0 {
		memBytes = int64(q.JobMemGb * gib)
	}
	if q.MaxJobMemGb > 0 {
		if memBytes <= 0 {
			memBytes = int64(q.MaxJobMemGb * gib)
		} else if memBytes > int64(q.MaxJobMemGb*gib) {
			return 0, 0, fmt.Errorf("memory limit %.1f GB exceeds your per-job maximum of %.1f GB", float64(memBytes)/gib, q.MaxJobMemGb)
		}
	}
	if cpus <= 0 && q.JobCpus > 0 {
		cpus = q.JobCpus
	}
	if q.MaxJobCpus > 0 {
		if cpus <= 0 {
			cpus = q.MaxJobCpus
		} else if cpus > q.MaxJobCpus {
			return 0, 0, fmt.Errorf("CPU limit %.1f exceeds your per-job maximum of %.1f cores", cpus, q.MaxJobCpus)
		}
	}
	return memBytes, cpus, nil
}

// MemGB is the memory a job accounts for in quotas: its limit, else its
// scheduler reservation label, else def.
func MemGB(memBytes int64, label string, def float64) float64 {
	if memBytes > 0 {
		return float64(memBytes) / gib
	}
	var v float64
	if _, err := fmt.Sscanf(label, "%g", &v); err == nil && v > 0 {
		return v
	}
	return def
}

// ---------------------------------------------------------------- policy

// Policy holds the tenancy policy a worker got from its hub (persisted, so
// teams, quotas and disabled users survive restarts while the hub is away).
type Policy struct {
	mu    sync.RWMutex
	path  string
	p     *pb.TenancyPolicy
	users map[string]*pb.TenantUser
}

// PolicyFile is the policy's file in longarmd's data dir.
const PolicyFile = "tenancy.json"

// LoadPolicy reads path ("" = memory only); a missing file is an empty policy.
func LoadPolicy(path string) (*Policy, error) {
	p := &Policy{path: path}
	np := &pb.TenancyPolicy{}
	if path != "" {
		b, err := os.ReadFile(path)
		if err == nil {
			if err := protojson.Unmarshal(b, np); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	p.setLocked(np)
	return p, nil
}

func (p *Policy) setLocked(np *pb.TenancyPolicy) {
	p.p = np
	p.users = map[string]*pb.TenantUser{}
	for _, u := range np.Users {
		p.users[u.Name] = u
	}
}

// Set replaces the policy and saves it.
func (p *Policy) Set(np *pb.TenancyPolicy) error {
	np = proto.Clone(np).(*pb.TenancyPolicy)
	p.mu.Lock()
	p.setLocked(np)
	path := p.path
	p.mu.Unlock()
	if path == "" {
		return nil
	}
	b, err := protojson.MarshalOptions{Indent: "  "}.Marshal(np)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	// (ReplaceFile: on Windows a reader holding the file open makes a plain
	// rename fail with "Access is denied")
	return platform.ReplaceFile(tmp, filepath.Clean(path))
}

// Get returns a copy of the policy.
func (p *Policy) Get() *pb.TenancyPolicy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return proto.Clone(p.p).(*pb.TenancyPolicy)
}

// Version of the policy (0 = never got one).
func (p *Policy) Version() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.p.Version
}

// Enabled reports whether the hub separates users.
func (p *Policy) Enabled() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.p.Enabled
}

// Teams of user.
func (p *Policy) Teams(user string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if u := p.users[user]; u != nil {
		return append([]string(nil), u.Teams...)
	}
	return nil
}

// Disabled reports whether user is disabled (or purged).
func (p *Policy) Disabled(user string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if u := p.users[user]; u != nil && u.Disabled {
		return true
	}
	return slices.Contains(p.p.Purge, user)
}

// Quota is user's effective quota (the default with the user's overrides).
func (p *Policy) Quota(user string) *pb.Quota {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var over *pb.Quota
	if u := p.users[user]; u != nil {
		over = u.Quota
	}
	return MergeQuota(p.p.DefaultQuota, over)
}

// Purge lists users whose data workers delete.
func (p *Policy) Purge() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.p.Purge...)
}
