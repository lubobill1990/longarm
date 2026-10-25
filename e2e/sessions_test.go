package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/platform"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func (e *env) sessionStatus(t *testing.T, name string) *jobs.Status {
	t.Helper()
	st, err := jobs.Dir(filepath.Join(e.dataDir, "sessions", name)).ReadStatus()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

type sessionRow struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Attached int    `json:"attached"`
	ExitCode int    `json:"exitCode"`
}

func (e *env) sessions(t *testing.T, all bool) map[string]sessionRow {
	t.Helper()
	args := []string{"sessions", "w1", "--json"}
	if all {
		args = append(args, "--all")
	}
	var resp struct {
		Sessions []sessionRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(e.mustLongarm(e.client, args...)), &resp); err != nil {
		t.Fatal(err)
	}
	out := map[string]sessionRow{}
	for _, s := range resp.Sessions {
		out[s.Name] = s
	}
	return out
}

// TestSessions covers longarm attach / sessions / logs --session / attach --kill:
// create, detach, re-attach with replay, multiple clients, resize, secrets,
// exit codes, kill, and a session surviving a longarmd crash.
func TestSessions(t *testing.T) {
	e := newEnv(t)
	e.startLongarmd()
	outer := t
	conn := e.dial(t)

	t.Run("longarm_attach_detach_reattach", func(t *testing.T) {
		b := e.longarmBG(t, e.client, "attach", "w1", "--session", "s1", "--", bins.helper, "repl")
		waitOutput(t, b.stdout.String, "repl ready", 30*time.Second)
		b.stdin.Write([]byte("hello one\n"))
		waitOutput(t, b.stdout.String, "echo: hello one", 15*time.Second)
		b.stdin.Write([]byte{0x1d, 'd'}) // Ctrl-] d
		if code := b.wait(t, 15*time.Second); code != 0 {
			t.Fatalf("detach exit %d: %s", code, b.stderr.String())
		}
		if !strings.Contains(b.stderr.String(), "created session") || !strings.Contains(b.stderr.String(), "detached") {
			t.Fatalf("stderr: %s", b.stderr.String())
		}
		if s := e.sessions(t, false)["s1"]; s.State != "JOB_STATE_RUNNING" {
			t.Fatalf("session after detach: %+v", s)
		}
		// Re-attach: replay shows the earlier output, then it is live again.
		b = e.longarmBG(t, e.client, "attach", "w1", "--session", "s1")
		waitOutput(t, b.stdout.String, "echo: hello one", 15*time.Second)
		b.stdin.Write([]byte("hello two\n"))
		waitOutput(t, b.stdout.String, "echo: hello two", 15*time.Second)
		b.stdin.Close() // closing the terminal (EOF) also detaches
		if code := b.wait(t, 15*time.Second); code != 0 {
			t.Fatalf("eof exit %d: %s", code, b.stderr.String())
		}
		if !strings.Contains(b.stderr.String(), "attached to session") {
			t.Fatalf("stderr: %s", b.stderr.String())
		}
		// Transcript.
		logs := plain(e.mustLongarm(e.client, "logs", "w1", "s1", "--session"))
		if !strings.Contains(logs, "echo: hello one") || !strings.Contains(logs, "echo: hello two") {
			t.Fatalf("logs --session:\n%s", logs)
		}
		// --no-create on a missing session fails.
		if r := e.longarm(e.client, "attach", "w1", "--session", "nope", "--no-create"); r.code == 0 || !strings.Contains(r.stderr, "NotFound") {
			t.Fatalf("no-create: %+v", r)
		}
	})

	t.Run("multi_attach_resize", func(t *testing.T) {
		a := attachGo(t, conn, &pb.AttachOpen{Name: "s2", Argv: []string{bins.helper, "repl"}, Size: &pb.TermSize{Cols: 100, Rows: 30}})
		if !a.started.Created {
			t.Fatal("expected a new session")
		}
		a.waitFor("repl ready")
		a.send("size\r")
		a.waitFor("size 100x30")
		b := attachGo(t, conn, &pb.AttachOpen{Name: "s2", NoReplay: true})
		if b.started.Created || b.started.Session.Attached < 1 {
			t.Fatalf("second attach: %+v", b.started)
		}
		if strings.Contains(plain(b.out.String()), "repl ready") {
			t.Fatal("no_replay still replayed")
		}
		if s := e.sessions(t, false)["s2"]; s.Attached != 2 {
			t.Fatalf("attached = %d, want 2", s.Attached)
		}
		// Input from one client, output on both; resize from the other.
		b.send("from b\r")
		a.waitFor("echo: from b")
		b.waitFor("echo: from b")
		b.resize(120, 40)
		time.Sleep(300 * time.Millisecond)
		a.send("size\r")
		a.waitFor("size 120x40")
		b.detach()
		a.send("exit 7\r")
		a.waitEnd()
		if !a.exited || a.exit != 7 {
			t.Fatalf("exit = %v %d (%v)", a.exited, a.exit, a.err)
		}
		if s := e.sessions(t, true)["s2"]; s.State != "JOB_STATE_FAILED" || s.ExitCode != 7 {
			t.Fatalf("ended session: %+v", s)
		}
		// longarm attach mirrors the exit code of an ending session.
		b2 := e.longarmBG(t, e.client, "attach", "w1", "--session", "s2b", "--", bins.helper, "repl")
		waitOutput(t, b2.stdout.String, "repl ready", 30*time.Second)
		b2.stdin.Write([]byte("exit 5\n"))
		if code := b2.wait(t, 15*time.Second); code != 5 {
			t.Fatalf("longarm attach exit %d, want 5", code)
		}
		// An ended name is reused by the next attach.
		c := attachGo(t, conn, &pb.AttachOpen{Name: "s2", Argv: []string{bins.helper, "repl"}})
		if !c.started.Created {
			t.Fatal("ended session was not re-created")
		}
		c.waitFor("repl ready")
		c.detach()
	})

	t.Run("secrets_not_on_disk", func(t *testing.T) {
		const secret = "sess-s3cr3t-Value-42"
		os.Setenv("E2E_SESSION_SECRET", secret)
		defer os.Unsetenv("E2E_SESSION_SECRET")
		b := e.longarmBG(t, e.client, "attach", "w1", "--session", "sec", "--secret-env", "E2E_SESSION_SECRET", "--", bins.helper, "repl")
		waitOutput(t, b.stdout.String, "repl ready", 30*time.Second)
		b.stdin.Write([]byte("env E2E_SESSION_SECRET\n"))
		// The user's own terminal shows the value; the transcript on disk must not.
		waitOutput(t, b.stdout.String, "env E2E_SESSION_SECRET="+secret, 15*time.Second)
		b.stdin.Write([]byte{0x1d, 'd'})
		b.wait(t, 15*time.Second)
		time.Sleep(300 * time.Millisecond)
		logs := plain(e.mustLongarm(e.client, "logs", "w1", "sec", "--session"))
		if !strings.Contains(logs, "env E2E_SESSION_SECRET=***") {
			t.Fatalf("transcript not redacted:\n%s", logs)
		}
		filepath.Walk(e.dataDir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || strings.HasSuffix(p, ".lock") {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(secret)) {
				t.Errorf("secret found on disk in %s", p)
			}
			return nil
		})
	})

	t.Run("survives_longarmd_crash", func(t *testing.T) {
		a := attachGo(t, conn, &pb.AttachOpen{Name: "s4", Argv: []string{bins.helper, "repl"}})
		a.waitFor("repl ready")
		a.send("before crash\r")
		a.waitFor("echo: before crash")
		st := e.sessionStatus(t, "s4")
		// A longarm attach CLI rides through the outage by re-attaching.
		cli := e.longarmBG(t, e.client, "attach", "w1", "--session", "s4", "--no-replay")
		waitOutput(t, cli.stderr.String, "attached to session", 30*time.Second)
		e.killLongarmd()
		a.waitEnd() // the stream broke with longarmd
		if !platform.ProcessAlive(st.ShimPID, st.ShimStart) || !platform.ProcessAlive(st.PID, st.PIDStart) {
			t.Fatal("session died with longarmd")
		}
		e.startLongarmd()
		conn2 := e.dial(outer)
		b := attachGo(t, conn2, &pb.AttachOpen{Name: "s4", NoCreate: true})
		b.waitFor("echo: before crash") // replay from the shim's ring
		b.send("after crash\r")
		b.waitFor("echo: after crash")
		b.detach()
		waitOutput(t, cli.stderr.String, "re-attaching", 30*time.Second)
		cli.stdin.Write([]byte("from cli\n")) // buffered until the CLI is back
		waitOutput(t, cli.stdout.String, "echo: from cli", 30*time.Second)
		cli.stdin.Write([]byte{0x1d, 'd'})
		if code := cli.wait(t, 15*time.Second); code != 0 {
			t.Fatalf("cli exit %d: %s", code, cli.stderr.String())
		}
		conn = conn2
	})

	t.Run("kill", func(t *testing.T) {
		pidfile := filepath.Join(e.root, "sess-tree.pids")
		a := attachGo(t, conn, &pb.AttachOpen{Name: "s5", Argv: []string{bins.helper, "tree", pidfile}})
		var pids []string
		waitFor(t, "5 tree processes", 30*time.Second, func() bool {
			b, _ := os.ReadFile(pidfile)
			pids = strings.Fields(string(b))
			return len(pids) == 5
		})
		out := e.mustLongarm(e.client, "attach", "w1", "--session", "s5", "--kill")
		if !strings.Contains(out, "s5 killed") {
			t.Fatalf("kill: %s", out)
		}
		a.waitEnd()
		if !a.exited || a.exit != platform.KilledExitCode {
			t.Fatalf("attached client saw exit %v %d", a.exited, a.exit)
		}
		st := e.sessionStatus(t, "s5")
		waitFor(t, "session tree to die", 10*time.Second, func() bool { return !platform.ProcessAlive(st.PID, st.PIDStart) })
	})

	t.Run("default_shell", func(t *testing.T) {
		a := attachGo(t, conn, &pb.AttachOpen{Name: "sh"})
		cmd := strings.ToLower(strings.Join(a.started.Session.Argv, " "))
		want := "sh"
		if runtime.GOOS == "windows" {
			want = "pwsh"
			if !strings.Contains(cmd, "pwsh") && !strings.Contains(cmd, "powershell") {
				t.Fatalf("default shell %q", cmd)
			}
		} else if !strings.Contains(cmd, want) {
			t.Fatalf("default shell %q", cmd)
		}
		a.send("echo shell-$((6*7))\r")
		if runtime.GOOS == "windows" {
			a.send("echo shell-$(6*7)\r")
		}
		a.waitFor("shell-42")
		a.send("exit\r")
		a.waitEnd()
	})
}
