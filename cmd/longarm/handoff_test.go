package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/client"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The test binary doubles as a fake agent CLI (copied to claude[.exe] /
// copilot[.exe]): --version and auth status, like the real ones.
func init() {
	if os.Getenv("LONGARM_TEST_FAKE_AGENT") == "" {
		return
	}
	args := os.Args[1:]
	switch {
	case slices.Equal(args, []string{"--version"}):
		fmt.Println(os.Getenv("LONGARM_TEST_FAKE_AGENT") + " (Fake)")
	case len(args) >= 2 && args[0] == "auth" && args[1] == "status":
		fmt.Printf(`{"loggedIn": %s, "configDirectory": %q}`+"\n", os.Getenv("LONGARM_TEST_FAKE_LOGGEDIN"), os.Getenv("CLAUDE_CONFIG_DIR"))
	default:
		os.Exit(3)
	}
	os.Exit(0)
}

func localEP(t *testing.T) *hoEndpoint {
	t.Helper()
	ep, err := openEndpoint(context.Background(), nil, localMachine)
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func TestClaudeProjectDirName(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Users\bolu1`:                         "C--Users-bolu1",
		`C:\Users\bolu1\.claude\jobs\d6f2`:       "C--Users-bolu1--claude-jobs-d6f2",
		"/home/u/src/app":                        "-home-u-src-app",
		"/home/u/worktrees/s.agent1.dmg0_x":      "-home-u-worktrees-s-agent1-dmg0-x",
		`C:\src\teams-media\.claude\worktrees\x`: "C--src-teams-media--claude-worktrees-x",
	} {
		if got, ok := claudeProjectDirName(in); got != want || !ok {
			t.Errorf("claudeProjectDirName(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	if _, ok := claudeProjectDirName("/" + strings.Repeat("a", 200)); ok {
		t.Error("a name over 200 characters must be refused (Claude Code hashes it)")
	}
}

func TestHandoffRewriteJSON(t *testing.T) {
	rw := newPathRewriter([]pathMap{
		{from: `C:\src\app`, to: "/home/w/longarm-handoff/app-1234", fromWin: true},
		{from: `C:\Users\me\.claude\projects\C--src-app`, to: "/home/w/.claude/projects/-home-w-longarm-handoff-app-1234", fromWin: true},
		{from: `C:\Users\me\.claude`, to: "/home/w/.claude", fromWin: true},
	})
	in := strings.Join([]string{
		`{"type":"user","cwd":"C:\\src\\app","sessionId":"s1","n":12345678901234567890,"message":{"role":"user","content":"look at C:\\src\\app\\main.go and C:\\src\\app2\\x <b>"}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","input":{"file_path":"c:\\SRC\\app\\pkg\\a.go","command":"cat C:/src/app/README.md"}}]}}`,
		`{"toolUseResult":{"filePath":"C:\\Users\\me\\.claude\\projects\\C--src-app\\s1\\tool-results\\r.txt"},"cwd":"C:\\src\\app\\sub"}`,
		`{"content":"{\"cwd\":\"C:\\\\src\\\\app\"}","other":"D:\\src\\app","prefix":"xC:\\src\\app"}`,
		`{"partial":"C:\\src\\ap`,
	}, "\n")
	out, n := rw.rewriteJSON([]byte(in))
	lines := strings.Split(string(out), "\n")
	var l0 struct {
		Cwd     string `json:"cwd"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &l0); err != nil {
		t.Fatal(err)
	}
	if l0.Cwd != "/home/w/longarm-handoff/app-1234" {
		t.Errorf("cwd = %q", l0.Cwd)
	}
	if l0.Message.Content != `look at /home/w/longarm-handoff/app-1234\main.go and C:\src\app2\x <b>` {
		t.Errorf("free text = %q (app2 must stay, the rest of a free-text path is not converted)", l0.Message.Content)
	}
	// key order, numbers and untouched strings stay byte for byte
	if !strings.HasPrefix(lines[0], `{"type":"user","cwd":"/home/w/longarm-handoff/app-1234","sessionId":"s1","n":12345678901234567890,`) {
		t.Errorf("line 0 changed beyond the strings: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"file_path":"/home/w/longarm-handoff/app-1234/pkg/a.go"`) {
		t.Errorf("a known path field (case-insensitive on Windows, separators converted): %s", lines[1])
	}
	if !strings.Contains(lines[1], `cat /home/w/longarm-handoff/app-1234/README.md`) {
		t.Errorf("forward-slash spelling in free text: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"filePath":"/home/w/.claude/projects/-home-w-longarm-handoff-app-1234/s1/tool-results/r.txt"`) ||
		!strings.Contains(lines[2], `"cwd":"/home/w/longarm-handoff/app-1234/sub"`) {
		t.Errorf("state dir / sub dir: %s", lines[2])
	}
	if !strings.Contains(lines[3], `{\"cwd\":\"/home/w/longarm-handoff/app-1234\"}`) {
		t.Errorf("JSON text in a string: %s", lines[3])
	}
	if !strings.Contains(lines[3], `"other":"D:\\src\\app"`) || !strings.Contains(lines[3], `"prefix":"xC:\\src\\app"`) {
		t.Errorf("other drives / mid-word must stay: %s", lines[3])
	}
	if lines[4] != `{"partial":"C:\\src\\ap` {
		t.Errorf("a partial line must stay: %s", lines[4])
	}
	if n != 7 {
		t.Errorf("%d values rewritten, want 7", n)
	}

	// Linux -> Windows: separators of known path fields converted
	rw = newPathRewriter([]pathMap{{from: "/home/u/app", to: `C:\Users\w\longarm-handoff\app-1`, toWin: true}})
	out, _ = rw.rewriteJSON([]byte(`{"cwd":"/home/u/app/x/y","text":"see /home/u/app/x","n":"/home/u/apple"}`))
	if string(out) != `{"cwd":"C:\\Users\\w\\longarm-handoff\\app-1\\x\\y","text":"see C:\\Users\\w\\longarm-handoff\\app-1/x","n":"/home/u/apple"}` {
		t.Errorf("linux -> windows: %s", out)
	}
}

func TestHandoffRewriteYAML(t *testing.T) {
	rw := newPathRewriter([]pathMap{{from: "/home/u/app", to: "/srv/h/app-1"}})
	in := "id: x\ncwd: /home/u/app/sub\ngit_root: \"/home/u/app\"\nsummary: |\n  cwd: /home/u/app\nname: n\n"
	out, n := rw.rewriteYAMLPaths([]byte(in))
	want := "id: x\ncwd: \"/srv/h/app-1/sub\"\ngit_root: \"/srv/h/app-1\"\nsummary: |\n  cwd: /home/u/app\nname: n\n"
	if string(out) != want || n != 2 {
		t.Errorf("yaml = %q (%d)", out, n)
	}
}

func TestOlderVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"2.1.300 (Claude Code)", "2.1.310", true},
		{"2.1.310", "2.1.310", false},
		{"2.2.0", "2.1.310", false},
		{"GitHub Copilot CLI 1.0.105.", "1.0.99", false},
		{"", "1.0.0", false},
		{"1.9.9", "1.10.0", true},
	} {
		if got := olderVersion(c.a, c.b); got != c.want {
			t.Errorf("olderVersion(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func writeFileT(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHandoffSnapshot: HEAD + uncommitted + untracked (not ignored) in a
// commit on a temporary ref; the user's index, branch and files untouched;
// the new worktree has the changes uncommitted again.
func TestHandoffSnapshot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "app")
	os.MkdirAll(dir, 0o755)
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "core.autocrlf", "false") // the new worktree is checked out with the repo's config
	writeFileT(t, filepath.Join(dir, "a.txt"), "a1\n")
	writeFileT(t, filepath.Join(dir, "b.txt"), "b1\n")
	writeFileT(t, filepath.Join(dir, "gone.txt"), "x\n")
	writeFileT(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-qm", "c1")
	head := gitT(t, dir, "rev-parse", "HEAD")
	writeFileT(t, filepath.Join(dir, "a.txt"), "a2 unstaged\n")
	writeFileT(t, filepath.Join(dir, "b.txt"), "b2 staged\n")
	gitT(t, dir, "add", "b.txt")
	os.Remove(filepath.Join(dir, "gone.txt"))
	writeFileT(t, filepath.Join(dir, "sub", "new.txt"), "untracked\n")
	writeFileT(t, filepath.Join(dir, "ignored.txt"), "ignored\n")
	idxBefore, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))
	statusBefore := gitT(t, dir, "status", "--porcelain")

	ep := localEP(t)
	r, err := openHoRepo(ctx, ep, filepath.Join(dir, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if r.prefix != "sub/" || !ep.samePath(r.top, dir) && !strings.EqualFold(filepath.Base(r.top), "app") {
		t.Fatalf("repo top %q prefix %q", r.top, r.prefix)
	}
	snap, base, err := r.snapshot(ctx, "refs/longarm/handoff/t1")
	if err != nil {
		t.Fatal(err)
	}
	if base != head || snap == head {
		t.Fatalf("snapshot %s base %s (HEAD %s)", snap, base, head)
	}
	if idxAfter, _ := os.ReadFile(filepath.Join(dir, ".git", "index")); !bytes.Equal(idxBefore, idxAfter) {
		t.Error("the user's index changed")
	}
	if s := gitT(t, dir, "status", "--porcelain"); s != statusBefore {
		t.Errorf("status changed:\n%s\nwas\n%s", s, statusBefore)
	}
	if b := gitT(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); b != "main" || gitT(t, dir, "rev-parse", "HEAD") != head {
		t.Error("the branch moved")
	}
	if got := gitT(t, dir, "rev-parse", "refs/longarm/handoff/t1"); got != snap {
		t.Errorf("ref = %s", got)
	}
	files := strings.Split(gitT(t, dir, "ls-tree", "-r", "--name-only", snap), "\n")
	if !slices.Equal(files, []string{".gitignore", "a.txt", "b.txt", "sub/new.txt"}) {
		t.Errorf("snapshot files %v", files)
	}
	if got := gitT(t, dir, "show", snap+":a.txt"); got != "a2 unstaged" {
		t.Errorf("a.txt in the snapshot: %q", got)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".git", "longarm-handoff-*"))
	if len(matches) != 0 {
		t.Errorf("temporary index left: %v", matches)
	}

	// the target worktree: base + the changes, uncommitted, on handoff/<id>
	wt := filepath.Join(root, "app-handoff-t1")
	br := r.freeBranch(ctx, "t1")
	if err := r.addLocalWorktree(ctx, wt, br, snap, base); err != nil {
		t.Fatal(err)
	}
	if b := gitT(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); b != "handoff/t1" || gitT(t, wt, "rev-parse", "HEAD") != head {
		t.Errorf("worktree on %s at %s", b, gitT(t, wt, "rev-parse", "HEAD"))
	}
	gitT(t, wt, "status") // refreshes the index's stat data (diff does not)
	if got := gitT(t, wt, "diff", "--name-only"); got != "a.txt\nb.txt\ngone.txt" {
		t.Errorf("unstaged changes in the worktree: %q", got)
	}
	if got := gitT(t, wt, "diff", "--cached", "--name-only"); got != "" {
		t.Errorf("staged changes in the worktree: %q", got)
	}
	if got := gitT(t, wt, "ls-files", "--others", "--exclude-standard"); got != "sub/new.txt" {
		t.Errorf("untracked files in the worktree: %q", got)
	}
	if _, err := os.Stat(filepath.Join(wt, "ignored.txt")); err == nil {
		t.Error("an ignored file travelled")
	}
	if r.freeBranch(ctx, "t1") != "handoff/t1-2" {
		t.Error("freeBranch must skip a taken branch")
	}
}

// claudeFixture writes a Claude Code session under cfg for project dir p.
func claudeFixture(t *testing.T, cfg, p, id string, extra ...string) string {
	t.Helper()
	enc, _ := claudeProjectDirName(p)
	lines := []string{
		fmt.Sprintf(`{"type":"user","cwd":%q,"sessionId":%q,"version":"2.1.300","message":{"role":"user","content":"fix the flaky test"}}`, p, id),
		fmt.Sprintf(`{"type":"assistant","cwd":%q,"version":"2.1.310","message":{"role":"assistant","content":[{"type":"text","text":"ok"}]}}`, p),
		`{"type":"ai-title","aiTitle":"Fix flaky test","sessionId":"` + id + `"}`,
	}
	lines = append(lines, extra...)
	tr := filepath.Join(cfg, "projects", enc, id+".jsonl")
	writeFileT(t, tr, strings.Join(lines, "\n")+"\n")
	writeFileT(t, filepath.Join(cfg, "projects", enc, id, "tool-results", "r1.txt"), "result\n")
	writeFileT(t, filepath.Join(cfg, "file-history", id, "abc@v1"), "old content\n")
	writeFileT(t, filepath.Join(cfg, "todos", id+"-agent-"+id+".json"), "[]")
	return tr
}

func TestHandoffDiscovery(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	cfg := filepath.Join(home, "claude-cfg")
	cop := filepath.Join(home, "copilot-home")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("COPILOT_HOME", cop)
	proj := filepath.Join(home, "src", "app")
	claudeFixture(t, cfg, proj, "11111111-2222-3333-4444-555555555555")
	claudeFixture(t, cfg, filepath.Join(home, "other"), "aaaaaaaa-2222-3333-4444-555555555555")
	writeFileT(t, filepath.Join(cfg, "projects", "x", "not-a-session.txt"), "")

	ep := localEP(t)
	ca := claudeAdapter{}
	if got := ca.configDir(ctx, ep); got != cfg {
		t.Fatalf("configDir %q (CLAUDE_CONFIG_DIR)", got)
	}
	list, err := ca.list(ctx, ep, cfg, "", false, nil)
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %d", err, len(list))
	}
	got, err := ca.list(ctx, ep, cfg, "11111111-2222-3333-4444-555555555555", true, nil)
	if err != nil || len(got) != 1 {
		t.Fatalf("list id: %v %d", err, len(got))
	}
	s := got[0]
	if s.ProjectDir != proj || s.Title != "Fix flaky test" || s.Version != "2.1.310" || s.Size == 0 {
		t.Errorf("session %+v", s)
	}
	var rels []string
	for _, f := range s.files {
		rels = append(rels, f.base+":"+f.rel)
	}
	slices.Sort(rels)
	want := []string{"config:file-history/11111111-2222-3333-4444-555555555555/abc@v1",
		"config:todos/11111111-2222-3333-4444-555555555555-agent-11111111-2222-3333-4444-555555555555.json",
		"project:11111111-2222-3333-4444-555555555555.jsonl",
		"project:11111111-2222-3333-4444-555555555555/tool-results/r1.txt"}
	if !slices.Equal(rels, want) {
		t.Errorf("files %v", rels)
	}

	// a marker newer than the transcript: handed off
	mb, _ := json.Marshal(handoffMark{To: "w1", At: time.Now().Add(time.Minute)})
	writeFileT(t, s.marker, string(mb))
	got, _ = ca.list(ctx, ep, cfg, s.ID, false, nil)
	if got[0].HandedOff == nil || got[0].HandedOff.To != "w1" {
		t.Errorf("marker not seen: %+v", got[0])
	}

	// Copilot CLI
	id := "0cb916db-26aa-40f2-86b5-1ba81b225fd2"
	sd := filepath.Join(cop, "session-state", id)
	writeFileT(t, filepath.Join(sd, "events.jsonl"), fmt.Sprintf(`{"type":"session.start","data":{"sessionId":%q,"copilotVersion":"1.0.105","context":{"cwd":%q,"gitRoot":%q}},"id":"e1"}`+"\n"+
		`{"type":"user.message","data":{"content":"add a flag"},"id":"e2"}`+"\n", id, proj, proj))
	writeFileT(t, filepath.Join(sd, "workspace.yaml"), "id: "+id+"\ncwd: "+proj+"\ngit_root: "+proj+"\n")
	writeFileT(t, filepath.Join(sd, "checkpoints", "c1.json"), "{}")
	cl, err := copilotAdapter{}.list(ctx, ep, cop, "", true, nil)
	if err != nil || len(cl) != 1 {
		t.Fatalf("copilot list: %v %d", err, len(cl))
	}
	if c := cl[0]; c.ID != id || c.ProjectDir != proj || c.Title != "add a flag" || c.Version != "1.0.105" || len(c.files) != 3 {
		t.Errorf("copilot session %+v (%d files)", c, len(c.files))
	}
}

