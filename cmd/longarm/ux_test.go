package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/doctor"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Golden strings of the messages that tell the user what to do next.
func TestUXMessages(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	toml := filepath.Join(t.TempDir(), "cfg")
	os.MkdirAll(toml, 0o700)
	os.WriteFile(filepath.Join(toml, client.WorkersFile), nil, 0o600)
	offline := &pb.NodeInfo{Name: "pc1", Os: "windows", Owner: "alice", State: "approved", LastSeen: timestamppb.New(now.Add(-2 * time.Hour))}
	expiry := time.Date(2026, 11, 2, 9, 30, 0, 0, time.Local)
	for _, c := range []struct{ name, got, want string }{
		{"not signed in (windows)", client.NotSignedInHint("windows"),
			"This machine isn't signed in: run `longarm login` (or open Longarm from the Start menu → Account → Sign in)"},
		{"not signed in (linux)", client.NotSignedInHint("linux"), "This machine isn't signed in: run `longarm login`"},
		{"worker only", client.WorkerOnlyError("C:/cfg/tls").Error(),
			"this machine is signed in as a worker only (C:/cfg/tls holds a worker certificate): run `longarm login` to sign in as you"},
		{"unknown worker", client.UnknownWorkerError("x", dir).Error(), "no worker named x: `longarm ls` lists them"},
		{"unknown worker, workers.toml", client.UnknownWorkerError("x", toml).Error(),
			"no worker named x (not in " + filepath.Join(toml, client.WorkersFile) + " either): `longarm ls` lists them"},
		{"did you mean", unknownWorkerError("devbx", dir, []string{"build-linux", "devbox2", "devbox"}).Error(),
			"no worker named devbx: `longarm ls` lists them (did you mean devbox or devbox2?)"},
		{"no workers, hub", noWorkersMsg(true, "hub.example.com:7443").Error(),
			"no workers: hub hub.example.com:7443 lists none you may use; make this or another machine yours: `longarm worker add`"},
		{"no workers, no hub", noWorkersMsg(false, "").Error(), "no workers: this machine isn't signed in to a hub: run `longarm login`"},
		{"offline", offlineNote(offline, now),
			"worker pc1 is offline (the hub last saw it 2h ago); a user-session worker runs only while its owner (alice) is logged on"},
		{"offline linux", offlineNote(&pb.NodeInfo{Name: "b1", Os: "linux", Owner: "alice", LastSeen: timestamppb.New(now.Add(-3 * 24 * time.Hour))}, now),
			"worker b1 is offline (the hub last saw it 3d ago)"},
		{"offline error", offlineError(offline, now).Error(),
			"worker pc1 is offline (the hub last saw it 2h ago); a user-session worker runs only while its owner (alice) is logged on; to try anyway: --force-dial"},
		{"expired", signInExpiredError(expiry).Error(), "your sign-in expired on 2026-11-02 09:30: run `longarm login`"},
		{"renewal failing", renewFailedNote(expiry, expiry.Add(-5*time.Hour), "hub said no"),
			"longarm: your sign-in expires on 2026-11-02 09:30 (in 5h) and renewing it failed (hub said no): run `longarm login`"},
		{"renewal failed early", renewFailedNote(expiry, expiry.Add(-10*24*time.Hour), "x"), "longarm: certificate renewal failed: x"},
		{"git hint", gitCheckoutHint("w1"), "longarm: running in w1's home directory; add `--repo .` to run in a copy of this checkout"},
		{"follow hint", followHint("01J0ABC"), "longarm: follow: longarm logs 01J0ABC -f"},
		{"dial error", dialErrorMessage(`connection error: desc = "transport: Error while dialing: worker pc1 is offline (the hub last saw it 2h ago): all 2 transport legs failed"`),
			"worker pc1 is offline (the hub last saw it 2h ago): all 2 transport legs failed"},
		{"logout", logoutMsg, "there is no separate logout: `longarm leave` signs this machine out.\n" +
			"  Warning: it revokes this machine's sign-in (its certificate) at the hub and deletes it here;\n" +
			"  to use Longarm again you sign in anew with `longarm login`."},
		{"worker add", workerAddHelp("hub:7443", "K7P-Q2M", "ab:cd", "pc2", "alice", "10-29 15:04"),
			"The easy way, on the new machine (signs in with your account):\n" +
				"  Windows: install the Longarm MSI and choose \"This machine runs jobs\", or run: longarmd login hub:7443\n" +
				"  Linux / macOS: longarmd login hub:7443\n" +
				"Or with the code above (for worker pc2, owned by alice, expires 10-29 15:04), on the new machine:\n" +
				"  longarmd join hub:7443 K7P-Q2M --ca-fingerprint ab:cd --name pc2\n" +
				"  then start it - Windows: longarmd session-mode install (runs while you are logged on)\n" +
				"                  Linux / macOS: sudo longarmd service install --user <account>\n" +
				"It joins approved, usable by you only.\n"},
	} {
		if c.got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, c.got, c.want)
		}
	}
	if !errors.Is(client.ErrNotSignedIn, client.ErrNotSignedIn) || exitCodeFor(signInExpiredError(expiry), true) != exitNoPerm {
		t.Error("expired sign-in: want exit 77")
	}
	if exitCodeFor(offlineError(offline, now), true) != exitLongarmError {
		t.Error("offline worker: want exit 255")
	}
}

