// Package jobs defines the job model shared by longarmd and its per-job shim:
// the state machine (DESIGN §5.1), the on-disk job directory and the log
// offset reader.
//
// Job directory (DESIGN §5, <data dir>\jobs\<id>):
//
//	<data>/jobs/<id>/
//	  job.json     spec (written once by longarmd at submit; no secret values)
//	  status.json  full runtime status; the source of truth (atomic replace)
//	  state        compat: one word, e.g. "running"
//	  pid          compat: pid of the job's main process
//	  exit         compat: exit code, once terminal
//	  out.log      stdout+stderr bytes, secrets redacted
//	  result.md    optional, written by agent jobs themselves
//	  kill         marker: longarmd asks the shim to kill the tree
//	  shim.log     the shim's own diagnostics
//
// Ownership: longarmd writes status.json before spawning the shim and only after
// the shim is gone; while the shim lives, only the shim writes it.
package jobs

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// State is a job state (DESIGN §5.1).
type State string

const (
	Queued    State = "queued"   // hub-side (P1)
	Assigned  State = "assigned" // hub-side (P1)
	Preparing State = "preparing"
	Running   State = "running"
	Done      State = "done"
	Failed    State = "failed"
	Killed    State = "killed"
	Lost      State = "lost"
)

// AllStates in lifecycle order.
var AllStates = []State{Queued, Assigned, Preparing, Running, Done, Failed, Killed, Lost}

// Terminal reports whether no further transitions happen without outside
// action. Lost is terminal for P0a (retry/resume policies arrive in P1, which
// adds lost -> running).
func (s State) Terminal() bool {
	switch s {
	case Done, Failed, Killed, Lost:
		return true
	}
	return false
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	for _, x := range AllStates {
		if s == x {
			return true
		}
	}
	return false
}

var transitions = map[State][]State{
	Queued:    {Assigned, Killed},
	Assigned:  {Preparing, Queued, Killed},
	Preparing: {Running, Failed, Killed, Lost},
	Running:   {Done, Failed, Killed, Lost},
	Lost:      {Running}, // resume with retries (P1)
}

// CanTransition reports whether from -> to is allowed. Re-asserting the same
// state is allowed (idempotent writes).
func CanTransition(from, to State) bool {
	if from == to {
		return true
	}
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// CheckTransition returns an error for a disallowed transition.
func CheckTransition(from, to State) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("invalid job state transition %s -> %s", from, to)
	}
	return nil
}

// Spec is job.json.
type Spec struct {
	ID             string            `json:"id"`
	Name           string            `json:"name,omitempty"`
	Argv           []string          `json:"argv"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	SecretEnvNames []string          `json:"secret_env_names,omitempty"`
	MemLimitBytes  int64             `json:"mem_limit_bytes,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	SubmittedBy    string            `json:"submitted_by,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	// Workspace: prepare a git worktree as the working directory (P0b).
	Workspace *Workspace `json:"workspace,omitempty"`
	// Pty: run the command in a pseudo terminal with an attach channel
	// (interactive sessions, <data>/sessions/<name>).
	Pty bool `json:"pty,omitempty"`
	// Initial terminal size for Pty.
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	// LogMaxBytes caps out.log; it rotates to out.log.1 (0 = unlimited).
	LogMaxBytes int64 `json:"log_max_bytes,omitempty"`
	// Owner is the user the job belongs to (multi-tenancy); ShareTeams may
	// read it.
	Owner      string   `json:"owner,omitempty"`
	ShareTeams []string `json:"share_teams,omitempty"`
	// CPULimit caps the tree's CPU in cores (0 = none).
	CPULimit float64 `json:"cpu_limit,omitempty"`
	// RunAs: per-user isolation - the shim (LocalSystem / root) starts the
	// workload as this account. Written by longarmd only; job directories
	// are not writable by the account.
	RunAs *RunAs `json:"run_as,omitempty"`
	// StdinBytes is the size of the job's stdin payload (its content is
	// memory-only: longarmd -> shim -> the job, never on disk).
	StdinBytes int64 `json:"stdin_bytes,omitempty"`
}

// MaxStdinBytes caps a job's stdin payload (longarm run --stdin-file).
const MaxStdinBytes = 1 << 20

// StdinLostReason is the reason of a job that took a stdin payload and
// ended before it was handed to the job (longarmd restarted, the shim did
// not start): it never ran. Resubmitting the same id with the payload
// replaces it.
const StdinLostReason = "stdin payload lost before start; resubmit"

// FeatureStdin is the InfoResponse.features entry of workers that accept
// SubmitRequest.stdin.
const FeatureStdin = "job-stdin"

// FeatureGitAuth is the InfoResponse.features entry of workers that accept
// a forwarded fetch credential (SubmitRequest.git_auth, Workspace.Fetch).
const FeatureGitAuth = "git-auth-forward"

// InfoResponse.features of workers with the programmable sessions API.
const (
	FeatureSessionStart      = "session-start"        // Sessions.Start (with a workspace), Sessions.Get
	FeatureSessionSend       = "session-send"         // Sessions.Send
	FeatureSessionSendIfIdle = "session-send-if-idle" // SendSessionRequest.if_idle_ms
	FeatureSessionStop       = "session-stop"         // Sessions.Stop
	FeatureSessionInfo       = "session-info"         // Session workdir, dir, reason and activity fields
	FeatureFileTail          = "file-tail"            // Files.Tail
)

