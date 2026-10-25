// Package toolchain reconciles a worker's tools with the toolchain profile
// the hub assigned (DESIGN §3.1 toolchain, §7.2): a declarative list such
// as git, gh, node (LTS), pnpm, uv, pwsh 7, claude and copilot CLIs, go, each
// with a version prefix or "latest".
//
// Reconciliation is idempotent: tools whose installed version satisfies the
// spec are left alone. Installs run only while the worker has no running
// jobs (replacing a running exe fails on Windows); until then the worker
// reports "pending" and drains itself so the scheduler sends nothing new.
//
// Version semantics: "latest" = installed (any version; bump the profile to
// an explicit version to force an upgrade); "22" or "2.45" = the installed
// version starts with that prefix at a dot boundary.
package toolchain

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Backend checks and installs tools.
type Backend interface {
	// Version returns the installed version ("" = not installed).
	Version(ctx context.Context, tool string) (string, error)
	// Install installs or upgrades tool to version ("latest" or a prefix).
	Install(ctx context.Context, tool, version string, log *slog.Logger) error
}

var versionRE = regexp.MustCompile(`\d+(\.\d+)+`)

// ParseVersion extracts the first dotted version from a --version output.
func ParseVersion(out string) string { return versionRE.FindString(out) }

// Satisfies reports whether installed meets spec.
func Satisfies(installed, spec string) bool {
	if installed == "" {
		return false
	}
	spec = strings.TrimPrefix(strings.TrimSpace(spec), "v")
	if spec == "" || spec == "latest" || spec == "lts" {
		return true
	}
	installed = strings.TrimPrefix(installed, "v")
	return installed == spec || strings.HasPrefix(installed, spec+".")
}

// Reconciler applies desired toolchains.
type Reconciler struct {
	Backend Backend
	Log     *slog.Logger
	// Running returns the number of running jobs (installs wait for 0).
	Running func() int
	// Drain marks the worker as draining (true) or not while an update
	// waits for jobs / runs.
	Drain func(on bool, reason string)
	// Report receives every state change.
	Report func(*pb.ToolchainReport)
	// Interval between re-checks of an applied profile (default 30m).
	Interval time.Duration

	mu      sync.Mutex
	desired *pb.ToolchainDesired
	report  *pb.ToolchainReport
	wake    chan struct{}
}

// SetDesired installs a new desired toolchain (from the hub).
func (r *Reconciler) SetDesired(d *pb.ToolchainDesired) {
	r.mu.Lock()
	same := r.desired != nil && proto.Equal(r.desired, d)
	r.desired = proto.Clone(d).(*pb.ToolchainDesired)
	r.mu.Unlock()
	if !same {
		r.Kick()
	}
}

// Kick asks for a reconcile now.
func (r *Reconciler) Kick() {
	r.mu.Lock()
	if r.wake == nil {
		r.wake = make(chan struct{}, 1)
	}
	w := r.wake
	r.mu.Unlock()
	select {
	case w <- struct{}{}:
	default:
	}
}

// Status returns the last report.
func (r *Reconciler) Status() *pb.ToolchainReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.report == nil {
		return &pb.ToolchainReport{State: "none"}
	}
	return proto.Clone(r.report).(*pb.ToolchainReport)
}

func (r *Reconciler) setReport(rep *pb.ToolchainReport) {
	rep.UpdatedAt = timestamppb.Now()
	r.mu.Lock()
	changed := r.report == nil || r.report.State != rep.State || r.report.Revision != rep.Revision ||
		r.report.Error != rep.Error || !maps.Equal(r.report.Versions, rep.Versions)
	r.report = rep
	r.mu.Unlock()
	if changed && r.Report != nil {
		r.Report(proto.Clone(rep).(*pb.ToolchainReport))
	}
}

// Run reconciles until ctx is done.
func (r *Reconciler) Run(ctx context.Context) {
	r.Kick()
	if r.Interval <= 0 {
		r.Interval = 30 * time.Minute
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	r.mu.Lock()
	wake := r.wake
	r.mu.Unlock()
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		case <-t.C:
		}
		r.reconcile(ctx)
	}
}

