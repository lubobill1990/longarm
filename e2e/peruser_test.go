package e2e

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/tenancy"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Multi-tenancy level 2 end to end (docs/security.md): two users (alice,
// bob; carol shares a team with alice) and an admin share one hub and one
// worker with [isolation] mode = "per-user". The worker runs as LocalSystem
// (Windows: a temporary service) / root (Linux) and creates temporary OS
// accounts, so the test is opt-in: LONGARM_PERUSER_TEST=1 in an elevated
// shell / as root. CI's hosted runners run it.

// requirePerUser skips unless the OS-level per-user tests may run here.
func requirePerUser(t *testing.T) {
	t.Helper()
	if os.Getenv("LONGARM_PERUSER_TEST") != "1" {
		t.Skip("set LONGARM_PERUSER_TEST=1 (Windows: elevated; Linux: root) to run the per-user isolation tests (they create OS accounts)")
	}
	switch runtime.GOOS {
	case "windows":
		if !platform.Elevated() {
			t.Skip("needs an elevated process")
		}
	case "linux":
		if os.Geteuid() != 0 {
			t.Skip("needs root")
		}
	default:
		t.Skip("per-user isolation is Windows and Linux only")
	}
}

// puWorker is a per-user worker started by the test.
type puWorker struct {
	base, dataDir, binDir, helper, prefix string
	port                                  int
	stop                                  func()
	logPath                               string
}

func (w *puWorker) account(user string) string { return tenancy.AccountName(w.prefix, user) }

// newPerUserWorker prepares a world-readable bin dir (the accounts run
// longarmd's helpers and the test helper), a worker data dir with w1's
// certificate and longarmd.toml, and starts longarmd privileged.
func newPerUserWorker(t *testing.T, h *hubEnv, extraToml string) *puWorker {
	return newPerUserWorkerNamed(t, h, "w1", extraToml)
}

