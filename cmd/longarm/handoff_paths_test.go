package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckRel(t *testing.T) {
	bad := []string{"", "../x", `..\x`, `C:\x`, "C:x", "/etc/x", "a/../../x", "a//b", "./a", "a/.", "a\x00b", "a\nb",
		`a\b`, "//server/share/x"}
	badWin := append(bad, "CON", "con.txt", "a/NUL", "COM1", "lpt9.log", "a:b", "x.jsonl:stream", "a/b.", "a/b ", "a<b", "AUX ")
	for _, r := range badWin {
		if checkRel(r, true) == nil {
			t.Errorf("checkRel(%q, windows) accepted", r)
		}
	}
	for _, r := range bad {
		if checkRel(r, false) == nil {
			t.Errorf("checkRel(%q, unix) accepted", r)
		}
	}
	for _, r := range []string{"a.jsonl", "id/subagents/agent-1.jsonl", "file-history/id/abc@v1", "tool-results/r 1.txt", "console.log", "CONFIG"} {
		if err := checkRel(r, true); err != nil {
			t.Errorf("checkRel(%q): %v", r, err)
		}
	}
	if checkRel("ab:c", false) != nil {
		t.Error(`":" is a plain character on Unix targets`)
	}
}

func isUsage(err error) bool {
	var ce *codedError
	return errors.As(err, &ce) && ce.code == exitUsage
}

// TestCheckSessionFilesForged: a source listing names outside the
// session's own files are refused for Windows and Unix targets.
func TestCheckSessionFilesForged(t *testing.T) {
	id := "11111111-2222-3333-4444-555555555555"
	for _, goos := range []string{"windows", "linux"} {
		dst := &hoEndpoint{name: "w2", goos: goos, conn: nil}
		cfg, proj := "/home/u/.claude", "/home/u/.claude/projects/-home-u-x"
		if goos == "windows" {
			cfg, proj = `C:\Users\u\.claude`, `C:\Users\u\.claude\projects\C--x`
		}
		base := func(b string) string {
			if b == "project" {
				return proj
			}
			return cfg
		}
		good := []agentFile{{base: "project", rel: id + ".jsonl"}, {base: "project", rel: id + "/subagents/a.jsonl"},
			{base: "config", rel: "file-history/" + id + "/x@v1"}, {base: "config", rel: "todos/" + id + "-agent-x.json"}}
		if err := checkSessionFiles(claudeAdapter{}, dst, &agentSession{ID: id, files: good}, base); err != nil {
			t.Fatalf("%s: legitimate files refused: %v", goos, err)
		}
		forged := []agentFile{
			{base: "project", rel: "../x"}, {base: "project", rel: `..\x`}, {base: "project", rel: `C:\x`},
			{base: "project", rel: "/etc/x"}, {base: "project", rel: id + "/../../x"}, {base: "project", rel: "a/../../x"},
			{base: "project", rel: id + "/CON"}, {base: "project", rel: id + "/a:b"}, {base: "project", rel: "other.jsonl"},
			{base: "config", rel: "settings.json"}, {base: "config", rel: "todos/other-x.json"},
			{base: "config", rel: "file-history/" + id + `/..\..\..\AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Startup\x.bat`},
		}
		for _, f := range forged {
			if goos == "linux" && (strings.HasSuffix(f.rel, "/CON") || strings.HasSuffix(f.rel, "/a:b")) {
				continue // plain names on Unix
			}
			files := append(append([]agentFile(nil), good...), f)
			if err := checkSessionFiles(claudeAdapter{}, dst, &agentSession{ID: id, files: files}, base); !isUsage(err) {
				t.Errorf("%s: forged %s:%q: %v", goos, f.base, f.rel, err)
			}
		}
		many := make([]agentFile, maxHandoffFiles+1)
		for i := range many {
			many[i] = agentFile{base: "project", rel: id + "/f" + strings.Repeat("x", i%7)}
		}
		if err := checkSessionNames(claudeAdapter{}, dst, &agentSession{ID: id, files: many}); !isUsage(err) {
			t.Errorf("%s: too many files: %v", goos, err)
		}
	}
	// Copilot: only session-state/<id>/...
	cop := []agentFile{{base: "config", rel: "session-state/" + id + "/../" + "x"}}
	if err := checkSessionNames(copilotAdapter{}, &hoEndpoint{goos: "linux"}, &agentSession{ID: id, files: cop}); !isUsage(err) {
		t.Errorf("copilot forged: %v", err)
	}
	if err := checkSessionNames(copilotAdapter{}, &hoEndpoint{goos: "linux"}, &agentSession{ID: id,
		files: []agentFile{{base: "config", rel: "session-state/" + id + "/checkpoints/c1.json"}}}); err != nil {
		t.Errorf("copilot legitimate: %v", err)
	}
}

