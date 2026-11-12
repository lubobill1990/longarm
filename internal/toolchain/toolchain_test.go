package toolchain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "longarm/proto/longarm/v1"
)

func TestInventoryWithoutProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake.json")
	if err := os.WriteFile(path, []byte(`{"tools":{"git":"2.45.0","copilot":"0.1.0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Backend: &Fake{Path: path}, Log: slog.Default()}
	r.reconcile(context.Background())
	rep := r.Status()
	if rep.State != "none" || len(rep.Versions) != len(KnownTools) || rep.Versions["git"] != "2.45.0" || rep.Versions["copilot"] != "0.1.0" {
		t.Fatalf("inventory: %v", rep)
	}
	if v, ok := rep.Versions["claude"]; !ok || v != "" {
		t.Fatalf("missing tool: %v", rep.Versions)
	}
	// A reconnect kick rechecks external installs without installing anything.
	if err := os.WriteFile(path, []byte(`{"tools":{"claude":"2.0.0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.reconcile(context.Background())
	if r.Status().Versions["claude"] != "2.0.0" {
		t.Fatal(r.Status())
	}
	r.Disabled = true
	before := r.Status()
	r.SetDesired(&pb.ToolchainDesired{Tools: []*pb.ToolSpec{{Name: "node", Version: "22"}}})
	r.reconcile(context.Background())
	if rep := r.Status(); rep.State != before.State || rep.OwnerRevision != before.OwnerRevision {
		t.Fatal("disabled reconciler changed status")
	}
}

type probeBackend struct {
	version func(context.Context, string) (string, error)
	install func(context.Context, string, string, *slog.Logger) error
}

func (b probeBackend) Version(ctx context.Context, tool string) (string, error) {
	return b.version(ctx, tool)
}

func (b probeBackend) Install(ctx context.Context, tool, version string, log *slog.Logger) error {
	return b.install(ctx, tool, version, log)
}

func TestProbeErrorsDoNotInstallOrEraseInventory(t *testing.T) {
	for _, probeErr := range []error{errors.New("probe failed"), context.DeadlineExceeded} {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("%v/cached=%v", probeErr, cached), func(t *testing.T) {
				r := &Reconciler{Log: slog.Default(), Backend: probeBackend{
					version: func(context.Context, string) (string, error) { return "", probeErr },
					install: func(context.Context, string, string, *slog.Logger) error {
						t.Fatal("installed a tool with an unknown version")
						return nil
					},
				}, Drain: func(bool, string) { t.Fatal("drained for a failed probe") }}
				if cached {
					r.setReport(&pb.ToolchainReport{Versions: map[string]string{"copilot": "1.2.3"}})
				}
				r.SetDesired(&pb.ToolchainDesired{Tools: []*pb.ToolSpec{{Name: "copilot", Version: "latest"}}})
				r.reconcile(context.Background())
				rep := r.Status()
				if rep.State != "failed" || !strings.Contains(rep.Error, "version unknown") {
					t.Fatalf("probe failure hidden: %v", rep)
				}
				r.refreshInventory(context.Background())
				v, found := r.Status().Versions["copilot"]
				if found != cached || (cached && v != "1.2.3") {
					t.Fatalf("unknown became missing: %v", r.Status())
				}
			})
		}
	}
}

func TestReconcileUsesLongProbeDeadline(t *testing.T) {
	r := &Reconciler{Log: slog.Default(), Backend: probeBackend{
		version: func(ctx context.Context, tool string) (string, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) < 20*time.Second {
				return "", context.DeadlineExceeded
			}
			return "1.2.3", nil
		},
		install: func(context.Context, string, string, *slog.Logger) error {
			t.Fatal("reinstalled a slow installed tool")
			return nil
		},
	}}
	r.SetDesired(&pb.ToolchainDesired{Tools: []*pb.ToolSpec{{Name: "copilot", Version: "latest"}}})
	r.reconcile(context.Background())
	if rep := r.Status(); rep.State != "ok" || rep.Versions["copilot"] != "1.2.3" {
		t.Fatalf("slow probe: %v", rep)
	}
	// A short inventory timeout preserves the last successful version.
	r.refreshInventory(context.Background())
	if r.Status().Versions["copilot"] != "1.2.3" {
		t.Fatal(r.Status())
	}
}