func TestWorkerOffline(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		n    *pb.NodeInfo
		want bool
	}{
		{&pb.NodeInfo{State: "approved", Online: true}, false},
		{&pb.NodeInfo{State: "approved", LastSeen: timestamppb.New(now.Add(-30 * time.Second))}, false}, // may be restarting
		{&pb.NodeInfo{State: "approved", LastSeen: timestamppb.New(now.Add(-3 * time.Minute))}, true},
		{&pb.NodeInfo{State: "approved"}, true},
		{&pb.NodeInfo{State: "pending"}, false},
	} {
		if got := workerOffline(c.n, now); got != c.want {
			t.Errorf("%v: %v", c.n, got)
		}
	}
}

func TestDidYouMean(t *testing.T) {
	known := []string{"alpha", "beta", "Gamma-Box", "gamma-box2", "w1", "w2", "w10"}
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"gamma", []string{"Gamma-Box", "gamma-box2"}}, // substring, any case
		{"GAMMA-BOX", []string{"Gamma-Box", "gamma-box2"}},
		{"w3", []string{"w1", "w2", "w10"}},
		{"zzzzzz", nil},
		{"alpah", []string{"alpha"}},
	} {
		if got := didYouMean(c.in, known); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.in, got, c.want)
		}
	}
}

func fj(worker, id, owner string, created time.Time) *pb.FleetJob {
	return &pb.FleetJob{Worker: worker, Job: &pb.Job{Spec: &pb.JobSpec{Id: id, Owner: owner, Argv: []string{"make"}},
		CreatedAt: timestamppb.New(created)}}
}