// newPerUserWorkerNamed is newPerUserWorker for worker name (other names
// than w1 get a certificate of their own; accounts get their own prefix).
func newPerUserWorkerNamed(t *testing.T, h *hubEnv, name, extraToml string) *puWorker {
	t.Helper()
	w := &puWorker{prefix: "lt" + puRand(2) + "-", port: freePort(t)}
	w.base = perUserBase(t)
	w.binDir = filepath.Join(w.base, "bin")
	w.dataDir = filepath.Join(w.base, name)
	if err := os.MkdirAll(w.binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	longarmd := filepath.Join(w.binDir, exeName("longarmd"))
	w.helper = filepath.Join(w.binDir, exeName("helper"))
	copyExe(t, bins.longarmd, longarmd)
	copyExe(t, bins.helper, w.helper)
	if err := os.MkdirAll(filepath.Join(w.dataDir, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	if name == "w1" {
		for _, f := range []string{"ca.crt", "node.crt", "node.key"} {
			copyExe(t, filepath.Join(h.dataDir, "tls", f), filepath.Join(w.dataDir, "tls", f))
		}
	} else {
		h.mustLongarm(h.client, "admin", "issue", "--role", "worker", "--name", name, "--out", filepath.Join(w.dataDir, "tls"))
	}
	cfg := fmt.Sprintf(`listen = ["tcp://127.0.0.1:%d"]
[hub]
address = %q
remote_jobs = "all"
[isolation]
mode = "per-user"
account_prefix = %q
[gc]
disabled = true
`, w.port, h.addr(), w.prefix) + extraToml
	if err := os.WriteFile(filepath.Join(w.dataDir, "longarmd.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	// Accounts and data go whatever happens (registered before the start:
	// cleanups run in reverse, so the worker stops first).
	t.Cleanup(func() {
		for _, u := range []string{"alice", "bob", "carol", "root"} {
			if err := platform.DeleteAccount(w.account(u)); err != nil {
				t.Logf("delete account %s: %v", w.account(u), err)
			}
		}
		for i := 0; i < 20; i++ {
			if os.RemoveAll(w.base) == nil {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	})
	w.stop = startPerUserWorker(t, w, longarmd)
	t.Cleanup(w.stop)
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(w.dataDir, "longarmd.endpoints")); err == nil {
			return w
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(w.logPath)
			t.Fatalf("per-user longarmd did not start:\n%s", b)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func copyExe(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	writeExe(t, dst, b)
}

// userDir creates a client config dir with a certificate of user (device
// name), pointing at the per-user worker and the hub.
func userDir(t *testing.T, h *hubEnv, w *puWorker, name, user string) string {
	t.Helper()
	dir := filepath.Join(h.root, "cfg-"+name)
	if dir == "" || name == "" {
		t.Fatal("bad user dir")
	}
	args := []string{"admin", "issue", "--role", "client", "--name", name, "--out", filepath.Join(dir, "tls"), "--ca-dir", filepath.Join(h.client, "ca")}
	if user != "" {
		args = append(args, "--user", user)
	}
	h.mustLongarm(h.client, args...)
	writeClientConfig(t, h, w, dir)
	return dir
}

func writeClientConfig(t *testing.T, h *hubEnv, w *puWorker, dir string) {
	t.Helper()
	workers := fmt.Sprintf("[[worker]]\nname = \"w1\"\naddress = \"127.0.0.1:%d\"\ntransport = \"tcp\"\n", w.port)
	if err := os.WriteFile(filepath.Join(dir, "workers.toml"), []byte(workers), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hub.toml"), []byte(fmt.Sprintf("address = %q\n", h.addr())), 0o600); err != nil {
		t.Fatal(err)
	}
}

// dialAs connects to w1 with the certificate in cfgDir.
func dialAs(t *testing.T, cfgDir string) *client.Conn {
	t.Helper()
	cfg, err := client.LoadConfig(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := client.NewDialer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	w, err := cfg.Worker("w1")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := d.Dial(w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(); d.Close() })
	return conn
}

func ctxT(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// execAs runs argv on w1 through conn; it returns combined output and the
// exit code.
func execAs(t *testing.T, conn *client.Conn, argv ...string) (string, int32) {
	t.Helper()
	r, err := conn.Exec().Run(ctxT(t), &pb.ExecRequest{Argv: argv, Output: pb.OutputMode_OUTPUT_MODE_COMBINED,
		Timeout: durationpb.New(time.Minute)})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	if r.Error != "" && r.ExitCode == -1 {
		t.Fatalf("exec %v: %s", argv, r.Error)
	}
	return string(r.Combined), r.ExitCode
}

func wantCode(t *testing.T, err error, codes ...codes.Code) {
	t.Helper()
	for _, c := range codes {
		if status.Code(err) == c {
			return
		}
	}
	t.Fatalf("want %v, got %v", codes, err)
}

// readLog reads a job's whole log through conn.
func readLog(t *testing.T, conn *client.Conn, id string) (string, error) {
	t.Helper()
	st, err := conn.Jobs().Logs(ctxT(t), &pb.LogsRequest{Id: id})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for {
		c, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return b.String(), nil
		}
		if err != nil {
			return b.String(), err
		}
		b.Write(c.Data)
	}
}

func readFile(t *testing.T, conn *client.Conn, path string) (string, error) {
	t.Helper()
	st, err := conn.Files().Read(ctxT(t), &pb.FileReadRequest{Path: path})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for {
		c, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return b.String(), nil
		}
		if err != nil {
			return "", err
		}
		b.Write(c.Data)
	}
}

func submitAs(t *testing.T, conn *client.Conn, id string, argv []string, secrets map[string]string, share ...string) (*pb.Job, error) {
	t.Helper()
	return conn.Jobs().Submit(ctxT(t), &pb.SubmitRequest{Spec: &pb.JobSpec{Id: id, Argv: argv}, SecretEnv: secrets, Share: share})
}

var homeRE = regexp.MustCompile(`user (\S+) home (.+)`)

func TestPerUserIsolation(t *testing.T) {
	requirePerUser(t)
	h := newHubEnv(t, "")
	w := newPerUserWorker(t, h, "")
	alice, root := h.client, h.admin
	writeClientConfig(t, h, w, alice)
	writeClientConfig(t, h, w, root)
	bob := userDir(t, h, w, "bob-laptop", "bob")
	carol := userDir(t, h, w, "carol", "")
	h.waitNode(t, "w1", "online", func(n map[string]any) bool { return n["online"] == true })

	// users, a team (alice + carol), a per-worker quota for bob
	h.mustLongarm(root, "admin", "user", "add", "alice", "--team", "t1", "--no-code")
	h.mustLongarm(root, "admin", "user", "add", "bob", "--no-code", "--max-jobs-per-worker", "1")
	h.mustLongarm(root, "admin", "user", "add", "carol", "--team", "t1", "--no-code")
	waitFor(t, "the tenancy policy on the worker", 30*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(w.dataDir, tenancy.PolicyFile))
		return err == nil && strings.Contains(string(b), "carol") && strings.Contains(string(b), "maxJobsPerWorker")
	})

	ca, cb, cc, cr := dialAs(t, alice), dialAs(t, bob), dialAs(t, carol), dialAs(t, root)

	// --- identities: every user runs as its own account
	info, err := ca.Node().Info(ctxT(t), &pb.InfoRequest{})
	if err != nil || info.Isolation != tenancy.PerUser || info.Account != w.account("alice") || info.CallerUser != "alice" {
		t.Fatalf("info %+v %v", info, err)
	}
	out, _ := execAs(t, ca, w.helper, "whoami")
	m := homeRE.FindStringSubmatch(out)
	if m == nil || !strings.Contains(strings.ToLower(m[1]), w.account("alice")) {
		t.Fatalf("alice runs as %q", out)
	}
	homeA := strings.TrimSpace(m[2])
	out, _ = execAs(t, cb, w.helper, "whoami")
	if m := homeRE.FindStringSubmatch(out); m == nil || !strings.Contains(strings.ToLower(m[1]), w.account("bob")) {
		t.Fatalf("bob runs as %q", out)
	}
	secretFile := filepath.Join(homeA, "secret.txt")
	if out, code := execAs(t, ca, w.helper, "write", secretFile, "topsecret"); code != 0 {
		t.Fatalf("alice writes her file: %s", out)
	}

	// --- forwarded Copilot credentials: a private COPILOT_HOME in alice's
	// space, writable by her account (the fake agent caches the token
	// there), not readable by bob's, gone with the job
	fakeCop := filepath.Join(w.binDir, exeName("copilot"))
	copyExe(t, w.helper, fakeCop)
	jobAg := "PUAGENT" + puRand(4)
	if _, err := ca.Jobs().Submit(ctxT(t), &pb.SubmitRequest{Spec: &pb.JobSpec{Id: jobAg, Argv: []string{fakeCop}},
		SecretEnv: map[string]string{"COPILOT_GITHUB_TOKEN": e2eCopilotValue}, AgentAuth: &pb.AgentAuth{Agents: []string{"copilot"}, CopilotHome: true}}); err != nil {
		t.Fatal(err)
	}
	var agLog string
	waitFor(t, "the agent job", 60*time.Second, func() bool {
		agLog, _ = readLog(t, ca, jobAg)
		return strings.Contains(agLog, "copilot_home")
	})
	time.Sleep(500 * time.Millisecond) // the cache write
	agLog, _ = readLog(t, ca, jobAg)
	_, after, _ := strings.Cut(agLog, "copilot_home ")
	agHome := strings.TrimSpace(strings.SplitN(after, "\n", 2)[0])
	if !strings.HasPrefix(agHome, w.dataDir) || filepath.Base(filepath.Dir(agHome)) != jobs.AgentHomeDir ||
		strings.Contains(agLog, "cache error") || strings.Contains(agLog, e2eCopilotValue) {
		t.Fatalf("per-user agent job:\n%s", agLog)
	}
	if out, code := execAs(t, cb, w.helper, "cat", filepath.Join(agHome, "token-cache.json")); code == 0 || strings.Contains(out, e2eCopilotValue) {
		t.Fatalf("bob reads alice's COPILOT_HOME: %d %s", code, out)
	}

	// --- alice's job (a dev server with a secret), a shared job, a session
	jobA := "PUJOBA" + puRand(4)
	if _, err := submitAs(t, ca, jobA, []string{w.helper, "serve", "300"}, map[string]string{"A_SECRET": "s3cr3t-alice-value"}); err != nil {
		t.Fatal(err)
	}
	var portA int
	waitFor(t, "alice's server port", 60*time.Second, func() bool {
		l, _ := readLog(t, ca, jobA)
		if m := regexp.MustCompile(`(?m)^port (\d+)`).FindStringSubmatch(l); m != nil {
			portA, _ = strconv.Atoi(m[1])
			return true
		}
		return false
	})
	jobShared := "PUJOBS" + puRand(4)
	if _, err := submitAs(t, ca, jobShared, []string{w.helper, "print", "3", "0", "0"}, nil, "team"); err != nil {
		t.Fatal(err)
	}
	sess := attachGo(t, ca, &pb.AttachOpen{Name: "dev", Argv: []string{w.helper, "repl"}})
	sess.waitFor("repl ready")
	sess.detach()

	// --- bob sees nothing of alice's through the API
	jl, err := cb.Jobs().List(ctxT(t), &pb.ListJobsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jl.Jobs {
		if j.Spec.Owner != "bob" {
			t.Fatalf("bob lists %s of %s", j.Spec.Id, j.Spec.Owner)
		}
	}
	_, err = cb.Jobs().Get(ctxT(t), &pb.GetJobRequest{Id: jobA})
	wantCode(t, err, codes.NotFound)
	_, err = readLog(t, cb, jobA)
	wantCode(t, err, codes.NotFound)
	_, err = cb.Jobs().Kill(ctxT(t), &pb.KillRequest{Id: jobA})
	wantCode(t, err, codes.NotFound)
	if ws, err := cb.Jobs().Wait(ctxT(t), &pb.WaitRequest{Id: jobA}); err == nil {
		_, err = ws.Recv()
		wantCode(t, err, codes.NotFound)
	}
	sl, err := cb.Sessions().List(ctxT(t), &pb.ListSessionsRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sl.Sessions {
		t.Fatalf("bob lists session %s of %s", s.Name, s.Owner)
	}
	_, err = cb.Sessions().Kill(ctxT(t), &pb.KillSessionRequest{Name: "dev", Owner: "alice"})
	wantCode(t, err, codes.PermissionDenied)
	_, err = cb.Sessions().Kill(ctxT(t), &pb.KillSessionRequest{Name: "dev"})
	wantCode(t, err, codes.NotFound) // bob's namespace has no "dev"
	if as, err := cb.Sessions().Attach(ctxT(t)); err == nil {
		as.Send(&pb.AttachInput{Msg: &pb.AttachInput_Open{Open: &pb.AttachOpen{Name: "dev", NoCreate: true}}})
		_, err = as.Recv()
		wantCode(t, err, codes.NotFound)
	}
	if as, err := cb.Sessions().Attach(ctxT(t)); err == nil {
		as.Send(&pb.AttachInput{Msg: &pb.AttachInput_Open{Open: &pb.AttachOpen{Name: "dev", Owner: "alice", NoCreate: true}}})
		_, err = as.Recv()
		wantCode(t, err, codes.PermissionDenied)
	}

	// --- ... nor through the Files API (which runs as bob's account)
	if got, err := readFile(t, ca, secretFile); err != nil || got != "topsecret" {
		t.Fatalf("alice reads her file: %q %v", got, err)
	}
	_, err = readFile(t, cb, secretFile)
	wantCode(t, err, codes.PermissionDenied, codes.NotFound)
	_, err = cb.Files().List(ctxT(t), &pb.FileListRequest{Path: homeA})
	wantCode(t, err, codes.PermissionDenied, codes.NotFound)
	if wst, err := cb.Files().Write(ctxT(t)); err == nil {
		wst.Send(&pb.FileWriteRequest{Msg: &pb.FileWriteRequest_Header{Header: &pb.FileWriteHeader{Path: filepath.Join(homeA, "planted.txt"), Size: 1}}})
		wst.Send(&pb.FileWriteRequest{Msg: &pb.FileWriteRequest_Data{Data: []byte("x")}})
		_, err = wst.CloseAndRecv()
		wantCode(t, err, codes.PermissionDenied, codes.NotFound)
	}

	// --- ... nor from inside its own jobs: alice's home, job dir, data,
	// process environment
	ra, err := cr.Jobs().Get(ctxT(t), &pb.GetJobRequest{Id: jobA}) // admins see everything
	if err != nil {
		t.Fatalf("admin get: %v", err)
	}
	if ra.Spec.Owner != "alice" {
		t.Fatalf("owner %q", ra.Spec.Owner)
	}
	usersDir := filepath.Join(w.dataDir, "users")
	spaceA := filepath.Join(usersDir, w.account("alice"))
	for _, argv := range [][]string{
		{w.helper, "cat", secretFile},
		{w.helper, "ls", homeA},
		{w.helper, "ls", spaceA},
		{w.helper, "ls", filepath.Join(spaceA, "worktrees")},
		{w.helper, "ls", filepath.Join(spaceA, "mirrors")},
		{w.helper, "cat", filepath.Join(spaceA, "jobs", jobA, "out.log")},
		{w.helper, "cat", filepath.Join(w.dataDir, "tls", "node.key")},
		{w.helper, "ls", usersDir},
		{w.helper, "procenv", strconv.Itoa(int(ra.Pid))},
	} {
		if out, code := execAs(t, cb, argv...); code == 0 {
			t.Errorf("bob's process could %v:\n%s", argv[1:], out)
		}
	}
	// positive control: alice reads her own process
	if out, code := execAs(t, ca, w.helper, "procenv", strconv.Itoa(int(ra.Pid))); code != 0 {
		t.Errorf("alice cannot open her own process: %s", out)
	} else if runtime.GOOS == "linux" && !strings.Contains(out, "s3cr3t-alice-value") {
		t.Errorf("alice's job environment lacks the secret:\n%s", out)
	}

	// --- forwards: only to ports of one's own processes
	if err := forwardEcho(t, ca, portA); err != nil {
		t.Fatalf("alice forwards to her server: %v", err)
	}
	if err := forwardEcho(t, cb, portA); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("bob forwarded to alice's port: %v", err)
	}

	// --- loopback between the accounts (Linux: [isolation] loopback,
	// nftables): bob's processes can't connect to alice's server directly
	if runtime.GOOS == "linux" {
		checkLoopbackIsolation(t, w, ca, cb, jobA, portA)
	}

	// --- team sharing: carol (team t1) reads the shared job, not the other
	waitFor(t, "shared job done", 30*time.Second, func() bool {
		j, err := ca.Jobs().Get(ctxT(t), &pb.GetJobRequest{Id: jobShared})
		return err == nil && j.State == pb.JobState_JOB_STATE_DONE
	})
	if l, err := readLog(t, cc, jobShared); err != nil || !strings.Contains(l, "line 3") {
		t.Fatalf("carol reads the shared job: %q %v", l, err)
	}
	_, err = cc.Jobs().Kill(ctxT(t), &pb.KillRequest{Id: jobShared})
	wantCode(t, err, codes.NotFound)
	_, err = cc.Jobs().Get(ctxT(t), &pb.GetJobRequest{Id: jobA})
	wantCode(t, err, codes.NotFound)
	_, err = cb.Jobs().Get(ctxT(t), &pb.GetJobRequest{Id: jobShared})
	wantCode(t, err, codes.NotFound)

	// --- quotas: bob may run one job here
	jobB := "PUJOBB" + puRand(4)
	if _, err := submitAs(t, cb, jobB, []string{w.helper, "sleep", "120"}, nil); err != nil {
		t.Fatal(err)
	}
	_, err = submitAs(t, cb, "PUJOBB2"+puRand(4), []string{w.helper, "sleep", "1"}, nil)
	wantCode(t, err, codes.ResourceExhausted)
	// bob cannot take alice's job id
	_, err = submitAs(t, cb, jobA, []string{w.helper, "sleep", "1"}, nil)
	wantCode(t, err, codes.AlreadyExists)

	// --- the hub's views follow the same rules
	waitFor(t, "the hub indexes the jobs", 30*time.Second, func() bool {
		return strings.Contains(h.mustLongarm(root, "jobs", "--json"), jobB)
	})
	if out := h.mustLongarm(bob, "jobs", "--json"); strings.Contains(out, jobA) || strings.Contains(out, jobShared) || !strings.Contains(out, jobB) {
		t.Fatalf("bob's hub job list:\n%s", out)
	}
	if out := h.mustLongarm(carol, "jobs", "--json"); !strings.Contains(out, jobShared) || strings.Contains(out, jobA) {
		t.Fatalf("carol's hub job list:\n%s", out)
	}
	if r := h.longarm(bob, "admin", "audit"); r.code == 0 {
		t.Fatalf("bob read the audit log:\n%s", r.stdout)
	}

	// --- mirrors are per user
	if gitOK() {
		repo := filepath.Join(h.root, "repo")
		gitInit(t, repo)
		h.mustLongarm(alice, "sync", repo, "w1:work/proj")
		key := filepath.Base(repo)
		ra, err := ca.Workspace().Refs(ctxT(t), &pb.RefsRequest{Repo: key})
		if err != nil || !ra.Exists || len(ra.Commits) == 0 {
			t.Fatalf("alice's mirror: %+v %v", ra, err)
		}
		rb, err := cb.Workspace().Refs(ctxT(t), &pb.RefsRequest{Repo: key})
		if err != nil || rb.Exists || len(rb.Commits) != 0 {
			t.Fatalf("bob sees alice's mirror: %+v %v", rb, err)
		}
		if out, code := execAs(t, cb, w.helper, "ls", filepath.Join(homeA, "work", "proj")); code == 0 {
			t.Fatalf("bob lists alice's worktree:\n%s", out)
		}
	}

	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
			cg := filepath.Join("/sys/fs/cgroup/longarm", w.account("alice"), "jobs-"+jobA)
			if _, err := os.Stat(cg); err != nil {
				t.Errorf("alice's job has no cgroup %s: %v", cg, err)
			}
		}
	}

	// --- admins act on everything
	if _, err := cr.Jobs().Kill(ctxT(t), &pb.KillRequest{Id: jobA}); err != nil {
		t.Fatalf("admin kill: %v", err)
	}

	// --- removing bob with --purge deletes his account and data on the worker
	h.mustLongarm(root, "admin", "user", "remove", "bob", "--purge")
	waitFor(t, "bob's account and data purged", 90*time.Second, func() bool {
		_, err := platform.LookupAccount(w.account("bob"))
		_, serr := os.Stat(filepath.Join(usersDir, w.account("bob")))
		return err != nil && errors.Is(serr, os.ErrNotExist)
	})
	if _, err := cb.Node().Info(ctxT(t), &pb.InfoRequest{}); status.Code(err) != codes.Unauthenticated && status.Code(err) != codes.PermissionDenied && status.Code(err) != codes.Unavailable {
		t.Fatalf("bob's revoked certificate still works: %v", err)
	}
}

// checkLoopbackIsolation: with the default [isolation] loopback = "isolate"
// longarmd installed the nftables table inet longarm; bob's process can't
// connect to alice's server (job jobA, helper serve) on 127.0.0.1 and ::1,
// alice's own process and root (the test, longarmd's forward) can. Skipped
// with a warning where nftables is missing or unusable.
func checkLoopbackIsolation(t *testing.T, w *puWorker, ca, cb *client.Conn, jobA string, portA int) {
	t.Helper()
	nft, err := exec.LookPath("nft")
	if err != nil {
		for _, p := range []string{"/usr/sbin/nft", "/sbin/nft"} {
			if _, serr := os.Stat(p); serr == nil {
				nft, err = p, nil
				break
			}
		}
	}
	if err != nil {
		t.Log("warning: nft not installed - loopback isolation between the accounts NOT tested")
		return
	}
	if out, err := exec.Command(nft, "list", "tables").CombinedOutput(); err != nil {
		t.Logf("warning: nftables unusable here (%v: %s) - loopback isolation between the accounts NOT tested", err, out)
		return
	}
	t.Cleanup(func() {
		exec.Command(nft, "delete", "table", "inet", "longarm").Run()
	})
	out, err := exec.Command(nft, "list", "table", "inet", "longarm").CombinedOutput()
	if err != nil {
		t.Fatalf("longarmd did not install the loopback rules: %v: %s", err, out)
	}
	for _, u := range []string{"alice", "bob"} {
		a, err := platform.LookupAccount(w.account(u))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), fmt.Sprintf("skuid %d ", a.UID)) && !strings.Contains(string(out), a.Name) {
			t.Fatalf("the loopback rules lack %s (uid %d):\n%s", a.Name, a.UID, out)
		}
	}
	l, err := readLog(t, ca, jobA)
	if err != nil {
		t.Fatal(err)
	}
	port6 := ""
	if m := regexp.MustCompile(`(?m)^v6port (\d+)`).FindStringSubmatch(l); m != nil {
		port6 = m[1]
	}
	type target struct{ host, port string }
	targets := []target{{"127.0.0.1", strconv.Itoa(portA)}}
	if port6 != "" {
		targets = append(targets, target{"::1", port6})
	} else {
		t.Log("warning: no IPv6 loopback here - ::1 NOT tested")
	}
	for _, tg := range targets {
		// alice reaches her own server
		if out, code := execAs(t, ca, w.helper, "connect", tg.port, tg.host); code != 0 || !strings.Contains(out, "echo hi") {
			t.Errorf("alice cannot connect to her own server on [%s]:%s: %s", tg.host, tg.port, out)
		}
		// bob does not
		start := time.Now()
		if out, code := execAs(t, cb, w.helper, "connect", tg.port, tg.host); code == 0 {
			t.Errorf("bob's process connected to alice's server on [%s]:%s:\n%s", tg.host, tg.port, out)
		} else {
			t.Logf("bob -> alice [%s]:%s refused after %v: %s", tg.host, tg.port, time.Since(start).Round(time.Millisecond), strings.TrimSpace(out))
		}
		// root (this test) does
		c, err := net.DialTimeout("tcp", net.JoinHostPort(tg.host, tg.port), 5*time.Second)
		if err != nil {
			t.Errorf("root cannot connect to alice's server on [%s]:%s: %v", tg.host, tg.port, err)
			continue
		}
		c.SetDeadline(time.Now().Add(10 * time.Second))
		c.Write([]byte("hi"))
		b, _ := io.ReadAll(c)
		c.Close()
		if !strings.Contains(string(b), "echo hi") {
			t.Errorf("root's connection to alice's server on [%s]:%s got %q", tg.host, tg.port, b)
		}
	}
	// longarmd's forward (root) to alice's port still works
	if err := forwardEcho(t, ca, portA); err != nil {
		t.Errorf("alice's forward with the loopback rules: %v", err)
	}
}

// forwardEcho opens a forward to 127.0.0.1:port, sends "hi" and expects
// the helper server's echo.
func forwardEcho(t *testing.T, conn *client.Conn, port int) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := conn.Forward().Open(ctx)
	if err != nil {
		return err
	}
	if err := st.Send(&pb.ForwardFrame{Target: strconv.Itoa(port), Data: []byte("hi"), Eof: true}); err != nil {
		return err
	}
	var got []byte
	for {
		f, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		got = append(got, f.Data...)
		if f.Eof {
			break
		}
	}
	if !strings.Contains(string(got), "echo hi") {
		return fmt.Errorf("forward got %q", got)
	}
	return nil
}

func gitOK() bool { return exec.Command("git", "--version").Run() == nil }

func gitInit(t *testing.T, dir string) {
	t.Helper()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("private code of alice\n"), 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "private"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func puRand(n int) string {
	b := make([]byte, n)
	crand.Read(b)
	return hex.EncodeToString(b)
}