// TestHandoffMoveRefusesForged moves a session between two local agent
// directories: a forged name is refused before anything is written, a
// symlink in the destination tree is refused, legitimate nested files
// arrive, and the source's size is enforced while copying.
func TestHandoffMoveRefusesForged(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	srcCfg, dstCfg := filepath.Join(root, "src"), filepath.Join(root, "dst")
	proj, dstTop := filepath.Join(root, "app"), filepath.Join(root, "app2")
	id := "11111111-2222-3333-4444-555555555555"
	tr := claudeFixture(t, srcCfg, proj, id)
	writeFileT(t, filepath.Join(filepath.Dir(tr), id, "subagents", "deep", "a.jsonl"), `{"cwd":`+string(jsonString(proj))+`}`+"\n")
	ep := localEP(t)
	ca := claudeAdapter{}
	list := func() *agentSession {
		got, err := ca.list(ctx, ep, srcCfg, id, true, nil)
		if err != nil || len(got) != 1 {
			t.Fatalf("list: %v %d", err, len(got))
		}
		return got[0]
	}
	repo := &hoRepo{ep: ep, top: proj}
	outside := filepath.Join(root, "outside.txt")

	// forged: a name that climbs out of the session's files
	s := list()
	s.files = append(s.files, agentFile{src: tr, base: "project", rel: id + "/../../../../outside.txt", size: 1, rewrite: ""})
	if _, _, err := moveSessionFiles(ctx, ca, ep, ep, dstCfg, s, repo, dstTop, dstTop, 1<<30); !isUsage(err) {
		t.Fatalf("forged name: %v", err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a forged name wrote outside the destination")
	}
	if _, err := os.Stat(dstCfg); err == nil {
		t.Fatal("something was written before the names were checked")
	}

	// a source that sends more than it listed
	s = list()
	for i := range s.files {
		s.files[i].size = 1
	}
	if _, _, err := moveSessionFiles(ctx, ca, ep, ep, dstCfg, s, repo, dstTop, dstTop, 1<<30); !isUsage(err) || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("oversized file: %v", err)
	}
	os.RemoveAll(dstCfg)

	// a symlink in the destination tree
	evil := filepath.Join(root, "evil")
	os.MkdirAll(evil, 0o755)
	os.MkdirAll(dstCfg, 0o755)
	err := os.Symlink(evil, filepath.Join(dstCfg, "file-history"))
	if err != nil && runtime.GOOS == "windows" { // no symlink privilege: a junction (no privilege needed)
		err = exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(dstCfg, "file-history"), evil).Run()
	}
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Logf("no symlink privilege (%v): symlink case covered on Unix", err)
		} else {
			t.Fatal(err)
		}
	} else {
		if _, _, err := moveSessionFiles(ctx, ca, ep, ep, dstCfg, list(), repo, dstTop, dstTop, 1<<30); !isUsage(err) || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink in the destination: %v", err)
		}
		if ents, _ := os.ReadDir(evil); len(ents) != 0 {
			t.Fatal("wrote through a symlink")
		}
		os.RemoveAll(dstCfg)
	}

	// legitimate: nested files arrive, rewritten
	n, _, err := moveSessionFiles(ctx, ca, ep, ep, dstCfg, list(), repo, dstTop, dstTop, 1<<30)
	if err != nil || n != 5 {
		t.Fatalf("legitimate move: %d %v", n, err)
	}
	enc, _ := claudeProjectDirName(dstTop)
	b, err := os.ReadFile(filepath.Join(dstCfg, "projects", enc, id, "subagents", "deep", "a.jsonl"))
	if err != nil || !strings.Contains(string(b), string(jsonString(dstTop))) {
		t.Fatalf("nested file: %v %s", err, b)
	}
	// moving again: what is there is backed up, never overwritten
	if _, _, err := moveSessionFiles(ctx, ca, ep, ep, dstCfg, list(), repo, dstTop, dstTop, 1<<30); err != nil {
		t.Fatalf("second move: %v", err)
	}
	if bk, _ := filepath.Glob(filepath.Join(dstCfg, "longarm-handoff-backup", id+"-*", "project", id+".jsonl")); len(bk) != 1 {
		t.Errorf("backup: %v", bk)
	}
}

func TestCopyLocalFileExclusive(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(src, []byte("12345"), 0o600)
	os.WriteFile(dst, []byte("old"), 0o600)
	if _, err := copyLocalFile(src, dst, 0o600, 100); err == nil {
		t.Error("an existing destination was overwritten")
	}
	os.Remove(dst)
	if _, err := copyLocalFile(src, dst, 0o600, 4); !isUsage(err) {
		t.Errorf("over the limit: %v", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a partial copy was left")
	}
	if n, err := copyLocalFile(src, dst, 0o600, 5); err != nil || n != 5 {
		t.Errorf("copy: %d %v", n, err)
	}
}
