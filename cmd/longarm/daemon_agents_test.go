package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/agentlog"
	"github.com/lubobill1990/longarm/internal/client"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// ---------------------------------------------------------------- fake worker

type fakeNode struct {
	pb.UnimplementedNodeServer
	dataDir string
}

func (n fakeNode) Info(context.Context, *pb.InfoRequest) (*pb.InfoResponse, error) {
	return &pb.InfoResponse{Name: "w1", Os: runtime.GOOS, DataDir: n.dataDir}, nil
}

// fakeJobs lists the fake worker's jobs.
type fakeJobs struct {
	pb.UnimplementedJobsServer
	jobs []*pb.Job
}

func (f fakeJobs) List(context.Context, *pb.ListJobsRequest) (*pb.ListJobsResponse, error) {
	return &pb.ListJobsResponse{Jobs: f.jobs}, nil
}

// fakeFiles serves this machine's files; "~" is home.
type fakeFiles struct {
	pb.UnimplementedFilesServer
	home string
}

func (f *fakeFiles) Stat(_ context.Context, req *pb.FileStatRequest) (*pb.FileInfo, error) {
	p := req.Path
	if p == "~" {
		p = f.home
	}
	fi, err := os.Stat(p)
	if err != nil {
		return &pb.FileInfo{Path: req.Path}, nil
	}
	return &pb.FileInfo{Path: req.Path, AbsPath: p, Exists: true, IsDir: fi.IsDir(), Size: fi.Size(), Mtime: timestamppb.New(fi.ModTime())}, nil
}

func (f *fakeFiles) List(_ context.Context, req *pb.FileListRequest) (*pb.FileListResponse, error) {
	if _, err := os.Stat(req.Path); err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	out := &pb.FileListResponse{AbsPath: req.Path}
	err := filepath.WalkDir(req.Path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == req.Path {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(req.Path, p)
		out.Entries = append(out.Entries, &pb.FileInfo{Path: filepath.ToSlash(rel), Exists: true, IsDir: d.IsDir(), Size: fi.Size(),
			Mtime: timestamppb.New(fi.ModTime())})
		return nil
	})
	return out, err
}

func (f *fakeFiles) Read(req *pb.FileReadRequest, st pb.Files_ReadServer) error {
	b, err := os.ReadFile(req.Path)
	if err != nil {
		return status.Error(codes.NotFound, err.Error())
	}
	for off := req.Offset; off < int64(len(b)); off += 1000 { // several chunks
		if err := st.Send(&pb.FileChunk{Offset: off, Data: b[off:min(off+1000, int64(len(b)))]}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeFiles) Tail(req *pb.FileTailRequest, st pb.Files_TailServer) error {
	off := req.Offset
	for {
		if b, err := os.ReadFile(req.Path); err == nil && int64(len(b)) > off {
			if err := st.Send(&pb.FileChunk{Offset: off, Data: b[off:]}); err != nil {
				return err
			}
			off = int64(len(b))
		}
		if !req.Follow {
			return nil
		}
		select {
		case <-st.Context().Done():
			return nil
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// fakeSessions records the calls; busy refuses sends with if_idle_ms.
type fakeSessions struct {
	pb.UnimplementedSessionsServer
	mu       sync.Mutex
	sessions []*pb.Session
	starts   []*pb.StartSessionRequest
	sends    []*pb.SendSessionRequest
	stops    []*pb.StopSessionRequest
	opens    []*pb.AttachOpen
	resizes  []*pb.TermSize
	busy     bool
	dataDir  string    // the worker's (Node.Info)
	jobs     []*pb.Job // Jobs.List
}

func (f *fakeSessions) List(context.Context, *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &pb.ListSessionsResponse{Sessions: f.sessions}, nil
}

func (f *fakeSessions) Get(_ context.Context, req *pb.GetSessionRequest) (*pb.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ss := range f.sessions {
		if ss.Name == req.Name {
			return ss, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "no session %s", req.Name)
}

func (f *fakeSessions) Start(_ context.Context, req *pb.StartSessionRequest) (*pb.AttachStarted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, proto.Clone(req).(*pb.StartSessionRequest))
	return &pb.AttachStarted{Session: &pb.Session{Name: req.Name, Argv: req.Argv, State: pb.JobState_JOB_STATE_RUNNING}, Created: true}, nil
}

func (f *fakeSessions) Send(_ context.Context, req *pb.SendSessionRequest) (*pb.SendSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy && req.IfIdleMs > 0 {
		return nil, status.Error(codes.ResourceExhausted, "input within the window")
	}
	f.sends = append(f.sends, proto.Clone(req).(*pb.SendSessionRequest))
	return &pb.SendSessionResponse{InputSeq: uint64(len(f.sends))}, nil
}

func (f *fakeSessions) Stop(_ context.Context, req *pb.StopSessionRequest) (*pb.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, proto.Clone(req).(*pb.StopSessionRequest))
	return &pb.Session{Name: req.Name, State: pb.JobState_JOB_STATE_DONE}, nil
}

// Attach: "missing" does not exist; else started, "hello", then echo;
// "exit" ends the program (code 3), a resize answers "resized", detach
// ends the stream.
func (f *fakeSessions) Attach(st pb.Sessions_AttachServer) error {
	m, err := st.Recv()
	if err != nil {
		return err
	}
	open := m.GetOpen()
	f.mu.Lock()
	f.opens = append(f.opens, proto.Clone(open).(*pb.AttachOpen))
	f.mu.Unlock()
	if open.Name == "missing" {
		return status.Error(codes.NotFound, "no session missing")
	}
	st.Send(&pb.AttachOutput{Msg: &pb.AttachOutput_Started{Started: &pb.AttachStarted{Session: &pb.Session{Name: open.Name}}}})
	st.Send(&pb.AttachOutput{Msg: &pb.AttachOutput_Data{Data: []byte("hello")}})
	for {
		in, err := st.Recv()
		if err != nil {
			return nil
		}
		switch m := in.Msg.(type) {
		case *pb.AttachInput_Data:
			if string(m.Data) == "exit" {
				return st.Send(&pb.AttachOutput{Msg: &pb.AttachOutput_ExitCode{ExitCode: 3}})
			}
			st.Send(&pb.AttachOutput{Msg: &pb.AttachOutput_Data{Data: m.Data}})
		case *pb.AttachInput_Resize:
			f.mu.Lock()
			f.resizes = append(f.resizes, m.Resize)
			f.mu.Unlock()
			st.Send(&pb.AttachOutput{Msg: &pb.AttachOutput_Data{Data: []byte("resized")}})
		case *pb.AttachInput_Detach:
			return nil
		}
	}
}

// agentTestDaemon serves the agent routes of a daemon whose worker w1 is
// an in-process fake with home as its home directory.
func agentTestDaemon(t *testing.T, home string, fk *fakeSessions) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterNodeServer(srv, fakeNode{dataDir: fk.dataDir})
	pb.RegisterJobsServer(srv, fakeJobs{jobs: fk.jobs})
	pb.RegisterFilesServer(srv, &fakeFiles{home: home})
	pb.RegisterSessionsServer(srv, fk)
	go srv.Serve(ln)
	t.Cleanup(srv.Stop)
	cc, err := grpc.NewClient("passthrough:///"+ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cc.Close() })
	agentHosts.Lock()
	agentHosts.m = map[string]*agentHost{}
	agentHosts.Unlock()
	d := &daemon{conns: map[string]*client.Conn{"w1": {ClientConn: cc, Worker: client.Worker{Name: "w1"}}}}
	mux := http.NewServeMux()
	d.agentsGateway(mux)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return hs
}

// call sends a JSON request and decodes the answer into out.
func call(t *testing.T, method, url, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, url, err, b)
		}
	}
	return resp.StatusCode
}

