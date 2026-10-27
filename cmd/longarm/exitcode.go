package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/client"
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