func TestHandoffInUse(t *testing.T) {
	ctx := context.Background()
	cfg := t.TempDir()
	ep := localEP(t)
	id := "11111111-2222-3333-4444-555555555555"
	s := &agentSession{ID: id, cfg: cfg}
	self, _ := os.Executable()
	old := agentProcNames
	agentProcNames = append(slices.Clone(old), strings.TrimSuffix(filepath.Base(self), ".exe"))
	defer func() { agentProcNames = old }()
	writeFileT(t, filepath.Join(cfg, "sessions", "1.json"), fmt.Sprintf(`{"pid":%d,"sessionId":"other"}`, os.Getpid()))
	if who := (claudeAdapter{}).inUse(ctx, ep, s); who != "" {
		t.Errorf("another session's process: %q", who)
	}
	writeFileT(t, filepath.Join(cfg, "sessions", fmt.Sprint(os.Getpid())+".json"), fmt.Sprintf(`{"pid":%d,"sessionId":%q}`, os.Getpid(), id))
	if who := (claudeAdapter{}).inUse(ctx, ep, s); !strings.Contains(who, fmt.Sprint(os.Getpid())) {
		t.Errorf("a live process with the session: %q", who)
	}
	agentProcNames = old
	// a pid that is gone is stale
	writeFileT(t, filepath.Join(cfg, "sessions", fmt.Sprint(os.Getpid())+".json"), fmt.Sprintf(`{"pid":%d,"sessionId":%q}`, 999999, id))
	if who := (claudeAdapter{}).inUse(ctx, ep, s); who != "" {
		t.Errorf("a dead pid: %q", who)
	}
	// Copilot CLI: a recently refreshed open-sessions entry
	writeFileT(t, filepath.Join(cfg, "open-sessions-state.json"), fmt.Sprintf(`{%q:{"schemaVersion":1,"refreshedAt":%q}}`, id, time.Now().UTC().Format(time.RFC3339)))
	if who := (copilotAdapter{}).inUse(ctx, ep, s); who == "" {
		t.Error("copilot: a fresh entry is in use")
	}
	writeFileT(t, filepath.Join(cfg, "open-sessions-state.json"), fmt.Sprintf(`{%q:{"schemaVersion":1,"refreshedAt":%q}}`, id, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)))
	if who := (copilotAdapter{}).inUse(ctx, ep, s); who != "" {
		t.Errorf("copilot: a stale entry: %q", who)
	}
}

