package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/version"
)

// Exit codes of longarm itself (docs/usage.md#exit-codes). Commands that
// mirror a job (exec, run --wait/--follow, wait) exit with the job's code
// once it ran; these codes are longarm's own outcome otherwise.
const (
	exitUsage          = 2   // usage error or a request that can't succeed as given
	exitNotFound       = 3   // the job, worker or file does not exist
	exitHubUnavailable = 69  // --hub-only: no hub configured or it did not answer
	exitTempFail       = 75  // refused for capacity, quota or rate: retry later
	exitNoPerm         = 77  // not allowed (permission, authentication, trust)
	exitNoneCapable    = 79  // release push --allow-none: no worker can self-update, nothing rolled out
	exitKilled         = 137 // job was killed
	exitLost           = 254 // job was lost (supervisor vanished)
	exitLongarmError   = 255 // transport, hub or worker failure; anything else
)

// exitError carries a specific process exit code.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// codedError is an error with the exit code it maps to; the message is
// the wrapped error's.
type codedError struct {
	code int
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// usageErr marks err as a usage error (exit 2); nil stays nil.
func usageErr(err error) error {
	if err == nil {
		return nil
	}
	return &codedError{code: exitUsage, err: err}
}

func usagef(format string, a ...any) error { return usageErr(fmt.Errorf(format, a...)) }

func notFoundf(format string, a ...any) error {
	return &codedError{code: exitNotFound, err: fmt.Errorf(format, a...)}
}

// exitCodeFor maps the error of a command to longarm's exit code. Errors
// returned before a command's RunE ran (flag parsing, argument counts,
// unknown commands) are usage errors (validated = false).
func exitCodeFor(err error, validated bool) int {
	var ee *exitError
	var ce *codedError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.code
	case errors.As(err, &ce):
		return ce.code
	case !validated:
		return exitUsage
	}
	if _, ok := client.TrustRefusal(err); ok {
		return exitNoPerm
	}
	if s, ok := status.FromError(err); ok {
		return grpcExitCode(s.Code())
	}
	return exitLongarmError
}

// grpcExitCode maps the status of a hub or worker answer. Only answers
// that hold until the request (or the caller's rights) change leave 255;
// the rest - Unavailable, DeadlineExceeded, Internal, Unknown, Aborted,
// Canceled, DataLoss - stay "longarm error": retrying may help.
func grpcExitCode(c codes.Code) int {
	switch c {
	case codes.OK:
		return 0
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange, codes.AlreadyExists, codes.Unimplemented:
		return exitUsage
	case codes.NotFound:
		return exitNotFound
	case codes.ResourceExhausted:
		return exitTempFail
	case codes.PermissionDenied, codes.Unauthenticated:
		return exitNoPerm
	}
	return exitLongarmError
}

// markUsageErrors makes cobra's own errors (flags, argument counts) usage
// errors and records in *validated when a command's RunE starts.
func markUsageErrors(root *cobra.Command, validated *bool) {
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageErr(err) })
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if run := c.RunE; run != nil {
			c.RunE = func(cmd *cobra.Command, args []string) error {
				*validated = true
				return run(cmd, args)
			}
		}
		if run := c.Run; run != nil {
			c.Run = func(cmd *cobra.Command, args []string) {
				*validated = true
				run(cmd, args)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// cliFeatures are what this longarm supports beyond the base CLI, for
// programs driving it (longarm version --json; docs/usage.md#capabilities).
// Names are lowercase-hyphenated and never reused for something else.
var cliFeatures = []string{
	"exit-codes",       // the exit-code contract
	"jobs-source",      // jobs --json "source" / "unreachable"
	"jobs-hub-only",    // jobs --hub-only, jobs --id
	"stdin-file",       // run --stdin-file
	"lfs",              // run / sync --lfs skip|ship
	"ref-sha",          // run --repo <path> --ref <full commit id>
	"worker-label",     // longarm worker label
	"git-auth-forward", // run / sync --git-auth forward, run --lfs fetch
	// sessions for programs (docs/usage.md#sessions-for-programs)
	"session-start",        // session start
	"session-send",         // session send (--text, --stdin-file, --key, --raw)
	"session-send-if-idle", // session send --if-idle-ms (exit 75)
	"session-stop",         // session stop --grace
	"sessions-json-v2",     // fleet-wide sessions, sessions --json source/worker/activity fields, sessions -s
	"tail",                 // tail <worker> <path> [-f] [--json]
	"run-session",          // run / exec --session
	"job-id-short",         // logs / wait / kill / jobs --id: a unique prefix or suffix of a job id, or "last"
	"jobs-mine",            // jobs --mine / --all
	"doctor-json",          // longarm doctor --json (docs/usage.md#doctor---json)
	"worker-update",        // worker update <worker> [--latest|--version X] [--wait] [--json] [--auto on|off]
	"ls-version",           // ls: the VERSION column, --json updateAvailable
	"agent-auth",           // run / exec / session start / attach --agent-auth, MCP agent_auth (docs/usage.md#agent-credentials)
	// node identity (docs/security.md#node-identity): targets by name, id,
	// id prefix, id:/name:; worker rename / admin rename; ids in listings
	// and JSON (worker_id / workerId / id)
	"node-ids",
	"agent-handoff",  // handoff --to/--from: move a Claude Code / Copilot CLI session and its code
	"agent-sessions", // agent-sessions [<worker>|local] [--json]
}

func versionCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "version",
		Short: "Print longarm's version (--json: and its features, for programs driving longarm)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !asJSON {
				fmt.Println("longarm", version.Version)
				return nil
			}
			b, _ := json.MarshalIndent(struct {
				Version  string   `json:"version"`
				Features []string `json:"features"`
				// the hub built into this release (login without a hub)
				DefaultHub string `json:"default_hub,omitempty"`
			}{version.Version, cliFeatures, version.DefaultHub}, "", "  ")
			fmt.Println(string(b))
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, `{"version": "...", "features": [...], "default_hub": "..."}`)
	return c
}
