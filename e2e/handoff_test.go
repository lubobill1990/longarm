package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// handoffOut is handoff --json.
type handoffOut struct {
	Agent     string   `json:"agent"`
	SessionID string   `json:"sessionId"`
	From      string   `json:"from"`
	To        string   `json:"to"`
	Worktree  string   `json:"worktree"`
	Cwd       string   `json:"cwd"`
	Branch    string   `json:"branch"`
	Session   string   `json:"session"`
	Snapshot  string   `json:"snapshot"`
	Base      string   `json:"base"`
	Files     int      `json:"files"`
	Resume    []string `json:"resume"`
	Transfer  string   `json:"transfer"`
	Relayed   int64    `json:"relayedBytes"`
	Temp      string   `json:"tempBranch"`
}

// claudeDir is Claude Code's project directory name of a path.
func claudeDir(p string) string {
	b := []byte(p)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

func homeEnv(home, path string) []string {
	return []string{"HOME=" + home, "USERPROFILE=" + home, "PATH=" + path, "CLAUDE_CONFIG_DIR=", "COPILOT_HOME="}
}

func mustWrite(t *testing.T, p, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// firstCwd is the first "cwd" of a JSON-lines transcript.
func firstCwd(t *testing.T, p string) string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &l) == nil && l.Cwd != "" {
			return l.Cwd
		}
	}
	return ""
}

// agentRecord waits for the fake agent's record of a resume in home.
func agentRecord(t *testing.T, home, id string) map[string]any {
	t.Helper()
	var rec map[string]any
	waitFor(t, "the fake agent on "+filepath.Base(home), 60*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(home, "fake-agent.log"))
		if err != nil {
			return false
		}
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var r map[string]any
			if json.Unmarshal([]byte(l), &r) == nil && strings.Contains(fmt.Sprint(r["args"]), id) {
				rec = r
			}
		}
		return rec != nil
	})
	return rec
}

// TestAgentHandoff moves a (fake) Claude Code session local -> w1 -> w2 ->
// local and a Copilot CLI session local -> w1: the transcript arrives at
// the agent's path for the new worktree with its paths rewritten, the code
// with its uncommitted and untracked changes, the source is marked handed
// off and the agent is resumed there (docs/usage.md#hand-off-an-agent-session).
// handoffEnv is two workers (w1, w2) and a client, each with its own home
// and the fake agents on PATH.
type handoffEnv struct {
	t                     *testing.T
	e, e2                 *env
	root, path, fakebin   string
	w1home, w2home, chome string
	extra                 []string // more client environment
}

func newHandoffEnv(t *testing.T, workerEnv ...string) *handoffEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var -> /private/var, as git and getwd see it
	if err != nil {
		t.Fatal(err)
	}
	h := &handoffEnv{t: t, root: root, e: newEnvAt(t, root)}
	h.fakebin = filepath.Join(root, "fakebin")
	os.MkdirAll(h.fakebin, 0o755)
	copyExe(t, bins.helper, filepath.Join(h.fakebin, exeName("claude")))
	copyExe(t, bins.helper, filepath.Join(h.fakebin, exeName("copilot")))
	h.path = h.fakebin + string(os.PathListSeparator) + os.Getenv("PATH")
	h.w1home, h.w2home, h.chome = filepath.Join(root, "w1home"), filepath.Join(root, "w2home"), filepath.Join(root, "chome")
	for _, d := range []string{h.w1home, h.w2home, h.chome} {
		os.MkdirAll(d, 0o755)
	}
	h.e.longarmdEnv = append(homeEnv(h.w1home, h.path), workerEnv...)
	h.e.startLongarmd()
	// w2: a second longarmd of the same CA with its own home
	h.e2 = &env{t: t, root: filepath.Join(root, "w2root"), dataDir: filepath.Join(root, "w2data"), port: freePort(t),
		longarmdEnv: append(homeEnv(h.w2home, h.path), workerEnv...)}
	os.MkdirAll(h.e2.root, 0o755)
	h.e.mustLongarm(h.e.client, "admin", "issue", "--role", "worker", "--name", "w2", "--out", filepath.Join(h.e2.dataDir, "tls"))
	t.Cleanup(h.e2.cleanup)
	h.e2.startLongarmd()
	workers := fmt.Sprintf("[[worker]]\nname = \"w1\"\naddress = \"127.0.0.1:%d\"\ntransport = \"tcp\"\n\n[[worker]]\nname = \"w2\"\naddress = \"127.0.0.1:%d\"\ntransport = \"tcp\"\n",
		h.e.port, h.e2.port)
	if err := os.WriteFile(filepath.Join(h.e.client, "workers.toml"), []byte(workers), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

// client runs longarm as the client in dir (its home, the fake agents on PATH).
func (h *handoffEnv) client(dir string, args ...string) result {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bins.longarm, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append(homeEnv(h.chome, h.path), "LONGARM_CONFIG_DIR="+h.e.client, "LONGARM_WORKER=", "LONGARM_SESSION=")...)
	cmd.Env = append(cmd.Env, h.extra...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		h.t.Fatalf("longarm %v: %v", args, err)
	}
	return result{out.String(), errb.String(), code}
}

