package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/term"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/steer"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// fastSteer shortens a steerable run's timings in the test shim.
func fastSteer(c *steer.Config) {
	c.Quiet, c.HumanGrace, c.ScreenQuiet = 500*time.Millisecond, time.Second, 200*time.Millisecond
	c.ExitGrace, c.KillAfter = 5*time.Second, 2*time.Second
}

// fakeAgent plays Claude Code / Copilot CLI for a steerable run: the
// folder trust screen (Claude), its hooks from the run's configuration, a
// turn per line typed ("go" answers the task), /exit.
func fakeAgent(agent string) int {
	if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer term.Restore(int(os.Stdin.Fd()), old)
	}
	hooks, err := fakeHooks(agent)
	if err != nil {
		fmt.Printf("fake %s: %v\r\n", agent, err)
		return 7
	}
	home := os.Getenv("COPILOT_HOME")
	transcript := filepath.Join(os.TempDir(), "fake-claude.jsonl")
	if agent == steer.Copilot {
		wd, _ := os.Getwd()
		if b, _ := os.ReadFile(filepath.Join(home, "config.json")); !strings.Contains(string(b), strings.ReplaceAll(wd, `\`, `\\`)) {
			fmt.Printf("fake copilot: %s is not trusted\r\n", wd)
			return 8
		}
		transcript = filepath.Join(home, "session-state", "fake", "events.jsonl")
		os.MkdirAll(filepath.Dir(transcript), 0o700)
	}
	fire := func(ev string, payload map[string]any) {
		if agent == steer.Claude {
			payload["session_id"], payload["transcript_path"] = "fake-1", transcript
		} else {
			payload["sessionId"], payload["transcriptPath"] = "fake-1", transcript
		}
		b, _ := json.Marshal(payload)
		args := splitCommand(hooks[ev])
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stdin = strings.NewReader(string(b))
		if err := cmd.Run(); err != nil {
			fmt.Printf("hook %s: %v\r\n", ev, err)
		}
	}
	in := bufio.NewReader(os.Stdin)
	if agent == steer.Claude {
		fmt.Print("\x1b[2JQuick safety check: Is this a project you created or one you trust?\r\n > No, exit\r\n   Yes, I trust this folder\r\n")
		if !waitKeys(in, steer.KeyDown) {
			return 9
		}
		fmt.Print("\x1b[2;1H   No, exit\r\n ❯ Yes, I trust this folder\r\n")
		if !waitKeys(in, steer.KeyEnter) {
			return 9
		}
	}
	submit, stop := "UserPromptSubmit", "Stop"
	if agent == steer.Copilot {
		submit, stop = "userPromptSubmitted", "agentStop"
	}
	turn := func(prompt, answer string) {
		fire(submit, map[string]any{"prompt": prompt})
		fmt.Print("working on it\r\n")
		if agent == steer.Copilot {
			f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			fmt.Fprintf(f, `{"type":"user.message","data":{"content":%q}}`+"\n", prompt)
			fmt.Fprintf(f, `{"type":"assistant.message","data":{"content":%q}}`+"\n", answer)
			f.Close()
			fire(stop, map[string]any{"stopReason": "end_turn"})
		} else {
			fire(stop, map[string]any{"last_assistant_message": answer, "background_tasks": []any{}})
		}
		fmt.Print(answer + "\r\n> ")
	}
	fire(submit, map[string]any{"prompt": "the task"})
	fmt.Print("working on it\r\n")
	n := 0
	var line []byte
	for {
		b, err := in.ReadByte()
		if err != nil {
			return 10
		}
		switch b {
		case 0x03, 0x1b: // Ctrl-C clears the input; Esc closes a dialog
			line = line[:0]
		case '\r':
			text := string(line)
			line = line[:0]
			switch {
			case text == "/exit":
				if agent == steer.Claude {
					fire("SessionEnd", map[string]any{"reason": "prompt_input_exit"})
				} else {
					fire("sessionEnd", map[string]any{"reason": "user_exit"})
				}
				return 0
			case text == "go" && n == 0:
				n++
				if agent == steer.Copilot { // the answer from the transcript
					f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
					fmt.Fprint(f, `{"type":"user.message","data":{"content":"the task"}}`+"\n")
					fmt.Fprint(f, `{"type":"assistant.message","data":{"content":"answer one"}}`+"\n")
					f.Close()
					fire(stop, map[string]any{"stopReason": "end_turn"})
				} else {
					fire(stop, map[string]any{"last_assistant_message": "answer one", "background_tasks": []any{}})
				}
				fmt.Print("answer one\r\n> ")
			default:
				n++
				turn(text, fmt.Sprintf("answer %d", n))
			}
		default:
			line = append(line, b)
		}
	}
}

// waitKeys reads until keys arrived (false: stdin ended).
func waitKeys(in *bufio.Reader, keys string) bool {
	var got []byte
	for !strings.HasSuffix(string(got), keys) {
		b, err := in.ReadByte()
		if err != nil {
			return false
		}
		got = append(got, b)
	}
	return true
}

// fakeHooks reads the hook commands of the run's configuration.
func fakeHooks(agent string) (map[string]string, error) {
	out := map[string]string{}
	if agent == steer.Claude {
		var settings string
		for i, a := range os.Args {
			if a == "--settings" && i+1 < len(os.Args) {
				settings = os.Args[i+1]
			}
		}
		b, err := os.ReadFile(settings)
		if err != nil {
			return nil, err
		}
		var cfg struct {
			Hooks map[string][]struct{ Hooks []struct{ Command string } }
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, err
		}
		for ev, g := range cfg.Hooks {
			out[ev] = g[0].Hooks[0].Command
		}
		return out, nil
	}
	b, err := os.ReadFile(steer.CopilotHooksFile(os.Getenv("COPILOT_HOME")))
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(string(b), "\xef\xbb\xbf") {
		return nil, errors.New("hooks file with a byte order mark")
	}
	var cfg struct {
		Hooks map[string][]struct{ Bash string }
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	for ev, h := range cfg.Hooks {
		out[ev] = h[0].Bash
	}
	return out, nil
}

// splitCommand splits a hook command: words, "quoted words".
func splitCommand(s string) []string {
	var out []string
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		if s[0] == '"' {
			end := strings.IndexByte(s[1:], '"')
			out = append(out, s[1:1+end])
			s = s[end+2:]
			continue
		}
		w, rest, _ := strings.Cut(s, " ")
		out = append(out, w)
		s = rest
	}
	return out
}

// fakeAgentExe copies the test binary to <tmp>/claude(.exe) / copilot.
func fakeAgentExe(t *testing.T, agent string) string {
	t.Helper()
	exe, _ := os.Executable()
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	name := agent
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSteerableRun(t *testing.T) {
	e := newAuthzEnv(t)
	for _, agent := range []string{steer.Claude, steer.Copilot} {
		t.Run(agent, func(t *testing.T) {
			exe := fakeAgentExe(t, agent)
			id := "STEER" + strings.ToUpper(agent)
			argv := []string{exe, "the task"}
			req := &pb.SubmitRequest{Spec: &pb.JobSpec{Id: id, Argv: argv, Env: map[string]string{"LONGARM_TEST_FAKE_AGENT": agent},
				Steer: &pb.SteerSpec{Agent: agent}}}
			if agent == steer.Copilot {
				req.Spec.Argv = []string{exe, "-i", "the task"}
				req.AgentAuth = &pb.AgentAuth{Agents: []string{"copilot"}, CopilotHome: true}
			}
			jobs := pb.NewJobsClient(e.alice)
			job, err := jobs.Submit(cctx(t), req)
			if err != nil {
				t.Fatal(err)
			}
			if job.Spec.GetSteer().GetAgent() != agent {
				t.Fatalf("spec.steer = %v", job.Spec.GetSteer())
			}
			// working once the prompt is submitted (Claude: after the trust screen)
			waitSteer(t, jobs, id, "working")

			// another user may not watch or type
			if _, err := pb.NewSessionsClient(e.bob).Send(cctx(t), &pb.SendSessionRequest{Name: id, Job: true, Data: []byte("x")}); status.Code(err) == codes.OK {
				t.Fatal("bob typed into alice's run")
			}
			if out := attachOutput(t, e.alice, id); !strings.Contains(out, "working on it") {
				t.Fatalf("attach: %q", out)
			}
			// steer: "go" finishes the task; the run ends a second later
			if _, err := pb.NewSessionsClient(e.alice).Send(cctx(t), &pb.SendSessionRequest{Name: id, Job: true, Data: []byte("go\r")}); err != nil {
				t.Fatal(err)
			}
			end := waitJob(t, jobs, id)
			if end.State != pb.JobState_JOB_STATE_DONE || end.ExitCode != 0 {
				t.Fatalf("ended %s %d: %s", end.State, end.ExitCode, end.Reason)
			}
			if st := end.GetSteer(); st.GetState() != "done" || st.Turns != 1 || st.SessionId != "fake-1" {
				t.Fatalf("steer = %v", st)
			}
			var log strings.Builder
			ls, err := jobs.Logs(cctx(t), &pb.LogsRequest{Id: id})
			if err != nil {
				t.Fatal(err)
			}
			for {
				c, err := ls.Recv()
				if err != nil {
					break
				}
				log.Write(c.Data)
			}
			if log.String() != "answer one\n" {
				t.Fatalf("log %q", log.String())
			}
		})
	}

	t.Run("refused", func(t *testing.T) {
		exe := fakeAgentExe(t, steer.Copilot)
		jobs := pb.NewJobsClient(e.alice)
		for _, spec := range []*pb.JobSpec{
			{Id: "BAD1", Argv: []string{"pwsh"}, Steer: &pb.SteerSpec{Agent: "claude"}},
			{Id: "BAD2", Argv: []string{exe}, Steer: &pb.SteerSpec{Agent: "copilot"}}, // no private COPILOT_HOME
			{Id: "BAD3", Argv: []string{exe}, Steer: &pb.SteerSpec{Agent: "aider"}},
		} {
			if _, err := jobs.Submit(cctx(t), &pb.SubmitRequest{Spec: spec}); status.Code(err) != codes.InvalidArgument {
				t.Errorf("%s: %v", spec.Id, err)
			}
		}
		sleepJob(t, e.alice, "PLAIN")
		if _, err := pb.NewSessionsClient(e.alice).Send(cctx(t), &pb.SendSessionRequest{Name: "PLAIN", Job: true, Data: []byte("x")}); status.Code(err) != codes.FailedPrecondition {
			t.Errorf("send to a plain job: %v", err)
		}
	})
}

func waitSteer(t *testing.T, jobs pb.JobsClient, id, state string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		j, err := jobs.Get(cctx(t), &pb.GetJobRequest{Id: id})
		if err != nil {
			t.Fatal(err)
		}
		if j.GetSteer().GetState() == state {
			return
		}
		if terminalState(j.State) || time.Now().After(deadline) {
			t.Fatalf("job %s, steer %v; want %s (reason %s)", j.State, j.GetSteer(), state, j.Reason)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func terminalState(s pb.JobState) bool {
	switch s {
	case pb.JobState_JOB_STATE_DONE, pb.JobState_JOB_STATE_FAILED, pb.JobState_JOB_STATE_KILLED, pb.JobState_JOB_STATE_LOST:
		return true
	}
	return false
}

func waitJob(t *testing.T, jobs pb.JobsClient, id string) *pb.Job {
	t.Helper()
	st, err := jobs.Wait(cctx(t), &pb.WaitRequest{Id: id})
	if err != nil {
		t.Fatal(err)
	}
	var last *pb.Job
	for {
		j, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return last
		}
		if err != nil {
			t.Fatal(err)
		}
		last = j
	}
}

// attachOutput attaches to a steerable job and returns the replayed output.
func attachOutput(t *testing.T, conn *grpc.ClientConn, id string) string {
	t.Helper()
	st, err := pb.NewSessionsClient(conn).Attach(cctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Open{Open: &pb.AttachOpen{Name: id, Job: true}}}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "working on it") && time.Now().Before(deadline) {
		m, err := st.Recv()
		if err != nil {
			t.Fatalf("attach: %v (got %q)", err, out.String())
		}
		out.Write(m.GetData())
	}
	st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Detach{Detach: true}})
	return out.String()
}
