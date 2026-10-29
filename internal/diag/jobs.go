package diag

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/pii"
)

// JobRoot is a directory of job or session directories.
type JobRoot struct {
	Kind  string // "job" | "session"
	Dir   string
	Label string // where, for the listing ("jobs", "users/<user:..>/sessions")
}

// JobMeta is a job's metadata: no command line, environment, working
// directory or output.
type JobMeta struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Where     string  `json:"where"`
	Owner     string  `json:"owner,omitempty"` // pseudonym
	State     string  `json:"state"`
	Created   string  `json:"created,omitempty"`
	Started   string  `json:"started,omitempty"`
	Ended     string  `json:"ended,omitempty"`
	ExitCode  *int    `json:"exit_code,omitempty"`
	Reason    string  `json:"reason,omitempty"`
	Orphaned  bool    `json:"orphaned,omitempty"`
	Pty       bool    `json:"pty,omitempty"`
	Workspace bool    `json:"workspace,omitempty"`
	RunAs     bool    `json:"run_as_account,omitempty"`
	LogBytes  int64   `json:"log_bytes"`
	MemLimit  int64   `json:"mem_limit_bytes,omitempty"`
	CPULimit  float64 `json:"cpu_limit,omitempty"`
	SpecError string  `json:"spec_error,omitempty"`
	at        time.Time
}

// JobDir is one job or session directory.
type JobDir struct {
	Root JobRoot
	Dir  jobs.Dir
}

// JobDirs lists the job directories of the roots.
func JobDirs(roots []JobRoot) []JobDir {
	var out []JobDir
	for _, r := range roots {
		ents, err := os.ReadDir(r.Dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() && jobs.ValidID(e.Name()) {
				out = append(out, JobDir{r, jobs.Dir(filepath.Join(r.Dir, e.Name()))})
			}
		}
	}
	return out
}

// JobOwners returns the owners of the jobs (for the scrubber's names).
func JobOwners(roots []JobRoot) []string {
	seen := map[string]bool{}
	var out []string
	for _, jd := range JobDirs(roots) {
		if s, err := jd.Dir.ReadSpec(); err == nil {
			if o := s.OwnerName(); o != "" && !seen[o] {
				seen[o] = true
				out = append(out, o)
			}
		}
	}
	return out
}

// JobList is the metadata of the jobs and sessions that were active or
// created within since (all active ones), newest first, at most limit.
func (b *Builder) JobList(roots []JobRoot, since time.Duration, limit int) []JobMeta {
	if limit <= 0 {
		limit = defaultJobLimit
	}
	cutoff := time.Now().Add(-since)
	out := []JobMeta{}
	for _, jd := range JobDirs(roots) {
		m := JobMeta{ID: jd.Dir.ID(), Kind: jd.Root.Kind, Where: jd.Root.Label, LogBytes: jd.Dir.LogSize()}
		st, err := jd.Dir.ReadStatus()
		if err != nil {
			m.State = "unreadable status: " + oneLine(err.Error())
		} else {
			m.State = string(st.State)
			m.ExitCode, m.Reason, m.Orphaned = st.ExitCode, st.Reason, st.Orphaned
			m.Created, m.at = ts(st.CreatedAt), st.CreatedAt
			m.Started, m.Ended = ts(st.StartedAt), ts(st.EndedAt)
			if st.State.Terminal() && since > 0 && st.EndedAt.Before(cutoff) && st.CreatedAt.Before(cutoff) {
				continue
			}
			if st.EndedAt.After(m.at) {
				m.at = st.EndedAt
			}
		}
		if s, err := jd.Dir.ReadSpec(); err != nil {
			m.SpecError = oneLine(err.Error())
		} else {
			if o := s.OwnerName(); o != "" {
				m.Owner = b.Scrub.Hash(pii.KindUser, o)
			}
			m.Pty, m.Workspace, m.RunAs = s.Pty, s.Workspace != nil, s.RunAs != nil
			m.MemLimit, m.CPULimit = s.MemLimitBytes, s.CPULimit
			if m.at.IsZero() {
				m.at = s.CreatedAt
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.After(out[j].at) })
	if len(out) > limit {
		b.Errorf("job list: %d jobs/sessions, only the newest %d listed", len(out), limit)
		out = out[:limit]
	}
	return out
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// JobDetail is what --include-job adds about a job.
type JobDetail struct {
	ID         string            `json:"id"`
	Name       string            `json:"name,omitempty"`
	Argv       []string          `json:"argv"`
	Cwd        string            `json:"cwd,omitempty"`
	EnvNames   []string          `json:"env_names,omitempty"` // values are never collected
	SecretEnv  []string          `json:"secret_env_names,omitempty"`
	AgentAuth  []string          `json:"agent_auth,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Workspace  *jobs.Workspace   `json:"workspace,omitempty"`
	Owner      string            `json:"owner,omitempty"`
	Status     *jobs.Status      `json:"status,omitempty"`
	LogMaxSize int64             `json:"log_max_bytes,omitempty"`
}

// IncludeJob adds a job's command line (scrubbed; environment names
// only) and the tail of its output: what the user asked for with
// --include-job. It reports whether the job was found.
func (b *Builder) IncludeJob(roots []JobRoot, id string) bool {
	for _, jd := range JobDirs(roots) {
		if jd.Dir.ID() != id {
			continue
		}
		dir := "jobs/" + id
		if jd.Root.Kind == "session" {
			dir = "sessions/" + id
		}
		s, err := jd.Dir.ReadSpec()
		if err != nil {
			b.Errorf("%s: %v", id, err)
			return true
		}
		d := JobDetail{ID: s.ID, Name: s.Name, Argv: s.Argv, Cwd: s.Cwd, SecretEnv: s.SecretEnvNames, AgentAuth: s.AgentAuth, Labels: s.Labels,
			Workspace: s.Workspace, LogMaxSize: s.LogMaxBytes}
		if o := s.OwnerName(); o != "" {
			d.Owner = b.Scrub.Hash(pii.KindUser, o)
		}
		for k := range s.Env {
			d.EnvNames = append(d.EnvNames, k)
		}
		sort.Strings(d.EnvNames)
		if st, err := jd.Dir.ReadStatus(); err == nil {
			st.AttachAddr = ""
			d.Status = st
		}
		b.AddJSON(dir+"/job.json", "--include-job: the job's command line (environment names only, no values)", d)
		b.AddFileTail(jd.Dir.Path(jobs.LogFile), dir+"/out.log", "--include-job: the tail of the job's output", JobOutputTail)
		b.AddFileTail(jd.Dir.Path(jobs.ShimLog), dir+"/shim.log", "--include-job: the job supervisor's log", 64<<10)
		return true
	}
	return false
}
