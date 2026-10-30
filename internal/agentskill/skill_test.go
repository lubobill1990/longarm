package agentskill

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func testEnv(t *testing.T, vars map[string]string) (Env, string) {
	t.Helper()
	home := t.TempDir()
	return Env{
		Home:     home,
		Getenv:   func(k string) string { return vars[k] },
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	}, home
}

func setBundle(t *testing.T, ver string, files map[string]string) {
	t.Helper()
	m := fstest.MapFS{}
	for n, c := range files {
		m[n] = &fstest.MapFile{Data: []byte(c)}
	}
	oldB, oldV := Bundle, BundledVersion
	Bundle, BundledVersion = m, func() string { return ver }
	t.Cleanup(func() { Bundle, BundledVersion = oldB, oldV })
}

var v1 = map[string]string{"SKILL.md": "---\nname: longarm\n---\nv1\n", "reference/a.md": "a1", "reference/old.md": "old"}
var v2 = map[string]string{"SKILL.md": "---\nname: longarm\n---\nv2\n", "reference/a.md": "a2", "reference/b.md": "b2"}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestInstallUpdateRemove(t *testing.T) {
	env, home := testEnv(t, nil)
	setBundle(t, "v1.0.0", v1)
	dir := filepath.Join(home, ".claude", "skills", "longarm")

	st, err := env.Status("claude")
	if err != nil || st.State != Absent || st.Detected || st.Dir != dir {
		t.Fatalf("before: %+v %v", st, err)
	}
	if exists(filepath.Join(home, ".claude")) {
		t.Fatal("status wrote to the agent's directory")
	}

	st, err = env.Install("claude")
	if err != nil || st.State != Current || st.Installed != "v1.0.0" {
		t.Fatalf("install: %+v %v", st, err)
	}
	if read(t, filepath.Join(dir, "SKILL.md")) != v1["SKILL.md"] || read(t, filepath.Join(dir, "reference", "old.md")) != "old" {
		t.Fatal("files not installed")
	}
	var m manifest
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dir, ManifestName))), &m); err != nil || m.Version != "v1.0.0" || len(m.Files) != 3 {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	// installing again is a no-op
	if st, err = env.Install("claude"); err != nil || st.State != Current {
		t.Fatalf("again: %+v %v", st, err)
	}

	// a newer Longarm: outdated, and the update replaces the files and
	// drops the one the new version no longer has
	setBundle(t, "v1.1.0", v2)
	if st, _ = env.Status("claude"); st.State != Outdated || st.Installed != "v1.0.0" || st.Bundled != "v1.1.0" {
		t.Fatalf("outdated: %+v", st)
	}
	if st, err = env.Install("claude"); err != nil || st.State != Current || st.Installed != "v1.1.0" {
		t.Fatalf("update: %+v %v", st, err)
	}
	if read(t, filepath.Join(dir, "reference", "b.md")) != "b2" || exists(filepath.Join(dir, "reference", "old.md")) {
		t.Fatal("update did not replace the files")
	}

	// a file the user added stays, and so does the folder
	if err := os.WriteFile(filepath.Join(dir, "reference", "mine.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = env.Remove("claude")
	if err != nil || st.State != Foreign || !strings.Contains(st.Note, "reference/mine.md") {
		t.Fatalf("remove with an extra file: %+v %v", st, err)
	}
	if read(t, filepath.Join(dir, "reference", "mine.md")) != "mine" || exists(filepath.Join(dir, "SKILL.md")) || exists(filepath.Join(dir, ManifestName)) {
		t.Fatal("remove deleted the wrong files")
	}
	os.Remove(filepath.Join(dir, "reference", "mine.md"))
	os.Remove(filepath.Join(dir, "reference"))
	os.Remove(dir)

	// a clean remove leaves nothing, not even the folder
	if _, err := env.Install("claude"); err != nil {
		t.Fatal(err)
	}
	if st, err = env.Remove("claude"); err != nil || st.State != Absent || exists(dir) {
		t.Fatalf("remove: %+v %v exists=%v", st, err, exists(dir))
	}
	if !exists(filepath.Join(home, ".claude", "skills")) {
		t.Fatal("remove deleted the skills directory")
	}
	// removing what isn't there is fine
	if st, err = env.Remove("claude"); err != nil || st.State != Absent {
		t.Fatalf("remove again: %+v %v", st, err)
	}
}

func TestUserModifiedIsLeftAlone(t *testing.T) {
	env, home := testEnv(t, nil)
	setBundle(t, "v1.0.0", v1)
	dir := filepath.Join(home, ".copilot", "skills", "longarm")
	if _, err := env.Install("copilot"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("my notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "reference", "old.md")) // deleted counts as changed

	st, _ := env.Status("copilot")
	if st.State != Modified || !slices.Equal(st.Modified, []string{"SKILL.md", "reference/old.md"}) {
		t.Fatalf("status: %+v", st)
	}
	var re *RefusedError
	if _, err := env.Remove("copilot"); !errors.As(err, &re) || !slices.Contains(re.Files, "SKILL.md") {
		t.Fatalf("remove: %v", err)
	}
	setBundle(t, "v1.1.0", v2)
	if _, err := env.Install("copilot"); !errors.As(err, &re) {
		t.Fatalf("update: %v", err)
	}
	if up, err := env.Refresh(); err != nil || len(up) != 0 {
		t.Fatalf("refresh: %+v %v", up, err)
	}
	if read(t, filepath.Join(dir, "SKILL.md")) != "my notes" || read(t, filepath.Join(dir, "reference", "a.md")) != "a1" {
		t.Fatal("a modified copy was changed")
	}
}

func TestBundledFileAddedByUserIsAConflict(t *testing.T) {
	env, home := testEnv(t, nil)
	setBundle(t, "v1.0.0", v1)
	dir := filepath.Join(home, ".agents", "skills", "longarm")
	if _, err := env.Install("codex"); err != nil {
		t.Fatal(err)
	}
	// the user wrote reference/b.md, which the next version ships
	os.WriteFile(filepath.Join(dir, "reference", "b.md"), []byte("user's"), 0o644)
	setBundle(t, "v1.1.0", v2)
	var re *RefusedError
	if _, err := env.Install("codex"); !errors.As(err, &re) || !slices.Contains(re.Files, "reference/b.md") {
		t.Fatalf("install over a user file: %v", err)
	}
	if read(t, filepath.Join(dir, "reference", "b.md")) != "user's" {
		t.Fatal("user file overwritten")
	}
}

func TestForeignSkillIsLeftAlone(t *testing.T) {
	env, home := testEnv(t, nil)
	setBundle(t, "v1.0.0", v1)
	dir := filepath.Join(home, ".claude", "skills", "longarm")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("someone else's"), 0o644)

	st, _ := env.Status("claude")
	if st.State != Foreign {
		t.Fatalf("status: %+v", st)
	}
	var re *RefusedError
	if _, err := env.Install("claude"); !errors.As(err, &re) {
		t.Fatalf("install: %v", err)
	}
	if _, err := env.Remove("claude"); !errors.As(err, &re) {
		t.Fatalf("remove: %v", err)
	}
	if read(t, filepath.Join(dir, "SKILL.md")) != "someone else's" {
		t.Fatal("foreign skill changed")
	}
	// an empty folder is no skill: installing into it is fine
	os.Remove(filepath.Join(dir, "SKILL.md"))
	if st, err := env.Install("claude"); err != nil || st.State != Current {
		t.Fatalf("install into an empty folder: %+v %v", st, err)
	}
}

func TestManifestCannotPointOutside(t *testing.T) {
	env, home := testEnv(t, nil)
	setBundle(t, "v1.0.0", v1)
	victim := filepath.Join(home, "victim.txt")
	os.WriteFile(victim, []byte("keep"), 0o644)
	dir := filepath.Join(home, ".claude", "skills", "longarm")
	if _, err := env.Install("claude"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../../../victim.txt", "/etc/passwd", `..\..\..\victim.txt`, "C:/victim.txt"} {
		m := manifest{Schema: 1, Skill: SkillName, Version: "v1.0.0", Files: map[string]string{bad: sum([]byte("keep"))}}
		b, _ := json.Marshal(m)
		os.WriteFile(filepath.Join(dir, ManifestName), b, 0o644)
		st, _ := env.Status("claude")
		if st.State != Foreign {
			t.Fatalf("%s: %+v", bad, st)
		}
		if _, err := env.Remove("claude"); err == nil {
			t.Fatalf("%s: remove went ahead", bad)
		}
		if read(t, victim) != "keep" {
			t.Fatalf("%s: file outside the skill deleted", bad)
		}
	}
}

func TestSymlinkedSkillDirIsRefused(t *testing.T) {
	env, home := testEnv(t, nil)
	setBundle(t, "v1.0.0", v1)
	elsewhere := filepath.Join(home, "elsewhere")
	os.MkdirAll(elsewhere, 0o755)
	skillsDir := filepath.Join(home, ".claude", "skills")
	os.MkdirAll(skillsDir, 0o755)
	link := filepath.Join(skillsDir, "longarm")
	if err := os.Symlink(elsewhere, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Skip("no symlinks here:", err)
		}
		// a junction needs no privilege
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, elsewhere).CombinedOutput(); err != nil {
			t.Skip("no junctions here:", err, string(out))
		}
	}
	var re *RefusedError
	if _, err := env.Install("claude"); !errors.As(err, &re) {
		t.Fatalf("install through a symlink: %v", err)
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Fatal("wrote through the symlink")
	}
}

func TestSymlinkedFileIsNotWrittenThrough(t *testing.T) {
	env, home := testEnv(t, nil)
	setBundle(t, "v1.0.0", v1)
	target := filepath.Join(home, "target.txt")
	os.WriteFile(target, []byte("keep"), 0o644)
	dir := filepath.Join(home, ".claude", "skills", "longarm")
	os.MkdirAll(dir, 0o755)
	if err := os.Symlink(target, filepath.Join(dir, "SKILL.md")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	if _, err := env.Install("claude"); err == nil {
		t.Fatal("installed over a symlink")
	}
	if read(t, target) != "keep" {
		t.Fatal("wrote through the symlink")
	}
}

func TestUnknownAgent(t *testing.T) {
	env, _ := testEnv(t, nil)
	for _, f := range []func(string) (Status, error){env.Status, env.Install, env.Remove} {
		if _, err := f("../../etc"); !errors.Is(err, ErrUnknownAgent) {
			t.Fatalf("got %v", err)
		}
	}
}

func TestEnvOverridesAndDetection(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "claude-cfg")
	env, home := testEnv(t, map[string]string{"CLAUDE_CONFIG_DIR": cfg, "COPILOT_HOME": "relative/dir"})
	env.LookPath = func(b string) (string, error) {
		if b == "codex" {
			return "/usr/bin/codex", nil
		}
		return "", errors.New("no")
	}
	setBundle(t, "v1.0.0", v1)
	os.MkdirAll(filepath.Join(home, ".gemini"), 0o755)
	all, err := env.StatusAll()
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Status{}
	for _, s := range all {
		by[s.Agent] = s
	}
	if by["claude"].Dir != filepath.Join(cfg, "skills", "longarm") || by["claude"].Detected {
		t.Fatalf("claude: %+v", by["claude"])
	}
	if by["copilot"].Dir != "" || by["copilot"].Note == "" {
		t.Fatalf("copilot with a relative COPILOT_HOME: %+v", by["copilot"])
	}
	if !by["codex"].Detected || by["codex"].DetectedBy != "codex on PATH" || by["codex"].Dir != filepath.Join(home, ".agents", "skills", "longarm") {
		t.Fatalf("codex: %+v", by["codex"])
	}
	if !by["gemini"].Detected {
		t.Fatalf("gemini: %+v", by["gemini"])
	}
}

func TestRefreshUpdatesOnlyLongarmsUnmodifiedCopies(t *testing.T) {
	env, home := testEnv(t, nil)
	setBundle(t, "v1.0.0", v1)
	env.Install("claude")
	env.Install("copilot")
	os.WriteFile(filepath.Join(home, ".copilot", "skills", "longarm", "SKILL.md"), []byte("mine"), 0o644)
	setBundle(t, "v1.1.0", v2)
	up, err := env.Refresh()
	if err != nil || len(up) != 1 || up[0].Agent != "claude" || up[0].State != Current {
		t.Fatalf("refresh: %+v %v", up, err)
	}
	if exists(filepath.Join(home, ".agents", "skills", "longarm")) {
		t.Fatal("refresh installed a new copy")
	}
}

func TestEmbeddedBundle(t *testing.T) {
	b, err := loadBundle()
	if err != nil {
		t.Fatal(err)
	}
	skill := string(b.files["SKILL.md"])
	if !strings.HasPrefix(skill, "---\nname: longarm\ndescription: ") {
		t.Fatalf("SKILL.md frontmatter: %.80q", skill)
	}
	// every reference the SKILL.md links to is bundled
	for _, part := range strings.Split(skill, "](")[1:] {
		link, _, _ := strings.Cut(part, ")")
		if strings.HasPrefix(link, "reference/") {
			if _, ok := b.files[link]; !ok {
				t.Errorf("SKILL.md links to %s, which is not bundled", link)
			}
		}
	}
	if lines := strings.Count(skill, "\n"); lines > 150 {
		t.Errorf("SKILL.md has %d lines; keep it under 150 (details go to reference/)", lines)
	}
}
