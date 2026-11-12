package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"longarm/internal/agentskill"
	"longarm/internal/version"
)

func skillCLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	var validated bool
	markUsageErrors(root, &validated)
	root.SetArgs(append([]string{"skill"}, args...))
	err := root.Execute()
	if err != nil {
		out.WriteString(err.Error())
	}
	return out.String(), exitCodeFor(err, validated)
}

// TestRefreshSkillsOnce: the first command of a release refreshes the skill
// copies, once per version (written to the config dir); local builds,
// completion and skill's own commands never.
func TestRefreshSkillsOnce(t *testing.T) {
	calls := 0
	oldFn, oldVer, oldDir := refreshSkillsFn, version.Version, configDir
	refreshSkillsFn = func() { calls++ }
	t.Cleanup(func() { refreshSkillsFn, version.Version, configDir = oldFn, oldVer, oldDir })
	root := newRootCmd()            // (its --config-dir default sets configDir: set it after)
	root.InitDefaultCompletionCmd() // (Execute adds it)
	configDir = t.TempDir()
	cmd := func(path ...string) *cobra.Command {
		c, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	version.Version = "dev"
	refreshSkillsOnce(cmd("ls"))
	version.Version = "1.2.3"
	refreshSkillsOnce(cmd("skill", "status"))
	refreshSkillsOnce(cmd("completion"))
	if calls != 0 {
		t.Fatalf("%d refreshes for dev, skill and completion", calls)
	}
	refreshSkillsOnce(cmd("ls"))
	refreshSkillsOnce(cmd("jobs"))
	if calls != 1 {
		t.Fatalf("%d refreshes of 1.2.3, want 1", calls)
	}
	version.Version = "1.2.4"
	refreshSkillsOnce(cmd("ls"))
	if calls != 2 {
		t.Fatalf("%d refreshes after an update, want 2", calls)
	}
	// no config dir: refreshed, the dir not created
	configDir = filepath.Join(t.TempDir(), "none")
	refreshSkillsOnce(cmd("ls"))
	if _, err := os.Stat(configDir); calls != 3 || err == nil {
		t.Fatalf("no config dir: %d calls, %v", calls, err)
	}
}

func TestSkillCLI(t *testing.T) {
	home := t.TempDir()
	old := skillEnv
	skillEnv = func() agentskill.Env {
		return agentskill.Env{Home: home, Getenv: func(string) string { return "" },
			LookPath: func(b string) (string, error) {
				if b == "claude" {
					return "/bin/claude", nil
				}
				return "", errors.New("no")
			}}
	}
	t.Cleanup(func() { skillEnv = old })

	out, code := skillCLI(t, "status", "--json")
	if code != 0 || !strings.Contains(out, `"state": "absent"`) {
		t.Fatalf("status: %d %s", code, out)
	}
	// install: the detected agents only
	if out, code = skillCLI(t, "install"); code != 0 || !strings.Contains(out, "Claude Code: installed") {
		t.Fatalf("install: %d %s", code, out)
	}
	skill := filepath.Join(home, ".claude", "skills", "longarm", "SKILL.md")
	if _, err := os.Stat(skill); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".copilot")); err == nil {
		t.Fatal("installed for an agent that is not here")
	}
	if out, code = skillCLI(t, "install", "--agent", "nope"); code != exitUsage || !strings.Contains(out, "unknown agent") {
		t.Fatalf("unknown agent: %d %s", code, out)
	}
	// a changed copy: remove refuses (exit 2) and names the file
	os.WriteFile(skill, []byte("mine"), 0o644)
	if out, code = skillCLI(t, "remove", "--agent", "claude"); code != exitUsage || !strings.Contains(out, "SKILL.md") {
		t.Fatalf("remove modified: %d %s", code, out)
	}
	out, _ = skillCLI(t, "status", "--json", "--agent", "claude")
	var st struct{ Agents []agentskill.Status }
	if err := json.Unmarshal([]byte(out), &st); err != nil || len(st.Agents) != 1 || st.Agents[0].State != agentskill.Modified {
		t.Fatalf("status after a change: %s", out)
	}
}