// SessionWorktreeKey names the worktree of one generation of session name
// (created: its spec's CreatedAt) under <data>/worktrees: "s.<name>.<gen>".
// The dots keep it apart from job ids (ValidID); the generation keeps a
// session that replaced an ended one of the name off the old one's
// worktree, which is removed concurrently.
func SessionWorktreeKey(name string, created time.Time) string {
	return "s." + name + "." + strconv.FormatInt(created.UnixNano(), 36)
}

// ParseSessionWorktreeKey splits a SessionWorktreeKey.
func ParseSessionWorktreeKey(k string) (name, gen string, ok bool) {
	rest, ok := strings.CutPrefix(k, "s.")
	if !ok {
		return "", "", false
	}
	i := strings.LastIndexByte(rest, '.')
	if i < 0 {
		return "", "", false
	}
	name, gen = rest[:i], rest[i+1:]
	if !ValidID(name) || !genRE.MatchString(gen) {
		return "", "", false
	}
	return name, gen, true
}

var genRE = regexp.MustCompile(`^-?[0-9a-z]{1,16}$`)

// ValidWorktreeKey reports whether k is a job id or a SessionWorktreeKey.
func ValidWorktreeKey(k string) bool {
	if _, _, ok := ParseSessionWorktreeKey(k); ok {
		return true
	}
	return ValidID(k)
}

// OwnerName is the job's owner; jobs from before multi-tenancy: the name in
// SubmittedBy ("client/alice").
func (s *Spec) OwnerName() string {
	if s.Owner != "" {
		return s.Owner
	}
	if _, name, ok := strings.Cut(s.SubmittedBy, "/"); ok {
		return name
	}
	return s.SubmittedBy
}

// RunAs describes how a per-user job runs.
type RunAs struct {
	Account string `json:"account"`
	// Cgroup (Linux): the job's cgroup v2 directory ("" = none).
	Cgroup string `json:"cgroup,omitempty"`
	// Tmp is the account's private temp dir (TMPDIR on Unix).
	Tmp string `json:"tmp,omitempty"`
	// Wrapper is prepended to the command (Windows: longarmd conhost --,
	// which gives the workload a UTF-8 console of its own).
	Wrapper []string `json:"wrapper,omitempty"`
}

// Workspace describes a git worktree prepared for a job (DESIGN §5.3).
type Workspace struct {
	Repo      string `json:"repo"`
	URL       string `json:"url,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Commit    string `json:"commit,omitempty"`
	StagingID string `json:"staging_id,omitempty"`
	// Overlay identifies the staged uncommitted changes and untracked
	// files (Store.StagingDigest; "" without staging). Unlike StagingID,
	// which every upload gets anew, it is part of what makes a resubmit
	// the same job.
	Overlay string `json:"overlay,omitempty"`
	// LFSFetch: also fetch the commit's Git LFS content from URL.
	LFSFetch bool `json:"lfs_fetch,omitempty"`
	// GitAuth: the fetch of URL used a credential forwarded with the
	// submit (never stored; this only records that there was one).
	GitAuth bool `json:"git_auth,omitempty"`
}

// GitAuth is a credential forwarded for a workspace fetch (longarm run
// --git-auth forward). It lives in memory only: the fetch sends it to the
// fetched url only and Clear zeroes it afterwards.
type GitAuth struct {
	Scheme   string // GitAuthBearer or GitAuthBasic
	Username string // basic
	Secret   []byte
}

// GitAuth schemes.
const (
	GitAuthBearer = "bearer"
	GitAuthBasic  = "basic"
)

// Clear zeroes the secret (a nil *GitAuth is fine).
func (a *GitAuth) Clear() {
	if a != nil {
		clear(a.Secret)
		a.Secret = nil
	}
}

// Valid reports whether a has a known scheme and a secret.
func (a *GitAuth) Valid() bool {
	return a != nil && len(a.Secret) > 0 && (a.Scheme == GitAuthBearer || a.Scheme == GitAuthBasic)
}

// Status is status.json.
type Status struct {
	State     State     `json:"state"`
	Seq       uint64    `json:"seq"` // bumped on every write; change detection
	CreatedAt time.Time `json:"created_at"`
	StartedAt time.Time `json:"started_at,omitzero"`
	EndedAt   time.Time `json:"ended_at,omitzero"`
	// Main process of the job and its creation-time token (pid reuse guard).
	PID      int    `json:"pid,omitempty"`
	PIDStart uint64 `json:"pid_start,omitempty"`
	// The shim supervising the job.
	ShimPID   int    `json:"shim_pid,omitempty"`
	ShimStart uint64 `json:"shim_start,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// Orphaned: the shim is gone but the main process is still alive; longarmd
	// adopts it read-only (watches the pid; the exit code will be unknown).
	Orphaned bool `json:"orphaned,omitempty"`
	// AttachAddr is the shim's attach channel (Pty jobs): a named pipe on
	// Windows, a unix socket path elsewhere.
	AttachAddr string `json:"attach_addr,omitempty"`
	// Workdir is the prepared worktree of a workspace job.
	Workdir string `json:"workdir,omitempty"`
}

var idRE = regexp.MustCompile(`^[0-9A-Za-z_-]{1,64}$`)

// ValidID reports whether id is safe to use as a directory name.
// Windows reserved device names (CON, NUL, COM1, ...) are rejected on every
// platform so job dirs stay portable.
func ValidID(id string) bool { return idRE.MatchString(id) && !reservedRE.MatchString(id) }

var reservedRE = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])$`)

// Exit codes for jobs without a natural one.
const (
	ExitNotFound = 127 // command could not be started
)