func TestJobIDResolution(t *testing.T) {
	if !fullJobID("01JBQ3Z8X5M2N4P6R8T0V2W4Y6") || fullJobID("01JBQ3Z8") || fullJobID("01NOSUCHJOB0000000000000000") || fullJobID("build-1") {
		t.Error("fullJobID")
	}
	t0 := time.Now()
	rows := []*pb.FleetJob{
		fj("w1", "01JBQ3Z8X5M2N4P6R8T0V2W4Y6", "alice", t0.Add(-3*time.Minute)),
		fj("w2", "01JBQ3Z9AAAAAAAAAAAAK7MD2Q", "bob", t0.Add(-2*time.Minute)),
		fj("w1", "01JBQ400BBBBBBBBBBBBK7MD2Q", "alice", t0.Add(-1*time.Minute)),
		fj("w3", "build", "alice", t0.Add(-10*time.Minute)),
		fj("w3", "build-2", "alice", t0.Add(-9*time.Minute)),
	}
	for _, c := range []struct {
		arg, want string
		amb       int
	}{
		{"01JBQ3Z8", "01JBQ3Z8X5M2N4P6R8T0V2W4Y6", 0}, // prefix
		{"t0v2w4y6", "01JBQ3Z8X5M2N4P6R8T0V2W4Y6", 0}, // suffix, any case (the tray's 8 characters)
		{"build", "build", 0},                         // an exact id wins over a longer one it prefixes
		{"01JBQ", "", 3},                              // ambiguous
		{"K7MD2Q", "", 2},                             // ambiguous suffix
		{"nope", "", 0},
	} {
		m, amb, ok := matchJobID(rows, c.arg)
		switch {
		case c.want != "" && (!ok || m.Job.Spec.Id != c.want):
			t.Errorf("%s: %v %v", c.arg, m, ok)
		case c.want == "" && (ok || len(amb) != c.amb):
			t.Errorf("%s: ok=%v, %d candidates", c.arg, ok, len(amb))
		}
	}
	if n := newestJobOf(rows, "alice"); n.Job.Spec.Id != "01JBQ400BBBBBBBBBBBBK7MD2Q" {
		t.Errorf("last: %s", n.Job.Spec.Id)
	}
	if newestJobOf(rows, "carol") != nil {
		t.Error("last of nobody's")
	}
	_, amb, _ := matchJobID(rows, "K7MD2Q")
	err := ambiguousJobError("K7MD2Q", amb)
	if exitCodeFor(err, true) != exitUsage {
		t.Errorf("ambiguous: exit %d", exitCodeFor(err, true))
	}
	want := "job id K7MD2Q is ambiguous: it matches 2 jobs; give more characters:\n" +
		"  01JBQ400BBBBBBBBBBBBK7MD2Q  w1  unspecified  make\n" +
		"  01JBQ3Z9AAAAAAAAAAAAK7MD2Q  w2  unspecified  make"
	if err.Error() != want {
		t.Errorf("ambiguous:\n%s\nwant\n%s", err, want)
	}
}

func TestLogsSessionArgs(t *testing.T) {
	for _, c := range []struct {
		flag    string
		args    []string
		session bool
		name    string
		rest    []string
		err     bool
	}{
		{"", []string{"w1", "id"}, false, "", []string{"w1", "id"}, false},
		{"", []string{"id"}, false, "", []string{"id"}, false},
		{"", nil, false, "", nil, true},
		{sessionFromArgs, []string{"w1", "s1"}, true, "s1", []string{"w1"}, false}, // logs w1 s1 --session (older form)
		{sessionFromArgs, []string{"s1"}, true, "s1", []string{}, false},           // logs -s s1
		{sessionFromArgs, nil, false, "", nil, true},
		{"s1", nil, true, "s1", nil, false}, // logs --session=s1
		{"s1", []string{"w1"}, true, "s1", []string{"w1"}, false},
		{"s1", []string{"w1", "x"}, false, "", nil, true},
	} {
		session, name, rest, err := logsSessionArgs(c.flag, c.args)
		if (err != nil) != c.err || session != c.session || name != c.name || len(rest) != len(c.rest) {
			t.Errorf("%q %v: %v %q %v %v", c.flag, c.args, session, name, rest, err)
		}
	}
}

