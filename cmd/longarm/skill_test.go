package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/agentskill"
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