// handoff runs handoff --json and decodes it (exit 0 required).
func (h *handoffEnv) handoff(dir string, args ...string) (handoffOut, result) {
	h.t.Helper()
	r := h.client(dir, append([]string{"handoff", "--json"}, args...)...)
	if r.code != 0 {
		h.t.Fatalf("handoff %v: exit %d\n%s\n%s", args, r.code, r.stdout, r.stderr)
	}
	var out handoffOut
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		h.t.Fatalf("handoff --json: %v\n%s", err, r.stdout)
	}
	return out, r
}

func TestAgentHandoff(t *testing.T) {
	h := newHandoffEnv(t)
	e, e2, root, w1home, w2home, chome := h.e, h.e2, h.root, h.w1home, h.w2home, h.chome
	client := h.client
	handoff := func(dir string, args ...string) handoffOut {
		t.Helper()
		out, _ := h.handoff(dir, args...)
		return out
	}

	t.Run("features", func(t *testing.T) {
		out := client(root, "version", "--json").stdout
		for _, f := range []string{"agent-handoff", "agent-sessions"} {
			if !strings.Contains(out, `"`+f+`"`) {
				t.Errorf("version --json lacks %s", f)
			}
		}
	})

	// the local checkout: a commit, an uncommitted change, an untracked file
	repo := filepath.Join(root, "src", "app")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "core.autocrlf", "false")
	mustWrite(t, filepath.Join(repo, "tracked.txt"), "v1\n")
	mustWrite(t, filepath.Join(repo, ".gitignore"), "*.log\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "c1")
	mustWrite(t, filepath.Join(repo, "tracked.txt"), "v2 uncommitted\n")
	mustWrite(t, filepath.Join(repo, "notes", "untracked.txt"), "untracked\n")
	mustWrite(t, filepath.Join(repo, "debug.log"), "ignored\n")

	// a Claude Code session of the checkout
	id := "5e55a7c1-0000-4000-8000-00000000c1a0"
	short := "5e55a7c1"
	srcProj := filepath.Join(chome, ".claude", "projects", claudeDir(repo))
	mustWrite(t, filepath.Join(srcProj, id+".jsonl"), strings.Join([]string{
		fmt.Sprintf(`{"type":"user","cwd":%s,"sessionId":%q,"version":"1.0.0","message":{"role":"user","content":"please fix tracked.txt"}}`, jsonQ(repo), id),
		fmt.Sprintf(`{"type":"assistant","cwd":%s,"message":{"role":"assistant","content":[{"type":"tool_use","input":{"file_path":%s}}]}}`, jsonQ(repo), jsonQ(filepath.Join(repo, "tracked.txt"))),
	}, "\n")+"\n")
	mustWrite(t, filepath.Join(srcProj, id, "tool-results", "r1.txt"), "tool output\n")
	mustWrite(t, filepath.Join(chome, ".claude", "file-history", id, "f1@v1"), "before\n")

	t.Run("agent_sessions_local", func(t *testing.T) {
		r := client(repo, "agent-sessions", "--json", "--repo-path", repo)
		if r.code != 0 || !strings.Contains(r.stdout, id) || !strings.Contains(r.stdout, `"title": "please fix tracked.txt"`) {
			t.Fatalf("agent-sessions: %+v", r)
		}
	})

	var h1, h2 handoffOut
	t.Run("local_to_w1", func(t *testing.T) {
		h1 = handoff(repo, "--to", "w1", "--agent", "claude", "--session", id, "--no-attach")
		wantTop := filepath.Join(w1home, "longarm-handoff", "app-"+short)
		if h1.Worktree != wantTop || h1.Cwd != wantTop || h1.Branch != "handoff/"+short || h1.Session != "handoff-"+short {
			t.Fatalf("handoff: %+v (want worktree %s)", h1, wantTop)
		}
		if got := readText(t, filepath.Join(wantTop, "tracked.txt")); got != "v2 uncommitted\n" {
			t.Errorf("tracked.txt on w1: %q", got)
		}
		if got := readText(t, filepath.Join(wantTop, "notes", "untracked.txt")); got != "untracked\n" {
			t.Errorf("untracked file on w1: %q", got)
		}
		if _, err := os.Stat(filepath.Join(wantTop, "debug.log")); err == nil {
			t.Error("an ignored file travelled")
		}
		if st := git(t, wantTop, "status", "--porcelain"); !strings.Contains(st, "M tracked.txt") || !strings.Contains(st, "?? notes/") {
			t.Errorf("w1 worktree status:\n%s", st)
		}
		tr := filepath.Join(w1home, ".claude", "projects", claudeDir(wantTop), id+".jsonl")
		if got := firstCwd(t, tr); got != wantTop {
			t.Errorf("cwd in the w1 transcript: %q", got)
		}
		if body := readText(t, tr); !strings.Contains(body, jsonQ(filepath.Join(wantTop, "tracked.txt"))) || strings.Contains(body, jsonQ(repo)) {
			t.Error("file_path not rewritten in the w1 transcript")
		}
		if _, err := os.Stat(filepath.Join(w1home, ".claude", "projects", claudeDir(wantTop), id, "tool-results", "r1.txt")); err != nil {
			t.Errorf("session dir: %v", err)
		}
		if _, err := os.Stat(filepath.Join(w1home, ".claude", "file-history", id, "f1@v1")); err != nil {
			t.Errorf("file history: %v", err)
		}
		var mark map[string]any
		if err := json.Unmarshal([]byte(readText(t, filepath.Join(srcProj, id+".handoff.json"))), &mark); err != nil || mark["handedOffTo"] != "w1" {
			t.Errorf("source marker: %v %v", mark, err)
		}
		if _, err := os.Stat(filepath.Join(srcProj, id+".jsonl")); err != nil {
			t.Error("the source transcript must stay (backup)")
		}
		rec := agentRecord(t, w1home, id)
		if rec["cwd"] != wantTop || rec["transcript"] != true || rec["recordedCwd"] != wantTop || rec["worker"] != "w1" || rec["session"] != "handoff-"+short {
			t.Errorf("the agent on w1: %v", rec)
		}
		// listed on w1; the handed-off local copy is no longer listed locally
		if r := client(repo, "agent-sessions", "w1", "--json"); r.code != 0 || !strings.Contains(r.stdout, id) {
			t.Errorf("agent-sessions w1: %+v", r)
		}
		if r := client(repo, "agent-sessions", "--json"); r.code != 0 || strings.Contains(r.stdout, id) {
			t.Errorf("agent-sessions local still lists the handed-off copy: %+v", r)
		}
		// handing it off from here again is refused: it is on w1 now
		if r := client(repo, "handoff", "--to", "w2", "--session", id, "--no-attach"); r.code != 2 || !strings.Contains(r.stderr, "handed off to w1") {
			t.Errorf("second handoff of a handed-off copy: %+v", r)
		}
	})

	t.Run("w1_to_w2", func(t *testing.T) {
		h2 = handoff(repo, "--from", "w1", "--to", "w2", "--session", id, "--no-attach", "--idle", "1s", "--grace", "5s")
		wantTop := filepath.Join(w2home, "longarm-handoff", "app-"+short)
		if h2.Worktree != wantTop || h2.Session != "handoff-"+short {
			t.Fatalf("handoff: %+v", h2)
		}
		// worker -> worker directly: this client relayed no commit bytes
		if h2.Transfer != "direct" || h2.Relayed != 0 {
			t.Errorf("transfer %q, %d bytes relayed by the client (want direct, 0)", h2.Transfer, h2.Relayed)
		}
		// the source session was stopped and its copy marked
		rows, _ := e.sessionJSON(t, "w1", "-s", "handoff-"+short, "--all")
		if len(rows) != 1 || rows[0]["state"] == "JOB_STATE_RUNNING" {
			t.Errorf("w1 session after the handoff: %v", rows)
		}
		var mark map[string]any
		w1tr := filepath.Join(w1home, ".claude", "projects", claudeDir(h1.Worktree), id)
		if err := json.Unmarshal([]byte(readText(t, w1tr+".handoff.json")), &mark); err != nil || mark["handedOffTo"] != "w2" {
			t.Errorf("w1 marker: %v %v", mark, err)
		}
		if got := readText(t, filepath.Join(wantTop, "tracked.txt")); got != "v2 uncommitted\n" {
			t.Errorf("tracked.txt on w2: %q", got)
		}
		if got := readText(t, filepath.Join(wantTop, "notes", "untracked.txt")); got != "untracked\n" {
			t.Errorf("untracked file on w2: %q", got)
		}
		tr := filepath.Join(w2home, ".claude", "projects", claudeDir(wantTop), id+".jsonl")
		body := readText(t, tr)
		if firstCwd(t, tr) != wantTop || !strings.Contains(body, "continued on w1home") || strings.Contains(body, jsonQ(h1.Worktree)) {
			t.Errorf("w2 transcript: cwd %q, has w1's turn %v", firstCwd(t, tr), strings.Contains(body, "continued on w1home"))
		}
		if rec := agentRecord(t, w2home, id); rec["cwd"] != wantTop || rec["recordedCwd"] != wantTop || rec["worker"] != "w2" {
			t.Errorf("the agent on w2: %v", rec)
		}
	})

	t.Run("w2_to_local", func(t *testing.T) {
		// not --json: the agent runs in this terminal (stdin ends: it exits)
		r := client(repo, "handoff", "--from", "w2", "--to", "local", "--session", id, "--idle", "1s", "--grace", "5s")
		if r.code != 0 {
			t.Fatalf("handoff: %+v", r)
		}
		wantTop := filepath.Join(root, "src", "app-handoff-"+short)
		if !strings.Contains(r.stdout, "handed off w2 -> local: "+wantTop) {
			t.Fatalf("handoff output: %s", r.stdout)
		}
		if b := git(t, wantTop, "rev-parse", "--abbrev-ref", "HEAD"); b != "handoff/"+short {
			t.Errorf("local worktree branch %s", b)
		}
		if got := readText(t, filepath.Join(wantTop, "tracked.txt")); got != "v2 uncommitted\n" {
			t.Errorf("tracked.txt locally: %q", got)
		}
		if got := readText(t, filepath.Join(wantTop, "notes", "untracked.txt")); got != "untracked\n" {
			t.Errorf("untracked file locally: %q", got)
		}
		if b := git(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); b != "main" {
			t.Errorf("the original checkout moved to %s", b)
		}
		if got := readText(t, filepath.Join(repo, "tracked.txt")); got != "v2 uncommitted\n" {
			t.Errorf("the original checkout changed: %q", got)
		}
		tr := filepath.Join(chome, ".claude", "projects", claudeDir(wantTop), id+".jsonl")
		body := readText(t, tr)
		if firstCwd(t, tr) != wantTop || !strings.Contains(body, "continued on w1home") || !strings.Contains(body, "continued on w2home") {
			t.Errorf("local transcript: cwd %q", firstCwd(t, tr))
		}
		if rec := agentRecord(t, chome, id); rec["cwd"] != wantTop || rec["recordedCwd"] != wantTop {
			t.Errorf("the agent here: %v", rec)
		}
		// the original local copy stays where it was, marked; the id-keyed
		// state it had was kept as a backup, not overwritten
		if _, err := os.Stat(filepath.Join(srcProj, id+".jsonl")); err != nil {
			t.Error("the original transcript is gone")
		}
		bk, _ := filepath.Glob(filepath.Join(chome, ".claude", "longarm-handoff-backup", id+"-*", "config", "file-history", id, "f1@v1"))
		if len(bk) != 1 {
			t.Errorf("backup of the earlier file history: %v", bk)
		}
		if _, err := os.Stat(filepath.Join(chome, ".claude", "file-history", id, "f1@v1")); err != nil {
			t.Errorf("file history back here: %v", err)
		}
	})

	t.Run("copilot_local_to_w1", func(t *testing.T) {
		cid := "0cb916db-26aa-40f2-86b5-1ba81b225fd2"
		sd := filepath.Join(chome, ".copilot", "session-state", cid)
		mustWrite(t, filepath.Join(sd, "events.jsonl"), fmt.Sprintf(`{"type":"session.start","data":{"sessionId":%q,"copilotVersion":"1.0.0","context":{"cwd":%s,"gitRoot":%s}},"id":"e1"}`+"\n"+
			`{"type":"user.message","data":{"content":"add a flag"},"id":"e2"}`+"\n", cid, jsonQ(repo), jsonQ(repo)))
		mustWrite(t, filepath.Join(sd, "workspace.yaml"), "id: "+cid+"\ncwd: "+repo+"\ngit_root: "+repo+"\nname: flag work\n")
		out := handoff(repo, "--to", "w1", "--agent", "copilot", "--session", cid, "--no-attach", "--name", "cop1")
		short := "0cb916db"
		wantTop := filepath.Join(w1home, "longarm-handoff", "app-"+short)
		if out.Worktree != wantTop || out.Session != "cop1" || strings.Join(out.Resume, " ") != "copilot --resume="+cid {
			t.Fatalf("handoff: %+v", out)
		}
		ws := readText(t, filepath.Join(w1home, ".copilot", "session-state", cid, "workspace.yaml"))
		if !strings.Contains(ws, "cwd: "+jsonQ(wantTop)) || !strings.Contains(ws, "name: flag work") {
			t.Errorf("workspace.yaml on w1:\n%s", ws)
		}
		if ev := readText(t, filepath.Join(w1home, ".copilot", "session-state", cid, "events.jsonl")); !strings.Contains(ev, `"cwd":`+jsonQ(wantTop)) {
			t.Error("events.jsonl context.cwd not rewritten")
		}
		if _, err := os.Stat(filepath.Join(sd, "longarm-handoff.json")); err != nil {
			t.Errorf("source marker: %v", err)
		}
		if rec := agentRecord(t, w1home, cid); rec["cwd"] != wantTop || rec["recordedCwd"] != wantTop || rec["session"] != "cop1" {
			t.Errorf("the agent on w1: %v", rec)
		}
		// w1 -> w2 when w2 can't reach w1: relayed through the client
		h.extra = []string{"LONGARM_TEST_HANDOFF_ENDPOINTS=tcp://127.0.0.1:1"}
		out2, r := h.handoff(repo, "--from", "w1", "--to", "w2", "--session", cid, "--no-attach", "--idle", "1s", "--grace", "5s", "--name", "cop2")
		h.extra = nil
		if out2.Transfer != "relay" || out2.Relayed <= 0 || !strings.Contains(r.stderr, "no direct transfer w1 -> w2") {
			t.Errorf("fallback to the client relay: %+v\n%s", out2, r.stderr)
		}
		if ws := readText(t, filepath.Join(w2home, ".copilot", "session-state", cid, "workspace.yaml")); !strings.Contains(ws, "cwd: "+jsonQ(out2.Worktree)) {
			t.Errorf("workspace.yaml on w2:\n%s", ws)
		}
		e.mustLongarm(e.client, "session", "stop", "w2", "-s", "cop2", "--grace", "0")
	})

	t.Run("refusals", func(t *testing.T) {
		if r := client(repo, "handoff", "--to", "local", "--session", id); r.code != 2 {
			t.Errorf("--from local --to local: %+v", r)
		}
		if r := client(repo, "handoff", "--to", "w1", "--session", "nope-0000-0000"); r.code != 3 {
			t.Errorf("unknown session: %+v", r)
		}
		// the target's agent is older than the one that wrote the session: exit 2, nothing changed
		cid := "1cb916db-26aa-40f2-86b5-1ba81b225fd2"
		sd := filepath.Join(chome, ".copilot", "session-state", cid)
		mustWrite(t, filepath.Join(sd, "events.jsonl"), fmt.Sprintf(`{"type":"session.start","data":{"copilotVersion":"99.0.0","context":{"cwd":%s}},"id":"e1"}`+"\n", jsonQ(repo)))
		if r := client(repo, "handoff", "--to", "w2", "--session", cid, "--no-attach"); r.code != 2 || !strings.Contains(r.stderr, "older than") {
			t.Errorf("an older agent on the target: %+v", r)
		}
		if _, err := os.Stat(filepath.Join(sd, "longarm-handoff.json")); err == nil {
			t.Error("a refused handoff marked the source")
		}

		// a session for the refusals below: each leaves it as it was
		rid := "6e55a7c1-0000-4000-8000-00000000c1a0"
		rProj := filepath.Join(chome, ".claude", "projects", claudeDir(repo))
		mustWrite(t, filepath.Join(rProj, rid+".jsonl"),
			fmt.Sprintf(`{"type":"user","cwd":%s,"sessionId":%q,"version":"1.0.0","message":{"role":"user","content":"x"}}`+"\n", jsonQ(repo), rid))
		untouched := func(what string) {
			t.Helper()
			if _, err := os.Stat(filepath.Join(rProj, rid+".handoff.json")); err == nil {
				t.Errorf("%s: the source was marked handed off", what)
			}
			if m, _ := filepath.Glob(filepath.Join(w2home, ".claude", "projects", "*", rid+"*")); len(m) != 0 {
				t.Errorf("%s: written on w2: %v", what, m)
			}
		}

		// a forged source listing (a compromised source: a name that climbs
		// out of the session's files): refused before anything moves
		h.extra = []string{"LONGARM_TEST_HANDOFF_FORGE=project:" + rid + "/../../../../outside.txt"}
		r := client(repo, "handoff", "--to", "w2", "--agent", "claude", "--session", rid, "--no-attach")
		h.extra = nil
		if r.code != 2 || !strings.Contains(r.stderr, "unsafe file name") {
			t.Errorf("a forged source listing: %+v", r)
		}
		untouched("forged listing")
		if _, err := os.Stat(filepath.Join(w2home, "outside.txt")); err == nil {
			t.Error("a forged name wrote outside the agent's directory")
		}

		// the session is open in an agent here (Claude Code's sessions/<pid>.json
		// names a live claude process): exit 75, nothing changed
		agent := exec.Command(filepath.Join(h.fakebin, exeName("claude")), "--resume", rid)
		agent.Dir = repo
		agent.Env = append(os.Environ(), homeEnv(chome, h.path)...)
		stdin, _ := agent.StdinPipe()
		stdout, _ := agent.StdoutPipe()
		if err := agent.Start(); err != nil {
			t.Fatal(err)
		}
		ready := make(chan bool, 1)
		go func() {
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				if strings.Contains(sc.Text(), "fake agent ready") {
					ready <- true
				}
			}
			close(ready)
		}()
		if !<-ready {
			agent.Process.Kill()
			t.Fatal("the local agent did not start")
		}
		r = client(repo, "handoff", "--to", "w2", "--agent", "claude", "--session", rid, "--no-attach")
		stdin.Close()
		agent.Wait()
		if r.code != 75 || !strings.Contains(r.stderr, "still has session "+rid+" open") {
			t.Errorf("a session open in a local agent: %+v", r)
		}
		untouched("session in use")
		// Copilot CLI: a freshly refreshed open-sessions-state.json entry
		ocid := "3cb916db-26aa-40f2-86b5-1ba81b225fd2"
		mustWrite(t, filepath.Join(chome, ".copilot", "session-state", ocid, "events.jsonl"),
			fmt.Sprintf(`{"type":"session.start","data":{"copilotVersion":"1.0.0","context":{"cwd":%s}},"id":"e1"}`+"\n", jsonQ(repo)))
		mustWrite(t, filepath.Join(chome, ".copilot", "open-sessions-state.json"),
			fmt.Sprintf(`{%q:{"schemaVersion":1,"refreshedAt":%q}}`, ocid, time.Now().UTC().Format(time.RFC3339)))
		r = client(repo, "handoff", "--to", "w2", "--agent", "copilot", "--session", ocid, "--no-attach")
		os.Remove(filepath.Join(chome, ".copilot", "open-sessions-state.json"))
		if r.code != 75 || !strings.Contains(r.stderr, "open-sessions-state.json") {
			t.Errorf("a session open in a local Copilot CLI: %+v", r)
		}
		if _, err := os.Stat(filepath.Join(chome, ".copilot", "session-state", ocid, "longarm-handoff.json")); err == nil {
			t.Error("a refused Copilot handoff marked the source")
		}

		// a symlink (or junction) in the worker's destination tree: refused, nothing written through it
		evil := filepath.Join(root, "evil")
		os.MkdirAll(evil, 0o755)
		os.MkdirAll(filepath.Join(w2home, ".copilot"), 0o755)
		link := filepath.Join(w2home, ".copilot", "session-state")
		os.RemoveAll(link)
		err := os.Symlink(evil, link)
		if err != nil && runtime.GOOS == "windows" {
			err = exec.Command("cmd", "/c", "mklink", "/J", link, evil).Run()
		}
		if err != nil {
			t.Logf("no symlink or junction: %v", err)
			return
		}
		defer os.Remove(link)
		cid2 := "2cb916db-26aa-40f2-86b5-1ba81b225fd2"
		sd2 := filepath.Join(chome, ".copilot", "session-state", cid2)
		mustWrite(t, filepath.Join(sd2, "events.jsonl"), fmt.Sprintf(`{"type":"session.start","data":{"context":{"cwd":%s}},"id":"e1"}`+"\n", jsonQ(repo)))
		if r := client(repo, "handoff", "--to", "w2", "--session", cid2, "--no-attach"); r.code != 2 || !strings.Contains(r.stderr, "symlink") {
			t.Errorf("symlink in the worker's destination: %+v", r)
		}
		if ents, _ := os.ReadDir(evil); len(ents) != 0 {
			t.Error("the handoff wrote through a symlink on the worker")
		}
	})
	if runtime.GOOS == "windows" {
		killShims(e2.dataDir)
	}
}

func jsonQ(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
