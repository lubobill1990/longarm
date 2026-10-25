package tenancy

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func TestAccountName(t *testing.T) {
	if got := AccountName("", "alice"); got != "la-alice" {
		t.Fatalf("got %q", got)
	}
	if got := AccountName("lt01-", "bob"); got != "lt01-bob" {
		t.Fatalf("got %q", got)
	}
	// older certificate names: cleaned, hashed, at most 20 characters,
	// distinct for distinct users, stable
	a, b := AccountName("", "Alice.Laptop"), AccountName("", "alice-laptop2")
	for _, n := range []string{a, b, AccountName("", strings.Repeat("x", 40))} {
		if len(n) > 20 || !accountRE.MatchString(n) {
			t.Errorf("bad account name %q", n)
		}
	}
	if a == AccountName("", "alice.laptop") || a != AccountName("", "Alice.Laptop") {
		t.Errorf("not distinct / stable: %q", a)
	}
}

func TestAccess(t *testing.T) {
	alice := CallerOf(pki.Identity{Role: pki.RoleClient, Name: "alice-pc", User: "alice"}, []string{"t1"})
	admin := CallerOf(pki.Identity{Role: pki.RoleAdmin, Name: "root"}, nil)
	if alice.User != "alice" || alice.Admin || !admin.Admin {
		t.Fatal(alice, admin)
	}
	for _, c := range []struct {
		owner      string
		share      []string
		read, ctrl bool
	}{
		{"alice", nil, true, true},
		{"bob", nil, false, false},
		{"bob", []string{"t1"}, true, false},
		{"bob", []string{"t2"}, false, false},
		{"", nil, false, false},
	} {
		if alice.CanRead(c.owner, c.share) != c.read || alice.CanControl(c.owner) != c.ctrl {
			t.Errorf("alice on %+v", c)
		}
		if !admin.CanRead(c.owner, c.share) || !admin.CanControl(c.owner) {
			t.Errorf("admin on %+v", c)
		}
	}
	if OwnerOf(&pb.JobSpec{SubmittedBy: "client/bob"}) != "bob" || OwnerOf(&pb.JobSpec{Owner: "carol", SubmittedBy: "client/x"}) != "carol" {
		t.Fatal("OwnerOf")
	}
}

func TestResolveShare(t *testing.T) {
	got, err := ResolveShare([]string{"team"}, []string{"t2", "t1"})
	if err != nil || !slices.Equal(got, []string{"t1", "t2"}) {
		t.Fatal(got, err)
	}
	if got, err := ResolveShare([]string{"team:t1", "team=t1"}, []string{"t1"}); err != nil || !slices.Equal(got, []string{"t1"}) {
		t.Fatal(got, err)
	}
	for _, bad := range [][]string{{"team:t9"}, {"everyone"}} {
		if _, err := ResolveShare(bad, []string{"t1"}); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if _, err := ResolveShare([]string{"team"}, nil); err == nil {
		t.Error("team without teams accepted")
	}
}

func TestQuota(t *testing.T) {
	q := MergeQuota(&pb.Quota{MaxJobs: 4, JobMemGb: 2, MaxJobMemGb: 8, MaxJobCpus: 4}, &pb.Quota{MaxJobs: 1})
	if q.MaxJobs != 1 || q.JobMemGb != 2 {
		t.Fatal(q)
	}
	mem, cpu, err := JobLimits(q, 0, 0)
	if err != nil || mem != 2<<30 || cpu != 4 {
		t.Fatal(mem, cpu, err)
	}
	if _, _, err := JobLimits(q, 16<<30, 0); err == nil {
		t.Fatal("over the per-job memory cap")
	}
	if _, _, err := JobLimits(q, 0, 8); err == nil {
		t.Fatal("over the per-job CPU cap")
	}
	if MemGB(0, "3", 2) != 3 || MemGB(1<<30, "3", 2) != 1 || MemGB(0, "", 2) != 2 {
		t.Fatal("MemGB")
	}
}

func TestPolicyPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), PolicyFile)
	p, err := LoadPolicy(path)
	if err != nil || p.Version() != 0 || p.Enabled() {
		t.Fatal(err)
	}
	p.Set(&pb.TenancyPolicy{Version: 3, Enabled: true, DefaultQuota: &pb.Quota{MaxJobsPerWorker: 2},
		Users: []*pb.TenantUser{{Name: "alice", Teams: []string{"t1"}}, {Name: "bob", Disabled: true}}, Purge: []string{"carol"}})
	p, err = LoadPolicy(path)
	if err != nil || p.Version() != 3 || !p.Enabled() {
		t.Fatal(err)
	}
	if !slices.Equal(p.Teams("alice"), []string{"t1"}) || !p.Disabled("bob") || !p.Disabled("carol") || p.Disabled("alice") ||
		p.Quota("alice").MaxJobsPerWorker != 2 {
		t.Fatal(p.Get())
	}
}
