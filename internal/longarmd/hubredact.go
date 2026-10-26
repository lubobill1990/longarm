package longarmd

import (
	"encoding/json"

	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// [access] hub_job_details = false: what this worker tells the hub about its
// jobs is limited to what scheduling, quotas and the panel's job list need
// (id, owner, submitter, sharing, state, times, exit code, resources). What
// users run - command lines, environment, directories, repositories, file
// paths, process names - stays in the local audit.log and job dirs.

// redactJob strips j (in place) of what reveals what its owner runs.
func redactJob(j *pb.Job) *pb.Job {
	if s := j.GetSpec(); s != nil {
		// The scheduler sizes jobs without a memory label by their type
		// (claude, copilot ...): keep that, not the command line.
		if s.Labels[sched.LabelMemGB] == "" && s.Labels[sched.LabelType] == "" {
			if typ := sched.TypeOf(s.Argv); typ != "" {
				labels := make(map[string]string, len(s.Labels)+1)
				for k, v := range s.Labels {
					labels[k] = v
				}
				labels[sched.LabelType] = typ
				s.Labels = labels
			}
		}
		s.Name, s.Argv, s.Cwd, s.Env, s.SecretEnvNames, s.Workspace = "", nil, "", nil, nil, nil
	}
	j.Workdir, j.Reason = "", "" // a start failure's reason names the program
	return j
}

// redactMetrics drops the process lists (names of what runs).
func redactMetrics(m *pb.MetricsSnapshot) *pb.MetricsSnapshot {
	m.TopCpu, m.TopMem = nil, nil
	return m
}

// hubAuditKeys are the audit fields sent to the hub; everything else
// (argv, cwd, env and secret names, paths, ssh commands, forward targets,
// repositories, the head of a truncated record ...) is dropped.
var hubAuditKeys = map[string]bool{
	"time": true, "who": true, "method": true, "user": true, "job": true, "session": true, "owner": true,
	"denied": true, "peer": true, "exit": true, "timed_out": true, "pty": true, "dry_run": true, "removed": true,
	"freed": true, "size": true, "truncated": true, "skipped_bytes": true, "error": true,
}

// redactAuditLine reduces one audit.log line to hubAuditKeys. Error texts
// and the denial reasons of forwards may quote commands and targets: they
// only say that something failed.
func redactAuditLine(line string) string {
	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		b, _ := json.Marshal(map[string]any{"unparsable": len(line)})
		return string(b)
	}
	_, forward := rec["target"]
	for k, v := range rec {
		switch {
		case !hubAuditKeys[k]:
			delete(rec, k)
		case k == "error" && v != "":
			rec[k] = "(redacted)"
		case k == "denied" && forward:
			rec[k] = true
		}
	}
	b, _ := json.Marshal(rec)
	return string(b)
}