func TestJobsView(t *testing.T) {
	for _, c := range []struct {
		o                 jobsViewOpts
		fetch, show       int
		mine, mineDefault bool
	}{
		{jobsViewOpts{}, 21, 20, false, false},                                              // table: newest 20
		{jobsViewOpts{json: true}, 0, 0, false, false},                                      // --json: everything, as before
		{jobsViewOpts{json: true, limit: 5, limitSet: true}, 6, 5, false, false},            // --json --limit
		{jobsViewOpts{admin: true}, 200, 20, true, true},                                    // admins: their own by default
		{jobsViewOpts{admin: true, all: true}, 21, 20, false, false},                        // --all
		{jobsViewOpts{admin: true, json: true}, 0, 0, false, false},                         // --json keeps everything
		{jobsViewOpts{mine: true, json: true}, 0, 0, true, false},                           // filters are additive
		{jobsViewOpts{limitSet: true, limit: 0}, 0, 0, false, false},                        // --limit 0: all
		{jobsViewOpts{id: true, admin: true, limitSet: true, limit: 3}, 0, 0, false, false}, // --id: one job
	} {
		v := planJobsView(c.o)
		if v.fetch != c.fetch || v.show != c.show || v.mine != c.mine || v.mineByDefault != c.mineDefault {
			t.Errorf("%+v: %+v", c.o, v)
		}
	}
	t0 := time.Now()
	var rows []*pb.FleetJob
	for i := 0; i < 25; i++ {
		owner := "alice"
		if i%2 == 1 {
			owner = "bob"
		}
		rows = append(rows, fj("w1", "J"+string(rune('A'+i)), owner, t0.Add(-time.Duration(i)*time.Minute)))
	}
	v := planJobsView(jobsViewOpts{})
	out, more := v.apply(rows, "alice")
	if len(out) != 20 || !more || v.note(more) != "longarm: the newest 20 jobs; --limit 0 for all" {
		t.Errorf("default: %d %v %q", len(out), more, v.note(more))
	}
	v = planJobsView(jobsViewOpts{admin: true})
	out, more = v.apply(rows, "alice")
	if len(out) != 13 || more || v.note(more) != "longarm: only yours; --all for everybody's" {
		t.Errorf("admin: %d %v %q", len(out), more, v.note(more))
	}
	for _, j := range out {
		if j.Job.Spec.Owner != "alice" {
			t.Fatalf("admin default shows %s's job", j.Job.Spec.Owner)
		}
	}
	if out, _ := planJobsView(jobsViewOpts{json: true}).apply(rows, "alice"); len(out) != 25 {
		t.Errorf("--json: %d rows", len(out))
	}
}

// fakeClientEnv drives clientChecks.
type fakeClientEnv struct {
	id        pki.Identity
	notAfter  time.Time
	signInErr error
	hubAddr   string
	hubVer    string
	hubErr    error
	daemonErr error
	fw        *pb.FirewallStatus
	nodes     []*pb.NodeInfo
	legs      map[string]workerLegs
}

func (f *fakeClientEnv) SignIn() (pki.Identity, time.Time, error) {
	return f.id, f.notAfter, f.signInErr
}
func (f *fakeClientEnv) Hub(context.Context) (string, string, error) {
	return f.hubAddr, f.hubVer, f.hubErr
}
func (f *fakeClientEnv) Daemon(context.Context) error { return f.daemonErr }
func (f *fakeClientEnv) Firewall() *pb.FirewallStatus { return f.fw }
func (f *fakeClientEnv) Workers(context.Context) ([]*pb.NodeInfo, map[string]workerLegs) {
	return f.nodes, f.legs
}

func checkByID(cs []doctor.Check, id string) doctor.Check {
	for _, c := range cs {
		if c.ID == id {
			return c
		}
	}
	return doctor.Check{}
}

