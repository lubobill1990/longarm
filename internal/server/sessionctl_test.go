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
