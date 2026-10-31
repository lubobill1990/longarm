package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/metrics"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/shim"
	"github.com/lubobill1990/longarm/internal/steer"
	"github.com/lubobill1990/longarm/internal/tenancy"
	"github.com/lubobill1990/longarm/internal/userspace"
	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The test binary doubles as the job shim and as a sleeping job.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == workspace.CredHelperArg { // git's credential helper
		os.Exit(workspace.CredentialHelperMain(os.Args[2:], os.Stdin, os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == steer.HookArg { // a steerable run's hook
		os.Exit(steer.HookMain(os.Args[2:], os.Stdin))
	}
	if agent := os.Getenv("LONGARM_TEST_FAKE_AGENT"); agent != "" { // steer_test.go
		if len(os.Args) > 1 && os.Args[1] == "conhost" {
			os.Exit(shim.ConsoleHost(os.Args[2:]))
		}
		os.Exit(fakeAgent(agent))
	}
	if os.Getenv("LONGARM_TEST_SLEEP") != "" {
		time.Sleep(2 * time.Minute)
		os.Exit(0)
	}
	if os.Getenv("LONGARM_TEST_STDIN_SHA") != "" { // a job: size and hash of its stdin
		b, _ := io.ReadAll(os.Stdin)
		fmt.Printf("%d %x\n", len(b), sha256.Sum256(b))
		os.Exit(0)
	}
	if os.Getenv("LONGARM_TEST_ENV_DUMP") != "" { // a job: its environment
		fmt.Println(strings.Join(os.Environ(), "\n"))
		os.Exit(0)
	}
	// (jobs inherit LONGARM_TEST_SHIM: the shim checks come last)
	if dir := os.Getenv("LONGARM_TEST_SHIM"); dir != "" {
		shim.SteerTimings = fastSteer
		os.Exit(shim.Main(dir, os.Stdin))
	}
	os.Exit(m.Run())
}

// authzEnv is a worker (shared isolation) whose hub separates users:
// alice and carol share team t1, bob is on his own, root is an admin.
type authzEnv struct {
	srv                     *Server
	alice, bob, carol, root *grpc.ClientConn
	// dial connects with a new certificate of the test CA
	dial func(o pki.IssueOptions) *grpc.ClientConn
	ca   *pki.CA
	addr string // the server's
}

func issue(t *testing.T, ca *pki.CA, dir string, o pki.IssueOptions) *pki.Bundle {
	t.Helper()
	certPEM, keyPEM, err := ca.Issue(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteBundle(dir, ca.CertPEM, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	b, err := pki.LoadBundle(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newAuthzEnv(t *testing.T) *authzEnv {
	t.Helper()
	return newAuthzEnvWith(t, nil)
}

// newAuthzEnvWith is newAuthzEnv with cfg adjusting the spaces' config
// (data dir already set).
func newAuthzEnvWith(t *testing.T, cfg func(*userspace.Config)) *authzEnv {
	t.Helper()
	root := t.TempDir()
	ca, err := pki.InitCA(filepath.Join(root, "ca"), "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	wb := issue(t, ca, filepath.Join(root, "w1"), pki.IssueOptions{Role: pki.RoleWorker, Name: "w1", Hosts: []string{"127.0.0.1"}})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	exe, _ := os.Executable()
	sc := userspace.Config{Mode: tenancy.Shared, DataDir: filepath.Join(root, "data"),
		ShimCommand: func(dir string) *exec.Cmd {
			c := exec.Command(exe)
			c.Env = append(os.Environ(), "LONGARM_TEST_SHIM="+dir)
			return c
		}}
	if cfg != nil {
		cfg(&sc)
	}
	spaces, err := userspace.Open(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	policy, _ := tenancy.LoadPolicy("")
	policy.Set(&pb.TenancyPolicy{Version: 1, Enabled: true, Users: []*pb.TenantUser{
		{Name: "alice", Teams: []string{"t1"}}, {Name: "bob"}, {Name: "carol", Teams: []string{"t1"}},
	}})
	revoked, _ := pki.LoadRevocationList("")
	s := &Server{Name: "w1", DataDir: filepath.Join(root, "data"), StartedAt: time.Now(), Spaces: spaces, Policy: policy,
		Sampler: &metrics.Sampler{Interval: time.Second, JobsFn: spaces.RunningJobs, DiskPath: root},
		Audit:   NewAudit(filepath.Join(root, "audit.log")), Revoked: revoked, Shutdown: func() {},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		GC:  func(context.Context, *pb.GcRequest) (*pb.GcResponse, error) { return &pb.GcResponse{}, nil }}
	tc, err := wb.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	g := s.GRPC(tc)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go g.Serve(ln)
	t.Cleanup(func() {
		g.Stop()
		type proc struct {
			pid   int
			start uint64
		}
		var procs []proc
		for _, sp := range spaces.All() {
			for _, j := range append(sp.Jobs.List(nil, 0), sp.Sessions.List(nil, 0)...) {
				procs = append(procs, proc{j.Status.ShimPID, j.Status.ShimStart}, proc{j.Status.PID, j.Status.PIDStart})
				if !j.Status.State.Terminal() {
					sp.Jobs.Kill(context.Background(), j.Spec.ID)
					sp.Sessions.Kill(context.Background(), j.Spec.ID)
				}
			}
		}
		// The shims write the jobs' final status into the data dir: wait
		// for them (and the jobs) to exit before t.TempDir's RemoveAll
		// (macOS: "unlinkat .../jobs/JOBA: directory not empty").
		deadline := time.Now().Add(30 * time.Second)
		for _, p := range procs {
			for p.pid > 0 && platform.ProcessAlive(p.pid, p.start) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
		}
	})
	dial := func(o pki.IssueOptions) *grpc.ClientConn {
		b := issue(t, ca, filepath.Join(root, "c-"+o.Name), o)
		conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(b.ClientConfig("w1"))))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	return &authzEnv{srv: s, dial: dial, ca: ca, addr: ln.Addr().String(),
		alice: dial(pki.IssueOptions{Role: pki.RoleClient, Name: "alice"}),
		bob:   dial(pki.IssueOptions{Role: pki.RoleClient, Name: "bob-laptop", User: "bob"}),
		carol: dial(pki.IssueOptions{Role: pki.RoleClient, Name: "carol"}),
		root:  dial(pki.IssueOptions{Role: pki.RoleAdmin, Name: "root"}),
	}
}

func cctx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func sleepJob(t *testing.T, conn *grpc.ClientConn, id string, share ...string) {
	t.Helper()
	exe, _ := os.Executable()
	_, err := pb.NewJobsClient(conn).Submit(cctx(t), &pb.SubmitRequest{Share: share,
		Spec: &pb.JobSpec{Id: id, Argv: []string{exe}, Env: map[string]string{"LONGARM_TEST_SLEEP": "1"}}})
	if err != nil {
		t.Fatal(err)
	}
}

// recvErr returns the first error of a server stream (nil once it ends).
func recvErr[T any](st grpc.ServerStreamingClient[T], err error) error {
	if err != nil {
		return err
	}
	for {
		if _, err := st.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// attachErr opens a session attach and returns its first error (nil once
// it started).
func attachErr(ctx context.Context, conn *grpc.ClientConn, o *pb.AttachOpen) error {
	st, err := pb.NewSessionsClient(conn).Attach(ctx)
	if err != nil {
		return err
	}
	if err := st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Open{Open: o}}); err != nil {
		return err
	}
	m, err := st.Recv()
	if err != nil {
		return err
	}
	if m.GetStarted() == nil {
		return errors.New("no started message")
	}
	st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Detach{Detach: true}})
	st.CloseSend()
	return nil
}

// TestAuthzTable checks every worker RPC for the job's / session's owner,
// another user, a member of a team the job is shared with, and an admin.
// Shared isolation with a separating hub: ownership is enforced on jobs,
// sessions and metrics; files, exec, forwards and workspaces are anybody's
// with the client role (no OS separation without per-user isolation - the
// per-user e2e test covers those).
func TestAuthzTable(t *testing.T) {
	e := newAuthzEnv(t)
	sleepJob(t, e.alice, "JOBA")         // private
	sleepJob(t, e.alice, "JOBS", "team") // shared with t1 (carol)
	exe, _ := os.Executable()
	if err := attachErr(cctx(t), e.alice, &pb.AttachOpen{Name: "dev", Argv: []string{exe},
		Env: map[string]string{"LONGARM_TEST_SLEEP": "1"}}); err != nil {
		t.Fatalf("alice opens her session: %v", err)
	}

	type who struct {
		name string
		conn *grpc.ClientConn
	}
	callers := []who{{"owner", e.alice}, {"other", e.bob}, {"team", e.carol}, {"admin", e.root}}
	ok := codes.OK
	nf, pd := codes.NotFound, codes.PermissionDenied
	type row struct {
		call func(ctx context.Context, c *grpc.ClientConn) error
		// expected code for owner, other, team member, admin
		want [4]codes.Code
	}
	jobs := func(c *grpc.ClientConn) pb.JobsClient { return pb.NewJobsClient(c) }
	sess := func(c *grpc.ClientConn) pb.SessionsClient { return pb.NewSessionsClient(c) }
	files := func(c *grpc.ClientConn) pb.FilesClient { return pb.NewFilesClient(c) }
	table := map[string]row{
		pb.Node_Info_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := pb.NewNodeClient(c).Info(ctx, &pb.InfoRequest{})
			return err
		}, [4]codes.Code{ok, ok, ok, ok}},
		pb.Exec_Run_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := pb.NewExecClient(c).Run(ctx, &pb.ExecRequest{Argv: []string{exe, "-test.run", "^$"}, Timeout: durationpb.New(30 * time.Second)})
			return err
		}, [4]codes.Code{ok, ok, ok, ok}},
		pb.Jobs_Submit_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			// somebody else's job id is taken
			_, err := jobs(c).Submit(ctx, &pb.SubmitRequest{Spec: &pb.JobSpec{Id: "JOBA", Argv: []string{exe},
				Env: map[string]string{"LONGARM_TEST_SLEEP": "1"}}})
			return err
		}, [4]codes.Code{ok, codes.AlreadyExists, codes.AlreadyExists, codes.AlreadyExists}},
		pb.Jobs_Get_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := jobs(c).Get(ctx, &pb.GetJobRequest{Id: "JOBA"})
			return err
		}, [4]codes.Code{ok, nf, nf, ok}},
		pb.Jobs_List_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			r, err := jobs(c).List(ctx, &pb.ListJobsRequest{})
			if err != nil {
				return err
			}
			for _, j := range r.Jobs {
				if j.Spec.Id == "JOBA" {
					return nil
				}
			}
			return status.Error(nf, "JOBA not listed")
		}, [4]codes.Code{ok, nf, nf, ok}},
		pb.Jobs_Logs_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			return recvErr(jobs(c).Logs(ctx, &pb.LogsRequest{Id: "JOBS"})) // the shared one
		}, [4]codes.Code{ok, nf, ok, ok}},
		pb.Jobs_Wait_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			wctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			st, err := jobs(c).Wait(wctx, &pb.WaitRequest{Id: "JOBS"})
			if err != nil {
				return err
			}
			_, err = st.Recv() // the first snapshot (or the refusal)
			return err
		}, [4]codes.Code{ok, nf, ok, ok}},
		pb.Jobs_Kill_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := jobs(c).Kill(ctx, &pb.KillRequest{Id: "JOBS"}) // shared: read-only for the team
			return err
		}, [4]codes.Code{ok, nf, nf, ok}},
		pb.Metrics_Snapshot_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := pb.NewMetricsClient(c).Snapshot(ctx, &pb.MetricsRequest{})
			return err
		}, [4]codes.Code{ok, ok, ok, ok}},
		pb.Metrics_Stream_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			st, err := pb.NewMetricsClient(c).Stream(ctx, &pb.MetricsStreamRequest{})
			if err != nil {
				return err
			}
			m, err := st.Recv()
			if err == nil && c != e.root && (len(m.TopCpu) > 0 || len(m.TopMem) > 0) {
				return status.Error(codes.FailedPrecondition, "process lists visible") // only admins see them
			}
			return err
		}, [4]codes.Code{ok, ok, ok, ok}},
		pb.Admin_Shutdown_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := pb.NewAdminClient(c).Shutdown(ctx, &pb.ShutdownRequest{})
			return err
		}, [4]codes.Code{pd, pd, pd, ok}},
		pb.Admin_Gc_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := pb.NewAdminClient(c).Gc(ctx, &pb.GcRequest{DryRun: true})
			return err
		}, [4]codes.Code{pd, pd, pd, ok}},
		pb.Sessions_Attach_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			return attachErr(ctx, c, &pb.AttachOpen{Name: "dev", Owner: "alice", NoCreate: true, NoReplay: true})
		}, [4]codes.Code{ok, pd, pd, ok}},
		pb.Sessions_List_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			r, err := sess(c).List(ctx, &pb.ListSessionsRequest{All: true})
			if err != nil {
				return err
			}
			for _, s := range r.Sessions {
				if s.Name == "dev" {
					return nil
				}
			}
			return status.Error(nf, "dev not listed")
		}, [4]codes.Code{ok, nf, nf, ok}},
		pb.Sessions_Kill_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := sess(c).Kill(ctx, &pb.KillSessionRequest{Name: "dev"}) // the caller's own namespace
			return err
		}, [4]codes.Code{ok, nf, nf, codes.NotFound}}, // root has no "dev" of its own (kill runs last: see below)
		pb.Sessions_Logs_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			return recvErr(sess(c).Logs(ctx, &pb.LogsRequest{Id: "dev", Owner: "alice"}))
		}, [4]codes.Code{ok, pd, pd, ok}},
		pb.Sessions_Start_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			// the same spec: alice's running session; a name of somebody else's: in use
			r, err := sess(c).Start(ctx, &pb.StartSessionRequest{Name: "dev", Argv: []string{exe}, Env: map[string]string{"LONGARM_TEST_SLEEP": "1"}})
			if err == nil && r.Created {
				return status.Error(codes.FailedPrecondition, "re-created")
			}
			return err
		}, [4]codes.Code{ok, codes.AlreadyExists, codes.AlreadyExists, codes.AlreadyExists}},
		pb.Sessions_Get_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := sess(c).Get(ctx, &pb.GetSessionRequest{Name: "dev", Owner: "alice"})
			return err
		}, [4]codes.Code{ok, pd, pd, ok}},
		pb.Sessions_Send_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := sess(c).Send(ctx, &pb.SendSessionRequest{Name: "dev", Owner: "alice"})
			return err
		}, [4]codes.Code{ok, pd, pd, ok}},
		pb.Sessions_Stop_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := sess(c).Stop(ctx, &pb.StopSessionRequest{Name: "nosuch"})
			return err
		}, [4]codes.Code{nf, nf, nf, nf}},
		pb.Files_Tail_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			return recvErr(files(c).Tail(ctx, &pb.FileTailRequest{Path: filepath.Join(e.srv.DataDir, "missing")}))
		}, [4]codes.Code{nf, nf, nf, nf}},
		pb.Forward_Open_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			st, err := pb.NewForwardClient(c).Open(ctx)
			if err != nil {
				return err
			}
			st.Send(&pb.ForwardFrame{Target: "1", Eof: true}) // nothing listens on port 1
			_, err = st.Recv()
			return err
		}, [4]codes.Code{codes.Unavailable, codes.Unavailable, codes.Unavailable, codes.Unavailable}},
		pb.Forward_Ssh_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			st, err := pb.NewForwardClient(c).Ssh(ctx)
			if err != nil {
				return err
			}
			st.Send(&pb.ForwardFrame{})
			_, err = st.Recv()
			return err
		}, [4]codes.Code{codes.Unimplemented, codes.Unimplemented, codes.Unimplemented, codes.Unimplemented}}, // no sshd here
		pb.Files_Stat_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := files(c).Stat(ctx, &pb.FileStatRequest{Path: os.TempDir()})
			return err
		}, [4]codes.Code{ok, ok, ok, ok}},
		pb.Files_List_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := files(c).List(ctx, &pb.FileListRequest{Path: filepath.Join(e.srv.DataDir, "staging-none")})
			return err
		}, [4]codes.Code{nf, nf, nf, nf}},
		pb.Files_Read_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			return recvErr(files(c).Read(ctx, &pb.FileReadRequest{Path: filepath.Join(e.srv.DataDir, "missing")}))
		}, [4]codes.Code{nf, nf, nf, nf}},
		pb.Files_Write_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			st, err := files(c).Write(ctx)
			if err != nil {
				return err
			}
			st.Send(&pb.FileWriteRequest{Msg: &pb.FileWriteRequest_Header{Header: &pb.FileWriteHeader{Path: filepath.Join(e.srv.DataDir, "w.txt"), Size: 0}}})
			_, err = st.CloseAndRecv()
			return err
		}, [4]codes.Code{ok, ok, ok, ok}},
		pb.Files_Mkdir_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := files(c).Mkdir(ctx, &pb.MkdirRequest{Path: filepath.Join(e.srv.DataDir, "d")})
			return err
		}, [4]codes.Code{ok, ok, ok, ok}},
		pb.Workspace_Refs_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := pb.NewWorkspaceClient(c).Refs(ctx, &pb.RefsRequest{Repo: "none"})
			return err
		}, [4]codes.Code{ok, ok, ok, ok}},
		pb.LinkTunnel_Dial_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			st, err := pb.NewLinkTunnelClient(c).Dial(ctx) // workers only (TestLinkTunnel)
			if err != nil {
				return err
			}
			_, err = st.Recv()
			return err
		}, [4]codes.Code{pd, pd, pd, pd}},
		pb.Handoff_Fetch_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			st, err := pb.NewHandoffClient(c).Fetch(ctx, &pb.HandoffFetchRequest{TransferId: "none-0000"}) // workers only (TestHandoffOfferScope)
			if err != nil {
				return err
			}
			_, err = st.Recv()
			return err
		}, [4]codes.Code{pd, pd, pd, pd}},
		pb.Handoff_Offer_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := pb.NewHandoffClient(c).Offer(ctx, &pb.HandoffOfferRequest{TransferId: "x"})
			return err
		}, [4]codes.Code{codes.InvalidArgument, codes.InvalidArgument, codes.InvalidArgument, codes.InvalidArgument}},
		pb.Handoff_Pull_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := pb.NewHandoffClient(c).Pull(ctx, &pb.HandoffPullRequest{})
			return err
		}, [4]codes.Code{codes.InvalidArgument, codes.InvalidArgument, codes.InvalidArgument, codes.InvalidArgument}},
		pb.Workspace_PushHandoff_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := pb.NewWorkspaceClient(c).PushHandoff(ctx, &pb.PushHandoffRequest{Branch: "main"})
			return err
		}, [4]codes.Code{codes.InvalidArgument, codes.InvalidArgument, codes.InvalidArgument, codes.InvalidArgument}},
		pb.Workspace_Fetch_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := pb.NewWorkspaceClient(c).Fetch(ctx, &pb.FetchRequest{Repo: "../x"})
			return err
		}, [4]codes.Code{codes.InvalidArgument, codes.InvalidArgument, codes.InvalidArgument, codes.InvalidArgument}},
		pb.Workspace_Sync_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			st, err := pb.NewWorkspaceClient(c).Sync(ctx)
			if err != nil {
				return err
			}
			st.Send(&pb.SyncChunk{Msg: &pb.SyncChunk_Header{Header: &pb.SyncHeader{Repo: "../x"}}})
			_, err = st.CloseAndRecv()
			return err
		}, [4]codes.Code{codes.InvalidArgument, codes.InvalidArgument, codes.InvalidArgument, codes.InvalidArgument}},
		// a fleet worker (no owner): nobody updates its machine (TestMachineUpdateOwnerOnly)
		pb.MachineUpdate_Start_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			st, err := pb.NewMachineUpdateClient(c).Start(ctx, &pb.MachineUpdateRequest{Latest: true})
			if err != nil {
				return err
			}
			_, err = st.Recv()
			return err
		}, [4]codes.Code{pd, pd, pd, pd}},
		pb.MachineUpdate_SetAuto_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := pb.NewMachineUpdateClient(c).SetAuto(ctx, &pb.MachineUpdateAutoRequest{Enabled: true})
			return err
		}, [4]codes.Code{pd, pd, pd, pd}},
		pb.MachineUpdate_Status_FullMethodName: {func(ctx context.Context, c *grpc.ClientConn) error {
			_, err := pb.NewMachineUpdateClient(c).Status(ctx, &pb.MachineUpdateStatusRequest{})
			return err
		}, [4]codes.Code{codes.Unimplemented, codes.Unimplemented, codes.Unimplemented, codes.Unimplemented}},
	}
	var missing []string
	for m := range MethodRoles {
		if _, ok := table[m]; !ok {
			missing = append(missing, m)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("authorization table misses %v", missing)
	}
	methods := make([]string, 0, len(table))
	for m := range table {
		methods = append(methods, m)
	}
	sort.Strings(methods)
	// Kills last (they end the job and the session the other rows use).
	slices.SortStableFunc(methods, func(a, b string) int {
		ka, kb := strings.HasSuffix(a, "/Kill"), strings.HasSuffix(b, "/Kill")
		switch {
		case ka == kb:
			return 0
		case ka:
			return 1
		}
		return -1
	})
	for _, m := range methods {
		r := table[m]
		for i, c := range callers {
			got := status.Code(r.call(cctx(t), c.conn))
			if got != r.want[i] {
				t.Errorf("%s as %s: %v, want %v", m, c.name, got, r.want[i])
			}
		}
	}

	// Admins act on other users' sessions explicitly.
	if _, err := sess(e.root).Kill(cctx(t), &pb.KillSessionRequest{Name: "dev", Owner: "alice"}); err != nil {
		t.Errorf("admin kills alice's session: %v", err)
	}
	// In shared isolation session names are one namespace: bob cannot take
	// alice's (ended) "dev" and wipe its transcript.
	if err := attachErr(cctx(t), e.bob, &pb.AttachOpen{Name: "dev", Argv: []string{exe}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("bob re-creates alice's session name: %v", err)
	}
	// A disabled user is refused everywhere.
	pol := e.srv.Policy.Get()
	for _, u := range pol.Users {
		u.Disabled = u.Name == "alice"
	}
	e.srv.Policy.Set(pol)
	if _, err := pb.NewNodeClient(e.alice).Info(cctx(t), &pb.InfoRequest{}); status.Code(err) != pd {
		t.Errorf("disabled alice: %v", err)
	}
}

// TestNoTenancyUnchanged: without a separating hub and without per-user
// isolation (one person's fleet) every client acts on every job, as before.
func TestNoTenancyUnchanged(t *testing.T) {
	e := newAuthzEnv(t)
	e.srv.Policy.Set(&pb.TenancyPolicy{Version: 2})
	sleepJob(t, e.alice, "JOBA")
	if _, err := pb.NewJobsClient(e.bob).Get(cctx(t), &pb.GetJobRequest{Id: "JOBA"}); err != nil {
		t.Fatalf("bob gets alice's job: %v", err)
	}
	if _, err := pb.NewJobsClient(e.bob).Kill(cctx(t), &pb.KillRequest{Id: "JOBA"}); err != nil {
		t.Fatalf("bob kills alice's job: %v", err)
	}
	m, err := pb.NewMetricsClient(e.bob).Snapshot(cctx(t), &pb.MetricsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_ = m // process lists are not stripped (nothing to check portably)
}