func TestOwnerProbeFailureDoesNotBlockOtherTools(t *testing.T) {
	installed := false
	r := &Reconciler{Log: slog.Default(), Running: func() int { return 0 }, Drain: func(bool, string) {},
		Backend: probeBackend{
			version: func(_ context.Context, tool string) (string, error) {
				if tool == "copilot" {
					return "", context.DeadlineExceeded
				}
				if tool == "node" && installed {
					return "22.0.0", nil
				}
				return "", nil
			},
			install: func(_ context.Context, tool, _ string, _ *slog.Logger) error {
				if tool != "node" {
					t.Fatalf("installed unknown tool %s", tool)
				}
				installed = true
				return nil
			},
		}}
	r.SetDesired(&pb.ToolchainDesired{Profile: "dev", Revision: 1, OwnerRevision: 1,
		Tools: []*pb.ToolSpec{{Name: "node", Version: "22"}, {Name: "copilot", Version: "latest"}}})
	r.reconcile(context.Background())
	if rep := r.Status(); !installed || rep.Versions["node"] != "22.0.0" || rep.State != "failed" {
		t.Fatalf("owner probe blocked profile tool: %v", rep)
	}
}

func TestInventoryOnlyReportsChanges(t *testing.T) {
	var reports int
	r := &Reconciler{Log: slog.Default(), Backend: &Fake{Path: filepath.Join(t.TempDir(), "fake.json")},
		Report: func(*pb.ToolchainReport) { reports++ }}
	r.reconcile(context.Background())
	first := r.Status().UpdatedAt
	r.refreshInventory(context.Background())
	r.reconcile(context.Background())
	if reports != 1 || !r.Status().UpdatedAt.AsTime().Equal(first.AsTime()) {
		t.Fatalf("unchanged inventory emitted %d reports", reports)
	}
}

func TestInventorySkipsInstall(t *testing.T) {
	started, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var probes atomic.Int32
	r := &Reconciler{Log: slog.Default(), Running: func() int { return 0 }, Drain: func(bool, string) {},
		Backend: probeBackend{
			version: func(context.Context, string) (string, error) {
				probes.Add(1)
				return "", nil
			},
			install: func(context.Context, string, string, *slog.Logger) error {
				close(started)
				<-finish
				return errors.New("install failed")
			},
		}}
	r.SetDesired(&pb.ToolchainDesired{Tools: []*pb.ToolSpec{{Name: "copilot", Version: "latest"}}})
	go func() { defer close(done); r.reconcile(context.Background()) }()
	<-started
	n := probes.Load()
	r.refreshInventory(context.Background())
	if probes.Load() != n || r.Status().State != "applying" {
		t.Error("inventory probed a half-installed tool")
	}
	close(finish)
	<-done
}

func TestDisabledDoesNotProbe(t *testing.T) {
	r := &Reconciler{Disabled: true, Backend: probeBackend{
		version: func(context.Context, string) (string, error) {
			t.Fatal("disabled toolchain ran a probe")
			return "", nil
		},
	}}
	r.reconcile(context.Background())
	r.refreshInventory(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	r.Run(ctx)
}

func TestOwnerToolsWithoutProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake.json")
	r := &Reconciler{Backend: &Fake{Path: path}, Log: slog.Default(),
		Running: func() int { return 0 }, Drain: func(bool, string) {}}
	r.SetDesired(&pb.ToolchainDesired{OwnerRevision: 3, Tools: []*pb.ToolSpec{{Name: "node", Version: "22"}}})
	r.reconcile(context.Background())
	if rep := r.Status(); rep.State != "ok" || rep.OwnerRevision != 3 || rep.Versions["node"] != "22.0.0" {
		t.Fatalf("owner tools: %v", rep)
	}
	r.SetDesired(&pb.ToolchainDesired{OwnerRevision: 4})
	r.reconcile(context.Background())
	if rep := r.Status(); rep.State != "none" || rep.OwnerRevision != 4 || rep.Versions["node"] != "22.0.0" {
		t.Fatalf("clear uninstalled tools: %v", rep)
	}
}

