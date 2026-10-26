package longarmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func TestRedactJob(t *testing.T) {
	labels := map[string]string{"pool": "eu"}
	j := redactJob(&pb.Job{
		Spec: &pb.JobSpec{Id: "J1", Name: "deploy prod", Argv: []string{`C:\bin\claude.exe`, "-p", "secret plan"}, Cwd: "/src/x",
			Env: map[string]string{"K": "v"}, SecretEnvNames: []string{"TOKEN"}, Labels: labels, Owner: "alice",
			SubmittedBy: "client/alice", Workspace: &pb.JobWorkspace{Repo: "x", Url: "https://git/x"}, MemLimitBytes: 1 << 30},
		State: pb.JobState_JOB_STATE_FAILED, HasExitCode: true, ExitCode: 3, Workdir: "/w/x", Reason: "start: claude.exe: not found",
	})
	s := j.Spec
	if s.Name != "" || s.Argv != nil || s.Cwd != "" || s.Env != nil || s.SecretEnvNames != nil || s.Workspace != nil || j.Workdir != "" || j.Reason != "" {
		t.Errorf("details left: %v", j)
	}
	if s.Id != "J1" || s.Owner != "alice" || s.SubmittedBy != "client/alice" || s.MemLimitBytes != 1<<30 || j.ExitCode != 3 || j.State != pb.JobState_JOB_STATE_FAILED {
		t.Errorf("bookkeeping lost: %v", j)
	}
	// the scheduler still knows the job's type; the job's own labels are untouched
	if s.Labels[sched.LabelType] != "claude" || s.Labels["pool"] != "eu" || len(labels) != 1 {
		t.Errorf("labels %v (original %v)", s.Labels, labels)
	}
}

func TestRedactAuditLine(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{`{"time":"t","who":"client/alice","method":"/longarm.v1.Jobs/Submit","job":"J1","argv":["rm","-rf"],"cwd":"/x","env_keys":["K"],"secret_keys":["S"],"share":null,"error":""}`,
			`{"error":"","job":"J1","method":"/longarm.v1.Jobs/Submit","time":"t","who":"client/alice"}`},
		{`{"time":"t","who":"client/alice","method":"/longarm.v1.Exec/Run","argv":["x"],"exit":1,"error":"exec: \"x\": not found"}`,
			`{"error":"(redacted)","exit":1,"method":"/longarm.v1.Exec/Run","time":"t","who":"client/alice"}`},
		{`{"time":"t","who":"client/bob","method":"/longarm.v1.Forward/Open","target":"10.0.0.5:22","denied":"per-user isolation: forwards only reach loopback ports of your own processes (not 10.0.0.5)"}`,
			`{"denied":true,"method":"/longarm.v1.Forward/Open","time":"t","who":"client/bob"}`},
		{`{"time":"t","who":"admin/root","method":"/longarm.v1.Exec/Run","denied":"admins = none","peer":"1.2.3.4:5"}`,
			`{"denied":"admins = none","method":"/longarm.v1.Exec/Run","peer":"1.2.3.4:5","time":"t","who":"admin/root"}`},
		{`{"time":"t","who":"admin/root","method":"/longarm.v1.Files/Read","path":"/home/a/.ssh/id"}`,
			`{"method":"/longarm.v1.Files/Read","time":"t","who":"admin/root"}`},
		{`{"time":"t","who":"x","method":"m","truncated":99999,"head":"{\"argv\":[\"secret"}`,
			`{"method":"m","time":"t","truncated":99999,"who":"x"}`},
		{`not json argv=secret`, `{"unparsable":20}`},
	} {
		got := redactAuditLine(tt.in)
		if got != tt.want {
			t.Errorf("redact %s\n got %s\nwant %s", tt.in, got, tt.want)
		}
		if !json.Valid([]byte(got)) || strings.Contains(got, "secret") {
			t.Errorf("bad output %s", got)
		}
	}
}
