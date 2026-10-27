package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type startOut struct {
	Worker  string `json:"worker"`
	Session string `json:"session"`
	Cwd     string `json:"cwd"`
	Created bool   `json:"created"`
	State   string `json:"state"`
}

func (e *env) sessionStart(t *testing.T, args ...string) (startOut, result) {
	t.Helper()
	r := e.longarm(e.client, append([]string{"session", "start", "--json"}, args...)...)
	var out startOut
	if r.code == 0 {
		if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
			t.Fatalf("start --json: %v\n%s", err, r.stdout)
		}
	}
	return out, r
}

// sessionJSON returns sessions --json [args] decoded.
func (e *env) sessionJSON(t *testing.T, args ...string) (rows []map[string]any, source string) {
	t.Helper()
	var v struct {
		Sessions []map[string]any `json:"sessions"`
		Source   string           `json:"source"`
	}
	if err := json.Unmarshal([]byte(e.mustLongarm(e.client, append([]string{"sessions", "--json"}, args...)...)), &v); err != nil {
		t.Fatal(err)
	}
	return v.Sessions, v.Source
}

// TestSessionsForPrograms covers the programmable session contract
// (docs/usage.md#sessions-for-programs): session start / send / stop,
// sessions --json v2, run / exec --session, tail, and longarm attach inside
// a pseudo terminal (ConPTY on Windows) alongside session send.
func TestSessionsForPrograms(t *testing.T) {
	e := newEnv(t)
	e.startLongarmd()
	conn := e.dial(t)

	t.Run("features", func(t *testing.T) {
		out := e.mustLongarm(e.client, "version", "--json")
		for _, f := range []string{"session-start", "session-send", "session-send-if-idle", "session-stop", "sessions-json-v2", "tail", "run-session"} {
			if !strings.Contains(out, `"`+f+`"`) {
				t.Errorf("version --json lacks %s: %s", f, out)
			}
		}
	})

	t.Run("start_send_list_stop", func(t *testing.T) {
		out, r := e.sessionStart(t, "w1", "-s", "p1", "--", bins.helper, "repl")
		if r.code != 0 || !out.Created || out.Worker != "w1" || out.Session != "p1" || out.State != "running" {
			t.Fatalf("start: %+v %+v", out, r)
		}
		// idempotent: same spec, also via automatic placement (finds it)
		if out, r = e.sessionStart(t, "w1", "-s", "p1", "--", bins.helper, "repl"); r.code != 0 || out.Created {
			t.Fatalf("start again: %+v %+v", out, r)
		}
		if out, r = e.sessionStart(t, "-s", "p1", "--", bins.helper, "repl"); r.code != 0 || out.Created || out.Worker != "w1" {
			t.Fatalf("start auto: %+v %+v", out, r)
		}
		if _, r = e.sessionStart(t, "w1", "-s", "p1", "--", bins.helper, "repl", "other"); r.code != 2 || !strings.Contains(r.stderr, "another spec") {
			t.Fatalf("other spec: %+v", r)
		}
		waitOutput(t, func() string { return e.mustLongarm(e.client, "logs", "w1", "p1", "--session") }, "repl ready", 30*time.Second)

		e.mustLongarm(e.client, "session", "send", "w1", "-s", "p1", "--text", "hello send\n")
		e.mustLongarm(e.client, "session", "send", "-s", "p1", "--text", "abc", "--key", "enter") // worker found by name
		logs := func() string { return e.mustLongarm(e.client, "logs", "w1", "p1", "--session") }
		waitOutput(t, logs, "echo: hello send", 15*time.Second)
		waitOutput(t, logs, "echo: abc", 15*time.Second)
		// --if-idle-ms: input arrived just now -> 75, nothing sent
		if r := e.longarm(e.client, "session", "send", "w1", "-s", "p1", "--text", "busy\n", "--if-idle-ms", "600000"); r.code != 75 {
			t.Fatalf("if-idle: %+v", r)
		}
		time.Sleep(400 * time.Millisecond)
		r = e.longarm(e.client, "session", "send", "w1", "-s", "p1", "--text", "idle\n", "--if-idle-ms", "200", "--json")
		if r.code != 0 || !strings.Contains(r.stdout, `"inputSeq"`) {
			t.Fatalf("if-idle after the window: %+v", r)
		}
		waitOutput(t, logs, "echo: idle", 15*time.Second)
		if strings.Contains(plain(logs()), "echo: busy") {
			t.Fatal("a refused if-idle send was written")
		}
		if r := e.longarm(e.client, "session", "send", "w1", "-s", "nope", "--text", "x"); r.code != 3 {
			t.Fatalf("send to a missing session: %+v", r)
		}
		// the input is not in the session's metadata or the audit log
		for _, f := range []string{filepath.Join(e.dataDir, "sessions", "p1", "job.json"), filepath.Join(e.dataDir, "sessions", "p1", "status.json"), filepath.Join(e.dataDir, "audit.log")} {
			if b, _ := os.ReadFile(f); bytes.Contains(b, []byte("hello send")) {
				t.Errorf("input found in %s", f)
			}
		}

		rows, source := e.sessionJSON(t, "w1", "-s", "p1")
		if source != "workers" || len(rows) != 1 {
			t.Fatalf("sessions -s p1: %s %v", source, rows)
		}
		s := rows[0]
		if s["worker"] != "w1" || s["state"] != "JOB_STATE_RUNNING" || s["lastInputSource"] != "send" || s["inputSeq"] == "0" ||
			s["lastActivity"] == nil || s["lastOutput"] == nil || s["pid"] == float64(0) {
			t.Fatalf("session row: %v", s)
		}
		for _, k := range []string{"attached", "exitCode", "cwd", "dir", "workdir"} {
			if _, ok := s[k]; !ok {
				t.Errorf("no %s in %v", k, s)
			}
		}
		if fleet, _ := e.sessionJSON(t); len(fleet) == 0 || fleet[0]["worker"] != "w1" {
			t.Fatalf("fleet-wide sessions: %v", fleet)
		}
		if r := e.longarm(e.client, "sessions", "w1", "-s", "nope", "--json"); r.code != 3 || !strings.Contains(r.stdout, `"sessions": []`) {
			t.Fatalf("sessions -s nope: %+v", r)
		}

		// stop: Ctrl-C ends the repl (no kill needed); ended: send exits 2
		if out := e.mustLongarm(e.client, "session", "stop", "w1", "-s", "p1", "--grace", "10s"); strings.Contains(out, "running") {
			t.Fatalf("stop: %s", out)
		}
		if r := e.longarm(e.client, "session", "send", "w1", "-s", "p1", "--text", "x"); r.code != 2 || !strings.Contains(r.stderr, "ended") {
			t.Fatalf("send to an ended session: %+v", r)
		}
		// --grace 0 kills; an ended name is re-created by start
		e.sessionStart(t, "w1", "-s", "p2", "--", bins.helper, "sleep", "120")
		if out := e.mustLongarm(e.client, "session", "stop", "w1", "-s", "p2", "--grace", "0"); !strings.Contains(out, "p2 killed") {
			t.Fatalf("stop --grace 0: %s", out)
		}
		if out, r := e.sessionStart(t, "w1", "-s", "p1", "--", bins.helper, "repl"); r.code != 0 || !out.Created {
			t.Fatalf("re-create an ended name: %+v %+v", out, r)
		}
		// a program that exits at once: start exits with its code
		// a program that exits at once: start exits with its code (or, if
		// it was still running when start answered, the session ends so)
		if out, r := e.sessionStart(t, "w1", "-s", "p3", "--", bins.helper, "print", "1", "0", "5"); r.code != 5 && !(r.code == 0 && out.State == "running") {
			t.Fatalf("start of a program that exits: %+v %+v", out, r)
		}
		waitFor(t, "p3 to end", 15*time.Second, func() bool {
			rows, _ := e.sessionJSON(t, "w1", "-s", "p3", "--all")
			return len(rows) == 1 && rows[0]["state"] == "JOB_STATE_FAILED" && rows[0]["exitCode"] == float64(5)
		})
		e.mustLongarm(e.client, "session", "stop", "w1", "-s", "p1", "--grace", "0")
	})

	t.Run("attach_and_send_concurrently", func(t *testing.T) {
		e.sessionStart(t, "w1", "-s", "c1", "--cols", "200", "--rows", "30", "--", bins.helper, "repl")
		a := attachGo(t, conn, &pb.AttachOpen{Name: "c1", NoCreate: true, Size: &pb.TermSize{Cols: 200, Rows: 30}})
		a.waitFor("repl ready")
		long := strings.Repeat("x", 120)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { // a human typing while the program sends
			defer wg.Done()
			for i := 0; i < 30; i++ {
				a.send("a")
				time.Sleep(2 * time.Millisecond)
			}
		}()
		e.mustLongarm(e.client, "session", "send", "w1", "-s", "c1", "--text", long+"\n")
		wg.Wait()
		a.send("\r")
		a.waitFor(long) // the send arrived in one piece
		rows, _ := e.sessionJSON(t, "w1", "-s", "c1")
		if rows[0]["attached"] != float64(1) || rows[0]["lastInputSource"] != "attach" {
			t.Fatalf("after attach input: %v", rows[0])
		}
		a.detach()
		e.mustLongarm(e.client, "session", "stop", "w1", "-s", "c1", "--grace", "0")
	})

	t.Run("repo_worktree_and_run_session", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not installed")
		}
		repo := filepath.Join(e.root, "sessrepo")
		os.MkdirAll(repo, 0o755)
		git(t, repo, "init", "-q", "-b", "main")
		os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("in-the-worktree\n"), 0o644)
		git(t, repo, "add", ".")
		git(t, repo, "commit", "-qm", "c1")
		out, r := e.sessionStart(t, "w1", "-s", "wt1", "--repo", repo, "--", bins.helper, "repl")
		// worktrees/s.<name>.<generation>
		wantDir := filepath.Clean(out.Cwd)
		if r.code != 0 || !out.Created || filepath.Dir(wantDir) != filepath.Join(e.dataDir, "worktrees") || !strings.HasPrefix(filepath.Base(wantDir), "s.wt1.") {
			t.Fatalf("start --repo: %+v %+v", out, r)
		}
		if b, err := os.ReadFile(filepath.Join(wantDir, "tracked.txt")); err != nil || !strings.Contains(string(b), "in-the-worktree") {
			t.Fatalf("worktree content: %q %v", b, err)
		}
		// idempotent with the same checkout
		if out, r := e.sessionStart(t, "w1", "-s", "wt1", "--repo", repo, "--", bins.helper, "repl"); r.code != 0 || out.Created {
			t.Fatalf("start --repo again: %+v %+v", out, r)
		}
		rows, _ := e.sessionJSON(t, "w1", "-s", "wt1")
		if ws, _ := rows[0]["workspace"].(map[string]any); ws == nil || ws["ref"] != "HEAD" || ws["commit"] == "" || filepath.Clean(rows[0]["workdir"].(string)) != wantDir {
			t.Fatalf("workspace fields: %v", rows[0])
		}
		// one-shot commands in the session's worktree
		r = e.longarm(e.client, "run", "--session", "wt1", "-f", "--", bins.helper, "cat", "tracked.txt")
		if r.code != 0 || !strings.Contains(r.stdout, "in-the-worktree") {
			t.Fatalf("run --session: %+v", r)
		}
		if out := e.mustLongarm(e.client, "exec", "w1", "--session", "wt1", "--", bins.helper, "cat", "tracked.txt"); !strings.Contains(out, "in-the-worktree") {
			t.Fatalf("exec --session: %s", out)
		}
		if out := e.mustLongarm(e.client, "run", "w1", "--cwd", wantDir, "-f", "--", bins.helper, "cat", "tracked.txt"); !strings.Contains(out, "in-the-worktree") {
			t.Fatalf("run --cwd <worktree>: %s", out)
		}
		// the worktree outlives the session until gc collects the ended session
		e.mustLongarm(e.client, "session", "stop", "w1", "-s", "wt1", "--grace", "0")
		if _, err := os.Stat(wantDir); err != nil {
			t.Fatal("worktree removed when the session ended")
		}
		gc := e.mustLongarm(e.admin, "admin", "gc", "w1", "--keep", "0s", "--min-free", "0")
		if !strings.Contains(gc, "sessions/wt1") {
			t.Fatalf("gc: %s", gc)
		}
		if _, err := os.Stat(wantDir); err == nil {
			t.Fatal("worktree survived the gc of its session")
		}
	})

	t.Run("tail", func(t *testing.T) {
		p := filepath.Join(e.root, "tmia", "agent-events.jsonl")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(`{"e":1}`+"\n"), 0o644)
		if out := e.mustLongarm(e.client, "tail", "w1", p); out != `{"e":1}`+"\n" {
			t.Fatalf("tail: %q", out)
		}
		out := e.mustLongarm(e.client, "tail", "w1", p, "--json")
		if !strings.Contains(out, `"next":8`) || !strings.Contains(out, `"line":"{\"e\":1}"`) {
			t.Fatalf("tail --json: %s", out)
		}
		if r := e.longarm(e.client, "tail", "w1", filepath.Join(e.root, "nope.jsonl")); r.code != 3 {
			t.Fatalf("tail missing: %+v", r)
		}
		b := e.longarmBG(t, e.client, "tail", "w1", p, "-f", "--from-offset", "8", "--json")
		time.Sleep(500 * time.Millisecond)
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString(`{"e":2}` + "\n")
		f.Close()
		waitOutput(t, b.stdout.String, `"offset":8,"next":16`, 15*time.Second)
		if strings.Contains(b.stdout.String(), `{\"e\":1}`) {
			t.Fatal("tail --from-offset re-sent earlier lines")
		}
		b.cmd.Process.Kill()
	})

	t.Run("attach_in_pty", func(t *testing.T) {
		// longarm attach inside a pseudo terminal (ConPTY on Windows), as
		// when a desktop app runs it in xterm.js: raw mode, the initial size,
		// resizes, typed input and the detach keys, with session send
		// arriving at the same time.
		ctr, err := platform.NewContainer(platform.ContainerOptions{Name: "e2e-attach-pty"})
		if err != nil {
			t.Fatal(err)
		}
		defer ctr.Close()
		env := append(os.Environ(), "LONGARM_CONFIG_DIR="+e.client)
		p, err := platform.StartPTY(ctr, platform.PTYOptions{Argv: []string{bins.longarm, "attach", "w1", "-s", "pty1", "--", bins.helper, "repl"},
			Env: env, Cols: 100, Rows: 30, Dir: e.root})
		if err != nil {
			t.Fatal(err)
		}
		defer p.Release()
		var mu sync.Mutex
		var buf bytes.Buffer
		go func() {
			b := make([]byte, 4096)
			for {
				n, err := p.Read(b)
				mu.Lock()
				buf.Write(b[:n])
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
		screen := func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }
		waitOutput(t, screen, "repl ready", 30*time.Second)
		p.Write([]byte("size\r"))
		waitOutput(t, screen, "size 100x30", 15*time.Second)
		if err := p.Resize(120, 40); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second) // longarm polls the size (Windows) / gets SIGWINCH
		p.Write([]byte("size\r"))
		waitOutput(t, screen, "size 120x40", 15*time.Second)
		e.mustLongarm(e.client, "session", "send", "w1", "-s", "pty1", "--text", "from send\n")
		p.Write([]byte("typed here\r"))
		waitOutput(t, screen, "echo: from send", 15*time.Second)
		waitOutput(t, screen, "echo: typed here", 15*time.Second)
		p.Write([]byte{0x1d, 'd'}) // Ctrl-] d
		done := make(chan int, 1)
		go func() { code, _ := p.Wait(); done <- code }()
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("attach in a pty exited %d:\n%s", code, plain(screen()))
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("attach in a pty did not detach:\n%s", plain(screen()))
		}
		if rows, _ := e.sessionJSON(t, "w1", "-s", "pty1"); rows[0]["state"] != "JOB_STATE_RUNNING" || rows[0]["attached"] != float64(0) {
			t.Fatalf("after detach: %v", rows[0])
		}
		e.mustLongarm(e.client, "session", "stop", "w1", "-s", "pty1", "--grace", "0")
	})
}
