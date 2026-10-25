package toolchain

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

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