// fakeAgentBin copies the test binary to dir as name[.exe].
func fakeAgentBin(t *testing.T, dir, name string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	in, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	out.Close()
}

func TestHandoffPreflight(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	srcCfg, dstCfg := filepath.Join(root, "src-claude"), filepath.Join(root, "dst-claude")
	proj := filepath.Join(root, "app")
	os.MkdirAll(proj, 0o755)
	ep := localEP(t)
	sess := &agentSession{ID: "s", cfg: srcCfg, ProjectDir: proj, Version: "2.1.310"}
	repo := &hoRepo{ep: ep, top: proj}

	// not installed: exit 2
	t.Setenv("PATH", bin)
	var w warnings
	err := preflight(ctx, claudeAdapter{}, ep, ep, dstCfg, sess, repo, false, &w)
	var ce *codedError
	if !errors.As(err, &ce) || ce.code != exitUsage || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing agent: %v", err)
	}

	fakeAgentBin(t, bin, "claude")
	t.Setenv("LONGARM_TEST_FAKE_AGENT", "2.1.300")
	t.Setenv("LONGARM_TEST_FAKE_LOGGEDIN", "false")
	if err := preflight(ctx, claudeAdapter{}, ep, ep, dstCfg, sess, repo, false, &w); !errors.As(err, &ce) || ce.code != exitUsage || !strings.Contains(err.Error(), "older") {
		t.Fatalf("older agent: %v", err)
	}
	// --force: a warning; not logged in and missing MCP servers: warnings
	writeFileT(t, filepath.Join(srcCfg, ".claude.json"), fmt.Sprintf(`{"mcpServers":{"github":{"env":{"TOKEN":"secret-value"}},"jira":{}},"projects":{%q:{"mcpServers":{"local-db":{}}}}}`, proj))
	writeFileT(t, filepath.Join(dstCfg, ".claude.json"), `{"mcpServers":{"github":{}}}`)
	writeFileT(t, filepath.Join(proj, ".mcp.json"), `{"mcpServers":{"jira":{}}}`)
	w = nil
	if err := preflight(ctx, claudeAdapter{}, ep, ep, dstCfg, sess, repo, true, &w); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(w, "\n")
	for _, want := range []string{"older than the session's 2.1.310", "does not look logged in", "MCP servers configured on local but not on local: local-db"} {
		if !strings.Contains(all, want) {
			t.Errorf("warnings lack %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "secret-value") {
		t.Error("an MCP server's configuration leaked into a warning")
	}
	t.Setenv("LONGARM_TEST_FAKE_AGENT", "2.1.311")
	t.Setenv("LONGARM_TEST_FAKE_LOGGEDIN", "true")
	writeFileT(t, filepath.Join(dstCfg, ".claude.json"), `{"mcpServers":{"github":{},"local-db":{}}}`)
	w = nil
	if err := preflight(ctx, claudeAdapter{}, ep, ep, dstCfg, sess, repo, false, &w); err != nil || len(w) != 0 {
		t.Errorf("all good: %v %v", err, w)
	}
}

// TestOwnMachine: handoff refuses a worker of another user - by what the
// worker says or what the hub lists - whoever the caller is; ownerless
// workers and the local machine pass.
func TestOwnMachine(t *testing.T) {
	w := func(owner, caller string) *hoEndpoint {
		return &hoEndpoint{name: "w", conn: &client.Conn{}, owner: owner, callerUser: caller}
	}
	for _, c := range []struct {
		ep   *hoEndpoint
		node *pb.NodeInfo
		ok   bool
	}{
		{&hoEndpoint{name: localMachine}, nil, true},
		// a fleet worker, or no hub
		{w("", "alice"), nil, true},
		{w("alice", "alice"), &pb.NodeInfo{Owner: "alice"}, true},
		// shared with bob or not
		{w("alice", "bob"), nil, false},
		// an older worker: the hub knows
		{w("", "bob"), &pb.NodeInfo{Owner: "alice"}, false},
		{w("alice", "root"), &pb.NodeInfo{Owner: "alice"}, false},
		// owner unknown yet
		{w("?", "alice"), nil, false},
	} {
		err := ownMachine(c.ep, c.node)
		if c.ok != (err == nil) {
			t.Errorf("owner %q caller %q node %v: %v", c.ep.owner, c.ep.callerUser, c.node, err)
		}
		var ce *codedError
		if err != nil && (!errors.As(err, &ce) || ce.code != exitNoPerm) {
			t.Errorf("not a permission error: %v", err)
		}
	}
}