// ---------------------------------------------------------------- list

// Sessions are paired with transcripts by the process's own record, the id
// resumed, else the newest transcript of the agent in the session's
// directory (case-insensitive on Windows) written since it started.
func TestMatchAgentSessions(t *testing.T) {
	t0 := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	ep := &hoEndpoint{name: "w1", goos: "windows", home: `C:\Users\alice`}
	running := pb.JobState_JOB_STATE_RUNNING
	sess := func(name string, argv []string, dir string, start time.Time, pid int32) *pb.Session {
		return &pb.Session{Name: name, Argv: argv, Dir: dir, StartedAt: timestamppb.New(start), State: running, Pid: pid,
			LastActivity: timestamppb.New(start)}
	}
	sessions := []*pb.Session{
		sess("agent1", []string{`C:\tools\claude.exe`}, `C:\src\app`, t0, 100),
		sess("agent2", []string{"claude", "--resume", "id-resumed"}, `C:\src\other`, t0, 101),
		sess("shell1", []string{"pwsh"}, "", t0, 102),
		sess("fresh", []string{"copilot.cmd"}, `C:\src\app`, t0.Add(time.Hour), 103),
		sess("bypid", []string{"claude"}, `C:\src\app`, t0, 200),
		{Name: "old", Argv: []string{"claude"}, Dir: `C:\src\gone`, State: pb.JobState_JOB_STATE_DONE, CreatedAt: timestamppb.New(t0)},
	}
	tr := func(agent, id, dir string, mod time.Time) *agentSession {
		return &agentSession{Agent: agent, ID: id, ProjectDir: dir, Modified: mod, Title: "t " + id, transcript: `C:\t\` + id}
	}
	handed := tr("copilot", "id-handed", `C:\src\app`, t0.Add(-time.Hour))
	handed.HandedOff = &handoffMark{To: "w2"}
	transcripts := []*agentSession{
		tr("claude", "id-app-new", `c:\SRC\App\`, t0.Add(30*time.Minute)),
		tr("claude", "id-app-old", `C:\src\app`, t0.Add(-time.Hour)),
		tr("claude", "id-resumed", `C:\src\other`, t0.Add(-2*time.Hour)),
		tr("claude", "id-pid", `C:\elsewhere`, t0.Add(10*time.Minute)),
		tr("claude", "id-desktop", `C:\x`, t0),
		handed,
	}
	live := map[string]int{"claude/id-pid": 200, "claude/id-desktop": 300}
	got := map[string]*agentItem{}
	for _, it := range matchAgentSessions(ep, sessions, transcripts, live, false, nil, nil) {
		if got[it.Key] != nil {
			t.Fatalf("key %s twice", it.Key)
		}
		got[it.Key] = it
	}
	want := map[string]struct{ where, session, state string }{
		"w1/claude/id-app-new": {whereLongarm, "agent1", ""},
		"w1/claude/id-resumed": {whereLongarm, "agent2", ""},
		"w1/session/shell1":    {whereLongarm, "shell1", stateRunning},
		"w1/session/fresh":     {whereLongarm, "fresh", ""},
		"w1/claude/id-pid":     {whereLongarm, "bypid", ""},
		"w1/session/old":       {whereLongarm, "old", "ended"},
		"w1/claude/id-app-old": {whereNone, "", "ended"},
		"w1/claude/id-desktop": {whereDesktop, "", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d items, want %d: %v", len(got), len(want), got)
	}
	for k, w := range want {
		it := got[k]
		if it == nil {
			t.Fatalf("no item %s", k)
		}
		if it.Where != w.where || it.Session != w.session || it.State != w.state {
			t.Errorf("%s: where %q session %q state %q, want %+v", k, it.Where, it.Session, it.State, w)
		}
	}
	if it := got["w1/session/fresh"]; it.Agent != "copilot" || it.ID != "" || it.ProjectDir != `C:\src\app` {
		t.Errorf("fresh copilot: %+v", it)
	}
	if it := got["w1/claude/id-desktop"]; it.PID != 300 || !it.live {
		t.Errorf("desktop: %+v", it)
	}
	if it := got["w1/claude/id-app-new"]; it.PID != 100 || it.Title != "t id-app-new" || it.transcript != `C:\t\id-app-new` {
		t.Errorf("agent1: %+v", it)
	}
	if it := got["w1/session/shell1"]; it.Title != "pwsh" || it.Agent != "" {
		t.Errorf("shell: %+v", it)
	}
	all := matchAgentSessions(ep, sessions, transcripts, live, true, nil, nil)
	if len(all) != len(want)+1 {
		t.Errorf("all: %d items, want the handed-off one too", len(all))
	}
}

func TestAgentOfAndResumedID(t *testing.T) {
	for argv, want := range map[string]string{"claude": "claude", `C:\x\Claude.EXE`: "claude", "/usr/bin/copilot": "copilot",
		"copilot.cmd --resume=x": "copilot", "pwsh": "", "claude-code": "", "": ""} {
		if got := agentOf(strings.Fields(argv)); got != want {
			t.Errorf("agentOf(%q) = %q, want %q", argv, got, want)
		}
	}
	for argv, want := range map[string]string{"claude --resume abc": "abc", "claude -r abc": "abc", "copilot --resume=abc": "abc",
		"claude --resume abc --fork-session": "", "claude": "", "claude --resume": ""} {
		if got := resumedID(strings.Fields(argv)); got != want {
			t.Errorf("resumedID(%q) = %q, want %q", argv, got, want)
		}
	}
}

// The list over HTTP: a claude in a Longarm session, paired with its
// transcript on the worker; filtered by agent.
func TestAgentListHTTP(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, "src", "app")
	id := "9f1c2d3e-0000-4000-8000-000000000001"
	writeClaudeTranscript(t, home, id, proj, 3)
	fk := &fakeSessions{sessions: []*pb.Session{{Name: "agent1", Argv: []string{"claude"}, Dir: proj, State: pb.JobState_JOB_STATE_RUNNING,
		StartedAt: timestamppb.New(time.Now().Add(-time.Hour)), Pid: 4242}}}
	hs := agentTestDaemon(t, home, fk)
	var got agentListJSON
	if c := call(t, "GET", hs.URL+"/v1/agent-sessions?worker=w1", "", &got); c != 200 {
		t.Fatalf("status %d", c)
	}
	if len(got.Items) != 1 || got.Unreachable == nil {
		t.Fatalf("got %+v", got)
	}
	it := got.Items[0]
	if it.Key != "w1/claude/"+id || it.Where != whereLongarm || it.Session != "agent1" || it.ProjectDir != proj || it.Title != "prompt 0" ||
		it.State == "" || it.PID != 4242 {
		t.Fatalf("item %+v", it)
	}
	if call(t, "GET", hs.URL+"/v1/agent-sessions?worker=w1&agent=copilot", "", &got); len(got.Items) != 0 {
		t.Fatalf("agent=copilot: %+v", got.Items)
	}
	if c := call(t, "GET", hs.URL+"/v1/agent-sessions?worker=w1&agent=bash", "", nil); c != 400 {
		t.Fatalf("agent=bash: %d", c)
	}
}

// writeClaudeTranscript writes n user lines (synthetic) and returns the path.
func writeClaudeTranscript(t *testing.T, home, id, cwd string, n int) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "C--src-app")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	for i := range n {
		l, _ := json.Marshal(map[string]any{"type": "user", "cwd": cwd, "sessionId": id,
			"message": map[string]any{"role": "user", "content": "prompt " + string(rune('0'+i))}})
		b.Write(l)
		b.WriteByte('\n')
	}
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---------------------------------------------------------------- messages

func TestTranscriptLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.jsonl")
	data := "{\"a\":1}\n{\"b\":2}\r\n\n{\"c\":3}\n{\"partial"
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	ep := &hoEndpoint{name: localMachine, goos: runtime.GOOS}
	ctx := context.Background()
	offsets := func(off, limit int64, align bool) ([]int64, int64) {
		lines, next, err := transcriptLines(ctx, ep, p, off, limit, align)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, l := range lines {
			out = append(out, l.Offset)
			if bytes.ContainsAny(l.Data, "\r\n") {
				t.Errorf("line at %d keeps a line end: %q", l.Offset, l.Data)
			}
		}
		return out, next
	}
	for _, c := range []struct {
		off, limit int64
		align      bool
		want       []int64
		next       int64
	}{
		{0, 100, false, []int64{0, 8, 18}, 26},
		{8, 100, true, []int64{8, 18}, 26},    // a line starts at off
		{10, 100, true, []int64{18}, 26},      // inside a line: skipped
		{26, 100, true, nil, 26},              // the partial last line is not returned
		{0, 12, false, []int64{0}, 8},         // limit
		{20, 100, true, nil, 26},              // inside the last complete line
		{8, 9, false, []int64{8}, 17},         // exactly one line
		{1, 100, true, []int64{8, 18}, 26},    // inside the first line
		{17, 100, true, []int64{18}, 26},      // a blank line
		{18, 100, false, []int64{18}, 26},     // given offset: trusted
		{35, 100, false, nil, 35},             // the end
		{0, 0, false, nil, 0},                 // nothing read
		{0, 100, true, []int64{0, 8, 18}, 26}, // align at 0: nothing to skip
	} {
		got, next := offsets(c.off, c.limit, c.align)
		if !equalInt64s(got, c.want) || next != c.next {
			t.Errorf("off %d limit %d align %v: lines %v next %d, want %v %d", c.off, c.limit, c.align, got, next, c.want, c.next)
		}
	}
	lines, start, size, err := tailLines(ctx, ep, p, 20)
	if err != nil || len(lines) != 1 || start != 18 || size != int64(len(data)) {
		t.Fatalf("tailLines: %v start %d size %d %v", lines, start, size, err)
	}
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// GET messages: the tail aligned to a line start (truncated), or from an
// offset; next_offset is after the last complete line.
func TestAgentMessagesHTTP(t *testing.T) {
	home := t.TempDir()
	id := "9f1c2d3e-0000-4000-8000-000000000002"
	p := writeClaudeTranscript(t, home, id, filepath.Join(home, "src", "app"), 4)
	full, _ := os.ReadFile(p)
	hs := agentTestDaemon(t, home, &fakeSessions{})
	base := hs.URL + "/v1/agent-sessions/w1/claude/" + id + "/messages"
	var m messagesJSON
	if c := call(t, "GET", base+"?tail_bytes=50", "", &m); c != 200 {
		t.Fatalf("status %d", c)
	}
	if !m.Truncated || m.NextOffset != int64(len(full)) || m.Size != int64(len(full)) || m.Messages == nil || m.State == "" {
		t.Fatalf("tail: %+v", m)
	}
	m = messagesJSON{}
	call(t, "GET", base+"?offset=0", "", &m)
	if m.Truncated || m.NextOffset != int64(len(full)) {
		t.Fatalf("offset 0: %+v", m)
	}
	// a partial line at the end is left for later
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"user"`)
	f.Close()
	m = messagesJSON{}
	call(t, "GET", base, "", &m)
	if m.Truncated || m.NextOffset != int64(len(full)) || m.Size != int64(len(full))+14 {
		t.Fatalf("partial line: %+v", m)
	}
	if c := call(t, "GET", hs.URL+"/v1/agent-sessions/w1/claude/9f1c2d3e-0000-4000-8000-00000000dead/messages", "", nil); c != 404 {
		t.Fatalf("unknown id: %d", c)
	}
	if c := call(t, "GET", hs.URL+"/v1/agent-sessions/w1/bash/"+id+"/messages", "", nil); c != 400 {
		t.Fatalf("unknown agent: %d", c)
	}
}

// The watch sends "messages" with the new next_offset once complete lines
// are appended.
func TestAgentWatchHTTP(t *testing.T) {
	home := t.TempDir()
	id := "9f1c2d3e-0000-4000-8000-000000000003"
	p := writeClaudeTranscript(t, home, id, filepath.Join(home, "src", "app"), 1)
	size0, _ := os.Stat(p)
	hs := agentTestDaemon(t, home, &fakeSessions{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/v1/agent-sessions/w1/claude/%s/messages/watch?offset=%d", hs.URL, id, size0.Size()), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() || sc.Text() != ": ok" {
		t.Fatalf("first line %q", sc.Text())
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("{\"type\":\"assistant\"}\n{\"type\":")
	f.Close()
	want := size0.Size() + int64(len("{\"type\":\"assistant\"}\n"))
	event := ""
	for sc.Scan() {
		l := sc.Text()
		if e, ok := strings.CutPrefix(l, "event: "); ok {
			event = e
			continue
		}
		if d, ok := strings.CutPrefix(l, "data: "); ok && event == "messages" {
			var m messagesJSON
			if err := json.Unmarshal([]byte(d), &m); err != nil {
				t.Fatal(err)
			}
			if m.NextOffset != want || m.Messages == nil {
				t.Fatalf("messages event: %s", d)
			}
			return
		}
	}
	t.Fatalf("no messages event: %v", sc.Err())
}

// ---------------------------------------------------------------- control

func TestStartCommand(t *testing.T) {
	id := "9f1c2d3e-0000-4000-8000-000000000004"
	for _, c := range []struct {
		req  startAgentReq
		want string // argv joined; "!" = an error
	}{
		{startAgentReq{Agent: "claude"}, "claude"},
		{startAgentReq{Agent: "claude", Resume: id}, "claude --resume " + id},
		{startAgentReq{Agent: "claude", Resume: id, Fork: true}, "claude --resume " + id + " --fork-session"},
		{startAgentReq{Agent: "copilot"}, "copilot"},
		{startAgentReq{Agent: "copilot", Resume: id}, "copilot --resume=" + id},
		{startAgentReq{Agent: "copilot", Resume: id, Fork: true}, "!"},
		{startAgentReq{Argv: []string{"pwsh", "-NoLogo"}}, "pwsh -NoLogo"},
		{startAgentReq{}, ""},
		{startAgentReq{Agent: "claude", Argv: []string{"x"}}, "!"},
		{startAgentReq{Resume: id}, "!"},
		{startAgentReq{Agent: "claude", Fork: true}, "!"},
		{startAgentReq{Agent: "claude", Resume: "x; rm -rf /"}, "!"},
		{startAgentReq{Agent: "bash"}, "!"},
	} {
		argv, err := c.req.command()
		got := strings.Join(argv, " ")
		if err != nil {
			got = "!"
		}
		if got != c.want {
			t.Errorf("%+v: %q (%v), want %q", c.req, got, err, c.want)
		}
	}
	for _, c := range []struct {
		req    startAgentReq
		argv   []string
		prefix string
	}{
		{startAgentReq{Name: "mine"}, nil, "mine"},
		{startAgentReq{Agent: "claude"}, []string{"claude"}, "claude-"},
		{startAgentReq{}, []string{`C:\bin\pwsh.exe`}, "pwsh-"},
		{startAgentReq{}, nil, "shell-"},
		{startAgentReq{}, []string{"./a b!"}, "ab-"},
	} {
		n := c.req.sessionName(c.argv)
		if !strings.HasPrefix(n, c.prefix) || (c.req.Name == "" && len(n) != len(c.prefix)+4) {
			t.Errorf("sessionName(%+v, %v) = %q, want %s<4 hex>", c.req, c.argv, n, c.prefix)
		}
	}
}

func TestSendWrites(t *testing.T) {
	got, err := sendReq{Text: "fix it\nnow\r\n", Keys: []string{"Enter", "ctrl-c", "esc"}}.writes()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"fix it\rnow\r", "\r", "\x03", "\x1b"}
	if len(got) != len(want) {
		t.Fatalf("writes %q", got)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("write %d: %q, want %q", i, got[i], want[i])
		}
	}
	if got, _ := (sendReq{Text: "a\nb", Raw: true}).writes(); len(got) != 1 || string(got[0]) != "a\nb" {
		t.Errorf("raw: %q", got)
	}
	for _, r := range []sendReq{{}, {Keys: []string{"hyper"}}, {Text: strings.Repeat("x", 256<<10+1)}} {
		if _, err := r.writes(); err == nil {
			t.Errorf("%.40v: no error", r)
		}
	}
}

// Start, send (Enter as its own write; if_idle_ms on the first; 409 when
// not idle), stop and get over HTTP.
func TestSessionControlHTTP(t *testing.T) {
	fk := &fakeSessions{sessions: []*pb.Session{{Name: "s1", State: pb.JobState_JOB_STATE_RUNNING}}}
	hs := agentTestDaemon(t, t.TempDir(), fk)
	var ss map[string]any
	if c := call(t, "POST", hs.URL+"/v1/workers/w1/sessions", `{"argv":["pwsh"],"cwd":"src","cols":120,"rows":36}`, &ss); c != 201 {
		t.Fatalf("start: %d %v", c, ss)
	}
	st := fk.starts[0]
	if !strings.HasPrefix(st.Name, "pwsh-") || st.Cwd != "src" || st.Size.GetCols() != 120 || st.Size.GetRows() != 36 || st.AgentAuth != nil ||
		ss["name"] != st.Name || ss["state"] != "JOB_STATE_RUNNING" {
		t.Fatalf("start request %v, answer %v", st, ss)
	}
	var e map[string]string
	if c := call(t, "POST", hs.URL+"/v1/workers/w1/sessions", `{"agent":"claude","argv":["x"]}`, &e); c != 400 || e["error"] == "" {
		t.Fatalf("agent and argv: %d %v", c, e)
	}
	var sent map[string]uint64
	if c := call(t, "POST", hs.URL+"/v1/workers/w1/sessions/s1/send", `{"text":"fix it","keys":["enter"],"if_idle_ms":1500}`, &sent); c != 200 {
		t.Fatalf("send: %d", c)
	}
	if len(fk.sends) != 2 || string(fk.sends[0].Data) != "fix it" || fk.sends[0].IfIdleMs != 1500 ||
		string(fk.sends[1].Data) != "\r" || fk.sends[1].IfIdleMs != 0 || sent["input_seq"] != 2 {
		t.Fatalf("sends %v, answer %v", fk.sends, sent)
	}
	fk.busy = true
	if c := call(t, "POST", hs.URL+"/v1/workers/w1/sessions/s1/send", `{"text":"y","if_idle_ms":1500}`, &e); c != 409 || e["error"] != "not idle" {
		t.Fatalf("busy: %d %v", c, e)
	}
	if c := call(t, "POST", hs.URL+"/v1/workers/w1/sessions/s1/send", `{"keys":["nope"]}`, nil); c != 400 {
		t.Fatalf("bad key: %d", c)
	}
	call(t, "POST", hs.URL+"/v1/workers/w1/sessions/s1/stop", "", &ss)
	call(t, "POST", hs.URL+"/v1/workers/w1/sessions/s1/stop", `{"grace_ms":0}`, &ss)
	if len(fk.stops) != 2 || fk.stops[0].Grace.AsDuration() != 10*time.Second || fk.stops[1].Grace.AsDuration() != 0 || ss["state"] != "JOB_STATE_DONE" {
		t.Fatalf("stops %v, answer %v", fk.stops, ss)
	}
	if c := call(t, "GET", hs.URL+"/v1/workers/w1/sessions/s1", "", &ss); c != 200 || ss["name"] != "s1" {
		t.Fatalf("get: %d %v", c, ss)
	}
	if c := call(t, "GET", hs.URL+"/v1/workers/w1/sessions/nope", "", &e); c != 404 || e["error"] != "no session nope" {
		t.Fatalf("get missing: %d %v", c, e)
	}
}

// ---------------------------------------------------------------- terminal

func TestAttachWebSocket(t *testing.T) {
	fk := &fakeSessions{}
	hs := agentTestDaemon(t, t.TempDir(), fk)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dial := func(name string) *websocket.Conn {
		ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hs.URL, "http")+"/v1/workers/w1/sessions/"+name+"/attach?cols=80&rows=24", nil)
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}
	expect := func(ws *websocket.Conn, typ websocket.MessageType, want string) {
		t.Helper()
		got, b, err := ws.Read(ctx)
		if err != nil || got != typ || !strings.Contains(string(b), want) {
			t.Fatalf("read %v %q %v, want %v %q", got, b, err, typ, want)
		}
	}
	closed := func(ws *websocket.Conn, code websocket.StatusCode) {
		t.Helper()
		_, b, err := ws.Read(ctx)
		if websocket.CloseStatus(err) != code {
			t.Fatalf("read %q %v, want close %d", b, err, code)
		}
	}

	ws := dial("s1")
	expect(ws, websocket.MessageText, `{"session":{"name":"s1"},"type":"started"}`)
	expect(ws, websocket.MessageBinary, "hello")
	ws.Write(ctx, websocket.MessageBinary, []byte("ls\r"))
	expect(ws, websocket.MessageBinary, "ls\r")
	ws.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":100,"rows":30}`))
	expect(ws, websocket.MessageBinary, "resized")
	ws.Write(ctx, websocket.MessageBinary, []byte("exit"))
	expect(ws, websocket.MessageText, `{"code":3,"type":"exit"}`)
	closed(ws, websocket.StatusNormalClosure)
	fk.mu.Lock()
	if o := fk.opens[0]; o.Name != "s1" || !o.NoCreate || o.NoReplay || o.Size.GetCols() != 80 || o.Size.GetRows() != 24 {
		t.Errorf("open %v", o)
	}
	if len(fk.resizes) != 1 || fk.resizes[0].Cols != 100 || fk.resizes[0].Rows != 30 {
		t.Errorf("resizes %v", fk.resizes)
	}
	fk.mu.Unlock()

	ws = dial("s1") // detach: the worker ends the stream, the bridge closes normally
	expect(ws, websocket.MessageText, "started")
	expect(ws, websocket.MessageBinary, "hello")
	ws.Write(ctx, websocket.MessageText, []byte(`{"type":"detach"}`))
	closed(ws, websocket.StatusNormalClosure)

	ws = dial("missing")
	expect(ws, websocket.MessageText, `{"message":"no session missing","type":"error"}`)
	closed(ws, websocket.StatusInternalError)
}