func TestDoctorClientChecks(t *testing.T) {
	now := time.Now()
	ctx := context.Background()
	healthy := func() *fakeClientEnv {
		return &fakeClientEnv{
			id: pki.Identity{Role: pki.RoleClient, Name: "alice-pc", User: "alice"}, notAfter: now.Add(30 * 24 * time.Hour),
			hubAddr: "hub:7443", hubVer: "0.1.3",
			fw: &pb.FirewallStatus{Ok: true, Program: `C:\longarm.exe`},
			nodes: []*pb.NodeInfo{
				{Name: "box", Owner: "alice", State: "approved", Online: true},
				{Name: "pc", Owner: "alice", State: "approved", Os: "windows", LastSeen: timestamppb.New(now.Add(-2 * time.Hour))},
				{Name: "fleet1", State: "approved", Online: true},
				{Name: "bobs", Owner: "bob", State: "approved", Online: true},
			},
			legs: map[string]workerLegs{"box": {Path: "longnet/direct 3ms", Race: "tcp: ok, longnet: won"}},
		}
	}
	cs := clientChecks(ctx, healthy(), now)
	want := map[string]doctor.Status{"signed-in": doctor.OK, "certificate": doctor.OK, "hub": doctor.OK, "daemon": doctor.OK,
		"firewall": doctor.OK, "worker:box": doctor.OK, "worker:pc": doctor.Fail}
	for id, st := range want {
		if got := checkByID(cs, id); got.Status != st {
			t.Errorf("%s: %+v", id, got)
		}
	}
	if c := checkByID(cs, "worker:fleet1"); c.ID != "" {
		t.Error("a fleet worker is not one of mine")
	}
	if c := checkByID(cs, "worker:pc"); !strings.Contains(c.Detail, "2h ago") || !strings.Contains(c.Fix, "longarmd session-mode start") {
		t.Errorf("offline worker: %+v", c)
	}
	if c := checkByID(cs, "worker:box"); !strings.Contains(c.Detail, "longnet/direct") || !strings.Contains(c.Detail, "tcp: ok") {
		t.Errorf("legs: %+v", c)
	}

	broken := []struct {
		name string
		mod  func(*fakeClientEnv)
		id   string
		st   doctor.Status
		fix  string
	}{
		{"not signed in", func(f *fakeClientEnv) { f.signInErr = client.ErrNotSignedIn }, "signed-in", doctor.Fail, "longarm login"},
		{"worker only", func(f *fakeClientEnv) { f.signInErr = client.WorkerOnlyError("tls") }, "signed-in", doctor.Fail, "longarm login"},
		{"expired", func(f *fakeClientEnv) { f.notAfter = now.Add(-time.Hour) }, "certificate", doctor.Fail, "longarm login"},
		{"expiring", func(f *fakeClientEnv) { f.notAfter = now.Add(10 * time.Hour) }, "certificate", doctor.Warn, "longarm login"},
		{"no hub", func(f *fakeClientEnv) { f.hubAddr = "" }, "hub", doctor.Fail, "longarm login"},
		{"hub down", func(f *fakeClientEnv) { f.hubErr = errors.New("context deadline exceeded") }, "hub", doctor.Fail, "longarm diag --upload"},
		{"hub refuses", func(f *fakeClientEnv) { f.hubErr = errors.New("tls: bad certificate") }, "hub", doctor.Fail, "longarm login"},
		{"daemon", func(f *fakeClientEnv) { f.daemonErr = errors.New("not running") }, "daemon", doctor.Warn, "longarm daemon start"},
		{"firewall", func(f *fakeClientEnv) {
			f.fw = &pb.FirewallStatus{Problem: "a Block rule", Fix: `longarm firewall allow`}
		}, "firewall", doctor.Warn, "longarm firewall allow  # elevated"},
		{"no legs", func(f *fakeClientEnv) { f.legs["box"] = workerLegs{Error: "all 2 transport legs failed"} }, "worker:box", doctor.Fail, "longarm diag --upload"},
	}
	for _, b := range broken {
		f := healthy()
		b.mod(f)
		cs := clientChecks(ctx, f, now)
		c := checkByID(cs, b.id)
		if c.Status != b.st || c.Fix != b.fix {
			t.Errorf("%s: %+v", b.name, c)
		}
		for _, c := range cs {
			if (c.Status == doctor.Fail || c.Status == doctor.Warn) && c.Fix == "" {
				t.Errorf("%s: %s is %s without a next command", b.name, c.ID, c.Status)
			}
		}
	}
	f := healthy()
	f.signInErr = client.ErrNotSignedIn
	if c := checkByID(clientChecks(ctx, f, now), "hub"); c.Status != doctor.Skip {
		t.Errorf("hub without a sign-in: %+v", c)
	}
}

// The command tree: groups, hidden plumbing, aliases, completion.
func TestRootCommand(t *testing.T) {
	root := newRootCmd()
	groups := map[string]bool{}
	for _, g := range root.Groups() {
		groups[g.ID] = true
	}
	for _, c := range root.Commands() {
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if !groups[c.GroupID] {
			t.Errorf("%s: no group", c.Name())
		}
	}
	for _, n := range []string{"daemon", "pool", "proxy", "transport", "firewall", "trust", "telemetry"} {
		c, _, err := root.Find([]string{n})
		if err != nil || !c.Hidden {
			t.Errorf("%s: not a hidden command (%v)", n, err)
		}
	}
	if c, _, err := root.Find([]string{"worker", "rm"}); err != nil || c.Name() != "remove" {
		t.Errorf("worker rm: %v", err)
	}
	if c, _, _ := root.Find([]string{"tail"}); c.Flags().Lookup("from") == nil || c.Flags().Lookup("from-offset") == nil {
		t.Error("tail --from")
	}
	if c, _, _ := root.Find([]string{"logs"}); c.Flags().ShorthandLookup("s") == nil {
		t.Error("logs -s")
	}
	var help bytes.Buffer
	root.SetOut(&help)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"Start here:", "Jobs:", "Sessions:", "Files & ports:", "Your workers:", "Account:", "Admin & troubleshooting:",
		"Getting started:", "longarm doctor"} {
		if !strings.Contains(help.String(), w) {
			t.Errorf("--help without %q", w)
		}
	}
	if strings.Contains(help.String(), "transport ") && strings.Contains(help.String(), "  pool ") {
		t.Error("--help lists plumbing commands")
	}
}

