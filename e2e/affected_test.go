package e2e

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAffectedScript: .github/scripts/affected.sh (release.yml "affected")
// in a small module laid out like this one: the tray's pages and tests
// don't change the hub, a package the hub builds from does; the workers
// change with what a rollout installs, not with hub-only code; an unknown
// version counts as changed.
func TestAffectedScript(t *testing.T) {
	t.Parallel()
	bash := findBash(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	script, _ := filepath.Abs("../.github/scripts/affected.sh")
	dir := t.TempDir()
	write := func(rel, s string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write("go.mod", "module longarm\n\ngo 1.22\n")
	write("internal/hubonly/h.go", "package hubonly\n\nconst X = 1\n")
	write("internal/shared/s.go", "package shared\n\nconst Y = 1\n")
	write("internal/workeronly/w.go", "package workeronly\n\nconst Z = 1\n")
	for _, c := range []string{"longarm-hub", "longarm-relay"} {
		write("cmd/"+c+"/main.go", "package main\n\nimport (\n\t_ \"longarm/internal/hubonly\"\n\t_ \"longarm/internal/shared\"\n)\n\nfunc main() {}\n")
	}
	for _, c := range []string{"longarmd", "longarm", "longarm-tray", "longarm-update", "longarm-msihelper"} {
		write("cmd/"+c+"/main.go", "package main\n\nimport (\n\t_ \"longarm/internal/shared\"\n\t_ \"longarm/internal/workeronly\"\n)\n\nfunc main() {}\n")
	}
	write("web/src/tray/a.tsx", "a\n")
	write("web/src/panel/p.tsx", "p\n")
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("tag", "v1.0.0")
	commit := func(rel, s string) {
		t.Helper()
		write(rel, s)
		git("add", "-A")
		git("commit", "-q", "-m", rel)
	}
	changed := func(comp, from string) bool {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bash, bashPath(script), "changed", comp, from, "HEAD")
		cmd.Dir = dir
		var errb bytes.Buffer
		cmd.Stderr = &errb
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return false
		} else if err != nil {
			t.Fatalf("affected.sh changed %s %s: %v\n%s", comp, from, err, errb.String())
		}
		return true
	}
	// the tray's pages and a test file: workers only
	commit("web/src/tray/a.tsx", "a2\n")
	commit("internal/hubonly/h_test.go", "package hubonly\n")
	if changed("hub", "v1.0.0") || !changed("workers", "v1.0.0") {
		t.Fatalf("tray change: hub %v, workers %v", changed("hub", "v1.0.0"), changed("workers", "v1.0.0"))
	}
	git("tag", "v1.0.1")
	// hub-only code: the hub only
	commit("internal/hubonly/h.go", "package hubonly\n\nconst X = 2\n")
	if !changed("hub", "v1.0.1") || changed("workers", "v1.0.1") {
		t.Fatalf("hub change: hub %v, workers %v", changed("hub", "v1.0.1"), changed("workers", "v1.0.1"))
	}
	git("tag", "v1.0.2")
	// shared code and the panel's pages: both / the hub
	commit("internal/shared/s.go", "package shared\n\nconst Y = 2\n")
	if !changed("hub", "v1.0.2") || !changed("workers", "v1.0.2") {
		t.Fatal("shared change: not both")
	}
	git("tag", "v1.0.3")
	commit("web/src/panel/p.tsx", "p2\n")
	if !changed("hub", "v1.0.3") {
		t.Fatal("panel change: hub unchanged")
	}
	// a version this repository doesn't have: changed
	if !changed("hub", "v9.9.9") {
		t.Fatal("unknown version: unchanged")
	}
	if !strings.Contains(string(must(os.ReadFile(script))), "go list -deps") {
		t.Fatal("affected.sh no longer derives the packages from the build")
	}
}
