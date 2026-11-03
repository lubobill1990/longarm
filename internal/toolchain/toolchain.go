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

// KnownTools is the baseline inventory, even on workers without a profile.
var KnownTools = []string{"git", "gh", "node", "pnpm", "uv", "pwsh", "claude", "copilot", "go"}

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
	// Disabled forbids both probes and installations.
	Disabled bool

	probeMu sync.Mutex
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

func (r *Reconciler) setReport(rep *pb.ToolchainReport) bool {
	r.mu.Lock()
	changed := r.setReportLocked(rep)
	r.mu.Unlock()
	if changed && r.Report != nil {
		r.Report(proto.Clone(rep).(*pb.ToolchainReport))
	}
	return changed
}

func (r *Reconciler) setReportLocked(rep *pb.ToolchainReport) bool {
	if rep.Versions == nil && r.report != nil {
		rep.Versions = maps.Clone(r.report.Versions)
	}
	changed := r.report == nil || r.report.Profile != rep.Profile || r.report.State != rep.State || r.report.Revision != rep.Revision ||
		r.report.OwnerRevision != rep.OwnerRevision || r.report.Error != rep.Error || !maps.Equal(r.report.Versions, rep.Versions)
	if changed {
		rep.UpdatedAt = timestamppb.Now()
		r.report = rep
	}
	return changed
}

// Run reconciles until ctx is done.
func (r *Reconciler) Run(ctx context.Context) {
	if r.Disabled {
		return
	}
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
	inventoryDone := make(chan struct{})
	go func() {
		defer close(inventoryDone)
		inventory := time.NewTicker(5 * time.Minute)
		defer inventory.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-inventory.C:
				r.refreshInventory(ctx)
			}
		}
	}()
	defer func() { <-inventoryDone }()
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

// Inventory keeps refreshing while waiting for jobs, but not during installs.
// Merge under the lock so it cannot roll back a state change.
func (r *Reconciler) refreshInventory(ctx context.Context) {
	if r.Disabled || !r.probeMu.TryLock() {
		return
	}
	defer r.probeMu.Unlock()
	versions, _, _ := r.check(ctx, &pb.ToolchainDesired{}, 3*time.Second)
	if ctx.Err() != nil {
		return
	}
	r.mu.Lock()
	rep := &pb.ToolchainReport{State: "none"}
	if r.report != nil {
		rep = proto.Clone(r.report).(*pb.ToolchainReport)
	}
	rep.Versions = versions
	changed := r.setReportLocked(rep)
	r.mu.Unlock()
	if changed && r.Report != nil {
		r.Report(proto.Clone(rep).(*pb.ToolchainReport))
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
func (r *Reconciler) check(ctx context.Context, d *pb.ToolchainDesired, timeout time.Duration) (map[string]string, []*pb.ToolSpec, []string) {
	versions := r.Status().Versions
	if versions == nil {
		versions = map[string]string{}
	}
	unknown := map[string]error{}
	var todo []*pb.ToolSpec
	var errs []string
	for _, name := range KnownTools {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		v, err := r.Backend.Version(cctx, name)
		cancel()
		if err != nil {
			r.Log.Warn("toolchain: version check", "tool", name, "err", err)
			unknown[name] = err
			continue
		}
		versions[name] = v
	}
	for _, t := range d.Tools {
		if err := unknown[t.Name]; err != nil {
			errs = append(errs, fmt.Sprintf("%s: version unknown: %v", t.Name, err))
			continue
		}
		if !Satisfies(versions[t.Name], t.Version) {
			todo = append(todo, t)
		}
	}
	return versions, todo, errs
}

func (r *Reconciler) reconcile(ctx context.Context) {
	if r.Disabled {
		return
	}
	r.probeMu.Lock()
	d := r.current()
	if d == nil {
		d = &pb.ToolchainDesired{}
	}
	versions, todo, probeErrs := r.check(ctx, d, 30*time.Second)
	r.probeMu.Unlock()
	report := func(state, message string) {
		r.setReport(&pb.ToolchainReport{Profile: d.Profile, Revision: d.Revision, OwnerRevision: d.OwnerRevision,
			State: state, Error: message, Versions: versions})
	}
	if len(d.Tools) == 0 {
		state := "none"
		if d.Revision > 0 {
			state = "ok"
		}
		report(state, "")
		return
	}
	if len(todo) == 0 {
		if len(probeErrs) > 0 {
			report("failed", strings.Join(probeErrs, "; "))
		} else {
			report("ok", "")
		}
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
		report("pending", fmt.Sprintf("waiting for %d running job(s) to finish", r.Running()))
		versions = nil // subsequent progress reports keep the refreshed inventory
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
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	report("applying", "")
	r.Log.Info("toolchain: installing", "tools", names)
	var errs []string
	sort.Slice(todo, func(i, j int) bool { return installOrder(todo[i].Name) < installOrder(todo[j].Name) })
	for _, t := range todo {
		if !proto.Equal(r.current(), d) {
			r.Kick()
			return
		}
		ictx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		err := r.Backend.Install(ictx, t.Name, t.Version, r.Log)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", t.Name, err))
		}
	}
	versions, todo, probeErrs = r.check(ctx, d, 30*time.Second)
	errs = append(errs, probeErrs...)
	for _, t := range todo {
		if !slicesContainsPrefix(errs, t.Name+":") {
			errs = append(errs, fmt.Sprintf("%s: installed %q does not satisfy %q", t.Name, versions[t.Name], t.Version))
		}
	}
	if len(errs) > 0 {
		r.Log.Warn("toolchain: failed", "errors", errs)
		report("failed", strings.Join(errs, "; "))
		return
	}
	r.Log.Info("toolchain: ok", "profile", d.Profile, "revision", d.Revision, "versions", versions)
	report("ok", "")
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