func TestLogoutPointsAtLeave(t *testing.T) {
	err := logoutCmd().RunE(nil, nil)
	if exitCodeFor(err, true) != exitUsage || !strings.Contains(err.Error(), "longarm leave") || !strings.Contains(err.Error(), "revokes") {
		t.Fatalf("logout: %v", err)
	}
}

func TestCompletion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LONGARM_CONFIG_DIR", dir) // newRootCmd sets configDir's default from it
	old := configDir
	configDir = dir
	defer func() { configDir = old }()
	os.WriteFile(filepath.Join(dir, client.WorkersFile), []byte("[[worker]]\nname = \"lab1\"\naddress = \"127.0.0.1:7701\"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, hubCacheFile), []byte(`{"nodes":[{"name":"devbox","state":"approved"},{"name":"gone","state":"revoked"},{"name":"desk"}]}`), 0o600)
	if got := cachedWorkerNames(dir); !slices.Equal(got, []string{"desk", "devbox", "lab1"}) {
		t.Errorf("worker names: %v", got)
	}
	got, dir2 := completeWorkers(1)(&cobra.Command{}, nil, "de")
	if !slices.Equal(got, []string{"desk", "devbox"}) || dir2 != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("complete de: %v", got)
	}
	if got, _ := completeWorkers(1)(&cobra.Command{}, []string{"devbox"}, ""); got != nil {
		t.Errorf("second argument: %v", got)
	}
	oldJobs := recentJobs
	recentJobs = func(context.Context) []*pb.FleetJob {
		return []*pb.FleetJob{fj("devbox", "01JBQ3Z8X5M2N4P6R8T0V2W4Y6", "alice", time.Now())}
	}
	defer func() { recentJobs = oldJobs }()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	got, _ = completeJobArgs(cmd, nil, "")
	want := []string{"last\tyour most recent job", "01JBQ3Z8X5M2N4P6R8T0V2W4Y6\tunspecified on devbox: make", "desk", "devbox", "lab1"}
	if !slices.Equal(got, want) {
		t.Errorf("job args:\n%q\nwant\n%q", got, want)
	}
	got, _ = completeJobArgs(cmd, []string{"devbox"}, "01")
	if !slices.Equal(got, []string{"01JBQ3Z8X5M2N4P6R8T0V2W4Y6\tunspecified on devbox: make"}) {
		t.Errorf("job id after a worker: %q", got)
	}
	oldSess := sessionNames
	sessionNames = func(_ context.Context, w string) []string {
		if w != "devbox" {
			return nil
		}
		return []string{"main\ton devbox", "build\ton devbox"}
	}
	defer func() { sessionNames = oldSess }()

	// through cobra's __complete, as the shell scripts call it
	for _, c := range []struct {
		args []string
		want []string
	}{
		{[]string{"__complete", "attach", "d"}, []string{"desk", "devbox"}},
		{[]string{"__complete", "attach", "devbox", "-s", "b"}, []string{"build\ton devbox"}},
		{[]string{"__complete", "exec", "--on", "l"}, []string{"lab1"}},
		{[]string{"__complete", "logs", "la"}, []string{"last\tyour most recent job", "lab1"}},
	} {
		root := newRootCmd()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetArgs(c.args)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			if !strings.HasPrefix(l, ":") && l != "" {
				lines = append(lines, l)
			}
		}
		if !slices.Equal(lines, c.want) {
			t.Errorf("%v: %q, want %q", c.args, lines, c.want)
		}
	}
}