func (r *Reconciler) current() *pb.ToolchainDesired {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.desired == nil {
		return nil
	}
	return proto.Clone(r.desired).(*pb.ToolchainDesired)
}

// check returns the installed versions and the tools needing an install.
func (r *Reconciler) check(ctx context.Context, d *pb.ToolchainDesired) (map[string]string, []*pb.ToolSpec) {
	versions := map[string]string{}
	var todo []*pb.ToolSpec
	for _, t := range d.Tools {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		v, err := r.Backend.Version(cctx, t.Name)
		cancel()
		if err != nil {
			r.Log.Warn("toolchain: version check", "tool", t.Name, "err", err)
		}
		versions[t.Name] = v
		if !Satisfies(v, t.Version) {
			todo = append(todo, t)
		}
	}
	return versions, todo
}

func (r *Reconciler) reconcile(ctx context.Context) {
	d := r.current()
	if d == nil {
		return
	}
	if d.Profile == "" || d.Revision == 0 {
		r.setReport(&pb.ToolchainReport{Profile: d.Profile, Revision: d.Revision, State: "none"})
		return
	}
	versions, todo := r.check(ctx, d)
	if len(todo) == 0 {
		r.setReport(&pb.ToolchainReport{Profile: d.Profile, Revision: d.Revision, State: "ok", Versions: versions})
		return
	}
	names := make([]string, len(todo))
	for i, t := range todo {
		names[i] = t.Name + "@" + t.Version
	}
	reason := fmt.Sprintf("toolchain %s r%d: %s", d.Profile, d.Revision, strings.Join(names, ", "))
	r.Drain(true, reason)
	defer r.Drain(false, "")
	for r.Running() > 0 {
		r.setReport(&pb.ToolchainReport{Profile: d.Profile, Revision: d.Revision, State: "pending", Versions: versions,
			Error: fmt.Sprintf("waiting for %d running job(s) to finish", r.Running())})
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if nd := r.current(); !proto.Equal(nd, d) { // the profile changed meanwhile
			r.Kick()
			return
		}
	}
	r.setReport(&pb.ToolchainReport{Profile: d.Profile, Revision: d.Revision, State: "applying", Versions: versions})
	r.Log.Info("toolchain: installing", "tools", names)
	var errs []string
	sort.Slice(todo, func(i, j int) bool { return installOrder(todo[i].Name) < installOrder(todo[j].Name) })
	for _, t := range todo {
		ictx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		err := r.Backend.Install(ictx, t.Name, t.Version, r.Log)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", t.Name, err))
		}
	}
	versions, todo = r.check(ctx, d)
	for _, t := range todo {
		if !slicesContainsPrefix(errs, t.Name+":") {
			errs = append(errs, fmt.Sprintf("%s: installed %q does not satisfy %q", t.Name, versions[t.Name], t.Version))
		}
	}
	if len(errs) > 0 {
		r.Log.Warn("toolchain: failed", "errors", errs)
		r.setReport(&pb.ToolchainReport{Profile: d.Profile, Revision: d.Revision, State: "failed", Versions: versions,
			Error: strings.Join(errs, "; ")})
		return
	}
	r.Log.Info("toolchain: ok", "profile", d.Profile, "revision", d.Revision, "versions", versions)
	r.setReport(&pb.ToolchainReport{Profile: d.Profile, Revision: d.Revision, State: "ok", Versions: versions})
}

func slicesContainsPrefix(s []string, p string) bool {
	for _, x := range s {
		if strings.HasPrefix(x, p) {
			return true
		}
	}
	return false
}

// installOrder: node before the npm-installed tools.
func installOrder(tool string) int {
	switch tool {
	case "git", "node", "pwsh":
		return 0
	case "pnpm", "claude", "copilot":
		return 2
	}
	return 1
}