func TestSatisfies(t *testing.T) {
	for _, c := range []struct {
		inst, spec string
		ok         bool
	}{
		{"22.11.0", "22", true}, {"22.11.0", "22.11", true}, {"22.11.0", "2", false}, {"22.11.0", "latest", true},
		{"", "latest", false}, {"2.45.0", "2.45.0", true}, {"2.45.0", "v2.45", true}, {"2.450.0", "2.45", false},
	} {
		if Satisfies(c.inst, c.spec) != c.ok {
			t.Errorf("Satisfies(%q, %q) != %v", c.inst, c.spec, c.ok)
		}
	}
	if ParseVersion("git version 2.45.1.windows.1\n") != "2.45.1" || ParseVersion("v22.11.0") != "22.11.0" {
		t.Fatal("ParseVersion")
	}
}

func TestReconcileWaitsForJobsAndDrains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake.json")
	os.WriteFile(path, []byte(`{"tools":{"git":"2.45.0"}}`), 0o600)
	var running atomic.Int32
	running.Store(1)
	var mu sync.Mutex
	var reports []*pb.ToolchainReport
	var drains []bool
	r := &Reconciler{Backend: &Fake{Path: path}, Running: func() int { return int(running.Load()) },
		Drain:  func(on bool, _ string) { mu.Lock(); drains = append(drains, on); mu.Unlock() },
		Report: func(rep *pb.ToolchainReport) { mu.Lock(); reports = append(reports, rep); mu.Unlock() }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	r.SetDesired(&pb.ToolchainDesired{Profile: "dev", Revision: 1, Tools: []*pb.ToolSpec{
		{Name: "git", Version: "2.45"}, {Name: "node", Version: "22"}, {Name: "claude", Version: "latest"}}})
	wait := func(state string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second) // generous: a loaded CI host
		for r.Status().State != state {
			if time.Now().After(deadline) {
				t.Fatalf("state %q, want %q", r.Status().State, state)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	wait("pending")
	mu.Lock()
	if len(drains) != 1 || !drains[0] {
		t.Fatalf("not drained while pending: %v", drains)
	}
	mu.Unlock()
	// Inventory still refreshes during a long wait, without installing or
	// replacing the pending state with an idle/old-profile report.
	if err := os.WriteFile(path, []byte(`{"tools":{"git":"2.45.0","gh":"2.60.0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.refreshInventory(ctx)
	if rep := r.Status(); rep.State != "pending" || rep.Versions["gh"] != "2.60.0" {
		t.Fatalf("inventory while pending: %v", rep)
	}
	running.Store(0)
	wait("ok")
	st := r.Status()
	if st.Versions["node"] != "22.0.0" || st.Versions["claude"] != "99.0.0" || st.Versions["git"] != "2.45.0" {
		t.Fatalf("versions %v", st.Versions)
	}
	mu.Lock()
	if drains[len(drains)-1] {
		t.Fatal("still draining after the update")
	}
	mu.Unlock()
	b, _ := os.ReadFile(path)
	// git was already fine: only node and claude were installed, node first.
	if want := `"node@22.0.0",`; !strings.Contains(string(b), want) || strings.Contains(string(b), `"git@`) {
		t.Fatalf("installs: %s", b)
	}
	// Idempotent: the same profile again installs nothing.
	r.Kick()
	time.Sleep(200 * time.Millisecond)
	if b2, _ := os.ReadFile(path); string(b2) != string(b) {
		t.Fatal("reconcile is not idempotent")
	}
	// A failing tool reports failed.
	os.WriteFile(path, []byte(`{"tools":{},"fail":["uv"]}`), 0o600)
	r.SetDesired(&pb.ToolchainDesired{Profile: "dev", Revision: 2, Tools: []*pb.ToolSpec{{Name: "uv", Version: "latest"}}})
	wait("failed")
	if r.Status().Revision != 2 || r.Status().Error == "" {
		t.Fatalf("%+v", r.Status())
	}
}
