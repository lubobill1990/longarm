package server

import (
	"testing"

	"github.com/lubobill1990/longarm/internal/jobs"
)

func TestSessionSpecDiff(t *testing.T) {
	have := jobs.Spec{Argv: []string{"claude"}, Cwd: "sub", Env: map[string]string{"A": "1"}, SecretEnvNames: []string{"GH_TOKEN"},
		Workspace: &jobs.Workspace{Repo: "r", Commit: "c1", Ref: "HEAD", StagingID: "S1"}}
	same := sessionReq{cwd: "sub", env: map[string]string{"A": "1"}, secrets: map[string]string{"GH_TOKEN": "x"},
		ws: &jobs.Workspace{Repo: "r", Commit: "c1", Ref: "HEAD", StagingID: "S2"}}
	argv := []string{"claude"}
	if d := sessionSpecDiff(have, argv, same); d != "" {
		t.Fatalf("same spec (another staging id) differs in %s", d)
	}
	for want, r := range map[string]sessionReq{
		"cwd":              {cwd: "x", env: same.env, secrets: same.secrets, ws: same.ws},
		"env":              {cwd: "sub", secrets: same.secrets, ws: same.ws},
		"secret env names": {cwd: "sub", env: same.env, ws: same.ws},
		"workspace":        {cwd: "sub", env: same.env, secrets: same.secrets, ws: &jobs.Workspace{Repo: "r", Commit: "c2", Ref: "HEAD"}},
	} {
		if d := sessionSpecDiff(have, argv, r); d != want {
			t.Errorf("got %q, want %q", d, want)
		}
	}
	if d := sessionSpecDiff(have, []string{"copilot"}, same); d != "command" {
		t.Errorf("command: %q", d)
	}
	if d := sessionSpecDiff(have, argv, sessionReq{cwd: "sub", env: same.env, secrets: same.secrets}); d != "workspace" {
		t.Errorf("no workspace: %q", d)
	}
}

// TestSessionEnv: a new session gets LONGARM_WORKER / LONGARM_SESSION
// (what the caller set wins), and a start of the same spec still matches
// the running session that has them.
func TestSessionEnv(t *testing.T) {
	s := &Server{Name: "w1"}
	env := s.sessionEnv("agent1", map[string]string{"A": "1"})
	if env[EnvSessionWorker] != "w1" || env[EnvSessionName] != "agent1" || env["A"] != "1" {
		t.Fatalf("env %v", env)
	}
	if got := s.sessionEnv("agent1", map[string]string{EnvSessionName: "mine"}); got[EnvSessionName] != "mine" {
		t.Errorf("the caller's value must win: %v", got)
	}
	have := jobs.Spec{Argv: []string{"claude"}, Env: env}
	if d := sessionSpecDiff(have, []string{"claude"}, sessionReq{env: map[string]string{"A": "1"}}); d != "" {
		t.Errorf("same spec differs in %s", d)
	}
	if d := sessionSpecDiff(have, []string{"claude"}, sessionReq{env: map[string]string{"A": "2"}}); d != "env" {
		t.Errorf("other env: %q", d)
	}
}