// The list's first page holds the last agentRecent and the running
// sessions, whatever their age; "older" leads to the next page.
func TestAgentListWindow(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, "src", "app")
	recent, old, pinned := "9f1c2d3e-0000-4000-8000-0000000000a1", "9f1c2d3e-0000-4000-8000-0000000000a2", "9f1c2d3e-0000-4000-8000-0000000000a3"
	writeClaudeTranscript(t, home, recent, proj, 1)
	oldTime := time.Now().Add(-10 * 24 * time.Hour).Truncate(time.Second)
	for _, id := range []string{old, pinned} {
		p := writeClaudeTranscript(t, home, id, proj, 1)
		mt := oldTime
		if id == pinned {
			mt = oldTime.Add(-24 * time.Hour)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	fk := &fakeSessions{sessions: []*pb.Session{{Name: "resumed", Argv: []string{"claude", "--resume", pinned}, Dir: proj,
		State: pb.JobState_JOB_STATE_RUNNING, StartedAt: timestamppb.New(time.Now().Add(-time.Minute)), Pid: 7}}}
	hs := agentTestDaemon(t, home, fk)
	ids := func(l agentListJSON) []string {
		var out []string
		for _, it := range l.Items {
			out = append(out, it.ID)
		}
		slices.Sort(out)
		return out
	}
	var first agentListJSON
	if c := call(t, "GET", hs.URL+"/v1/agent-sessions?worker=w1", "", &first); c != 200 {
		t.Fatalf("status %d", c)
	}
	if got := ids(first); !slices.Equal(got, []string{recent, pinned}) || !first.Older.Equal(oldTime) {
		t.Fatalf("first page: %v older %v", got, first.Older)
	}
	var page agentListJSON
	q := url.Values{"worker": {"w1"}, "until": {first.Older.Format(time.RFC3339Nano)}}
	call(t, "GET", hs.URL+"/v1/agent-sessions?"+q.Encode(), "", &page)
	if got := ids(page); !slices.Equal(got, []string{old}) || !page.Older.IsZero() {
		t.Fatalf("older page: %v older %v", got, page.Older) // the pinned one is on the first page only
	}
	if c := call(t, "GET", hs.URL+"/v1/agent-sessions?worker=w1&since=yesterday", "", nil); c != 400 {
		t.Fatalf("bad since: %d", c)
	}
}

// "Load earlier": before=<start> returns the part before what is shown.
func TestAgentMessagesBefore(t *testing.T) {
	home := t.TempDir()
	id := "9f1c2d3e-0000-4000-8000-0000000000b1"
	p := writeClaudeTranscript(t, home, id, filepath.Join(home, "src", "app"), 6)
	full, _ := os.ReadFile(p)
	hs := agentTestDaemon(t, home, &fakeSessions{})
	base := hs.URL + "/v1/agent-sessions/w1/claude/" + id + "/messages"
	var tail messagesJSON
	call(t, "GET", base+"?tail_bytes=200", "", &tail)
	if !tail.Truncated || tail.Start <= 0 || tail.NextOffset != int64(len(full)) {
		t.Fatalf("tail: %+v", tail)
	}
	var earlier messagesJSON
	call(t, "GET", fmt.Sprintf("%s?before=%d&tail_bytes=1000000", base, tail.Start), "", &earlier)
	if earlier.Truncated || earlier.Start != 0 || earlier.NextOffset != tail.Start || len(earlier.Messages) == 0 {
		t.Fatalf("earlier: %+v", earlier)
	}
	if got := len(earlier.Messages) + len(tail.Messages); got != 6 {
		t.Fatalf("%d messages in both parts, want 6", got)
	}
	var step messagesJSON // a small window: still whole lines, ending at before
	call(t, "GET", fmt.Sprintf("%s?before=%d&tail_bytes=150", base, tail.Start), "", &step)
	if step.NextOffset != tail.Start || !step.Truncated || len(step.Messages) == 0 {
		t.Fatalf("step: %+v", step)
	}
}

// describe runs once per transcript as it is (the Sessions window lists
// every few seconds); a changed file is read again.
func TestDescribeCached(t *testing.T) {
	ep := &hoEndpoint{name: "w-cache"}
	s := &agentSession{transcript: "/x/a.jsonl", Size: 10, Modified: time.Unix(100, 0)}
	n := 0
	describe := func(_ context.Context, _ *hoEndpoint, s *agentSession) { n++; s.Title = fmt.Sprint("title ", n) }
	for range 3 {
		c := *s
		describeCached(context.Background(), ep, &c, describe)
		if c.Title != "title 1" {
			t.Fatalf("cached title %q", c.Title)
		}
	}
	if n != 1 {
		t.Fatalf("described %d times", n)
	}
	changed := *s
	changed.Size = 11
	describeCached(context.Background(), ep, &changed, describe)
	other := *s
	describeCached(context.Background(), &hoEndpoint{name: "w-other"}, &other, describe)
	if n != 3 || changed.Title != "title 2" || other.Title != "title 3" {
		t.Fatalf("n %d: %q %q", n, changed.Title, other.Title)
	}
}

// Agents in jobs: a Copilot job's conversation in its private
// COPILOT_HOME, a Claude job's transcript in its worktree - listed as the
// job's; a Longarm session's own Copilot home pairs with it.
func TestAgentListJobs(t *testing.T) {
	home := t.TempDir()
	data := filepath.Join(home, ".longarm")
	running, done := "01M66YJJ2AHVSSW2DBRQNSDWSF", "01M66Y2FGEAF79SP1EFNJT0WG8"
	jobConv, sessConv, claudeJob := "11111111-0000-4000-8000-000000000001", "11111111-0000-4000-8000-000000000002", "9f1c2d3e-0000-4000-8000-0000000000c1"
	copilotConv := func(homeDir, id, prompt string) {
		dir := filepath.Join(data, "agent-home", homeDir, "session-state", id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		ev := `{"type":"session.start","data":{"sessionId":"` + id + `","context":{"cwd":"C:\\src"}},"id":"e1","timestamp":"` + now + `"}` + "\n" +
			`{"type":"user.message","data":{"content":"` + prompt + `"},"id":"e2","timestamp":"` + now + `"}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(ev), 0o600); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "workspace.yaml"), []byte("id: "+id+"\ncwd: C:\\src\n"), 0o600)
	}
	copilotConv("j-"+running+"-0123456789ab", jobConv, "review W29")
	copilotConv("s-cop1-abcdef012345", sessConv, "in my session")
	writeClaudeTranscript(t, home, claudeJob, filepath.Join(data, "worktrees", done, "app"), 1)
	fk := &fakeSessions{dataDir: data,
		sessions: []*pb.Session{{Name: "cop1", Argv: []string{"copilot"}, Dir: filepath.Join(home, "elsewhere"), State: pb.JobState_JOB_STATE_RUNNING,
			StartedAt: timestamppb.New(time.Now().Add(-time.Hour)), Pid: 99}},
		jobs: []*pb.Job{
			{Spec: &pb.JobSpec{Id: running, Name: "review-W29"}, State: pb.JobState_JOB_STATE_RUNNING},
			{Spec: &pb.JobSpec{Id: done, Name: "fix-W28"}, State: pb.JobState_JOB_STATE_DONE},
		}}
	hs := agentTestDaemon(t, home, fk)
	var got agentListJSON
	if c := call(t, "GET", hs.URL+"/v1/agent-sessions?worker=w1", "", &got); c != 200 {
		t.Fatalf("status %d", c)
	}
	byKey := map[string]*agentItem{}
	for _, it := range got.Items {
		byKey[it.Key] = it
	}
	if it := byKey["w1/copilot/"+jobConv]; it == nil || it.Where != whereJob || it.Job != running || it.JobName != "review-W29" ||
		it.State == agentlog.StateEnded || it.Title != "review W29" {
		t.Fatalf("copilot job: %+v (items %+v)", it, got.Items)
	}
	if it := byKey["w1/claude/"+claudeJob]; it == nil || it.Where != whereJob || it.Job != done || it.JobName != "fix-W28" || it.State != agentlog.StateEnded {
		t.Fatalf("claude job: %+v", it)
	}
	if it := byKey["w1/copilot/"+sessConv]; it == nil || it.Where != whereLongarm || it.Session != "cop1" {
		t.Fatalf("session's own copilot home: %+v", it)
	}
	// its conversation: found from the list, and again by a fresh daemon (searching the homes)
	base := "/v1/agent-sessions/w1/copilot/" + jobConv + "/messages"
	var m messagesJSON
	if c := call(t, "GET", hs.URL+base, "", &m); c != 200 || len(m.Messages) == 0 {
		t.Fatalf("messages: %d %+v", c, m)
	}
	hs2 := agentTestDaemon(t, home, fk)
	if c := call(t, "GET", hs2.URL+base, "", &m); c != 200 {
		t.Fatalf("messages from a fresh daemon: %d", c)
	}
}

func TestJobOfDir(t *testing.T) {
	ep := &hoEndpoint{name: "w", goos: "windows"}
	data := `C:\Users\u\.longarm`
	for dir, want := range map[string]string{
		`C:\Users\u\.longarm\worktrees\01M66YJJ2AHVSSW2DBRQNSDWSF`:       "01M66YJJ2AHVSSW2DBRQNSDWSF",
		`c:\users\u\.longarm\worktrees\01M66YJJ2AHVSSW2DBRQNSDWSF\sub\x`: "01M66YJJ2AHVSSW2DBRQNSDWSF",
		`C:\Users\u\.longarm\worktrees\s.agent1.3`:                       "", // a session's worktree
		`C:\Users\u\.longarm\worktrees`:                                  "",
		`C:\Users\u\src\app`:                                             "",
		``:                                                               "",
	} {
		if got := jobOfDir(ep, data, dir); got != want {
			t.Errorf("jobOfDir(%q) = %q, want %q", dir, got, want)
		}
	}
	if got := jobOfDir(ep, "", `C:\x\worktrees\01M66YJJ2AHVSSW2DBRQNSDWSF`); got != "" {
		t.Errorf("no data dir: %q", got)
	}
}

// A listing already made answers for its directory and subdirectories.
func TestEndpointListed(t *testing.T) {
	ep := &hoEndpoint{name: "w", goos: "windows", conn: &client.Conn{}, listed: map[string][]hoFile{
		`C:\data\agent-home`: {
			{rel: "j-1", isDir: true},
			{rel: "j-1/session-state", isDir: true},
			{rel: "j-1/session-state/a/events.jsonl", size: 3},
			{rel: "s-x/session-state/b/events.jsonl", size: 4},
		},
	}}
	rels := func(dir string) []string {
		got, err := ep.list(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range got {
			out = append(out, f.rel)
		}
		return out
	}
	if got := rels(`C:\data\agent-home\j-1\session-state`); !slices.Equal(got, []string{"a/events.jsonl"}) {
		t.Fatalf("subdirectory: %v", got)
	}
	if got := rels(`c:\DATA\agent-home\`); len(got) != 4 {
		t.Fatalf("the root (case, trailing separator): %v", got)
	}
	if got := rels(`C:\data\agent-home\s-x`); !slices.Equal(got, []string{"session-state/b/events.jsonl"}) {
		t.Fatalf("s-x: %v", got)
	}
	if sub, ok := ep.subRel(`C:\data\agent-home`, `C:\data\other`); ok {
		t.Fatalf("outside the listing: %q", sub)
	}
}

// describe reads firstHeadRead, and more when the first prompt lies beyond.
func TestDescribeReadsMoreWhenNeeded(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	meta, _ := json.Marshal(map[string]any{"type": "user", "isMeta": true, "cwd": `C:\src\app`,
		"message": map[string]any{"role": "user", "content": strings.Repeat("x", 40<<10)}})
	prompt, _ := json.Marshal(map[string]any{"type": "user", "cwd": `C:\src\app`, "message": map[string]any{"role": "user", "content": "the real prompt"}})
	data := append(append(meta, '\n'), append(prompt, '\n')...)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &agentSession{transcript: p, Size: int64(len(data))}
	claudeAdapter{}.describe(context.Background(), &hoEndpoint{name: localMachine, goos: runtime.GOOS}, s)
	if s.Title != "the real prompt" || s.ProjectDir != `C:\src\app` {
		t.Fatalf("got title %q dir %q", s.Title, s.ProjectDir)
	}
}

// A steerable job: its conversation by the agent's session id, its state;
// one still at a start-up screen (no transcript) listed all the same; send
// and attach reach the job.
func TestAgentListSteerable(t *testing.T) {
	home := t.TempDir()
	conv := "9f1c2d3e-0000-4000-8000-0000000000d1"
	writeClaudeTranscript(t, home, conv, `C:\src\app`, 1)
	withConv, starting := "01M66YJJ2AHVSSW2DBRQNSDWSA", "01M66YJJ2AHVSSW2DBRQNSDWSB"
	fk := &fakeSessions{dataDir: filepath.Join(home, ".longarm"), jobs: []*pb.Job{
		{Spec: &pb.JobSpec{Id: withConv, Name: "fix", Argv: []string{"claude", "fix it"}, Steer: &pb.SteerSpec{Agent: "claude"}},
			State: pb.JobState_JOB_STATE_RUNNING, Steer: &pb.SteerState{State: "needs_you", SessionId: conv}},
		{Spec: &pb.JobSpec{Id: starting, Argv: []string{"copilot", "-i", "go"}, Steer: &pb.SteerSpec{Agent: "copilot"}},
			State: pb.JobState_JOB_STATE_RUNNING, StartedAt: timestamppb.Now()},
	}}
	hs := agentTestDaemon(t, home, fk)
	var got agentListJSON
	if c := call(t, "GET", hs.URL+"/v1/agent-sessions?worker=w1", "", &got); c != 200 {
		t.Fatalf("status %d", c)
	}
	byKey := map[string]*agentItem{}
	for _, it := range got.Items {
		byKey[it.Key] = it
	}
	if it := byKey["w1/claude/"+conv]; it == nil || it.Where != whereJob || it.Job != withConv || it.Steer != "needs_you" || it.JobName != "fix" {
		t.Fatalf("steerable claude job: %+v (items %+v)", it, got.Items)
	}
	if it := byKey["w1/job/"+starting]; it == nil || it.Agent != "copilot" || it.Job != starting || it.Steer != "starting" ||
		it.State != agentlog.StateWorking || it.Title == "" {
		t.Fatalf("starting job: %+v", it)
	}
	if c := call(t, "POST", hs.URL+"/v1/workers/w1/jobs/"+starting+"/send", `{"text":"y","keys":["enter"]}`, nil); c != 200 {
		t.Fatalf("send: %d", c)
	}
	if len(fk.sends) != 2 || !fk.sends[0].Job || fk.sends[0].Name != starting {
		t.Fatalf("sends %v", fk.sends)
	}
}
