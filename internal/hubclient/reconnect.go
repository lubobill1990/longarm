package hubclient

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Reconnect is the backoff of a long-lived hub session (longarmd's hub
// agent, the longarm daemon's event relay). A hub redeploy (ACI: 30-60 s of
// nothing) must not cost the fleet another backoff period on top: while
// the failures are connection errors the waits are 1, 2, 4, 5, 5, ... s
// (FastFor long, then at most SlowCap), so a session is back within a few
// seconds of the hub answering. Other failures (the hub refuses the node)
// back off to RefusedCap. Every wait is jittered (±20%) so a fleet doesn't
// reconnect in lockstep. Not safe for concurrent use.
type Reconnect struct {
	// zero: the defaults below
	First, FastCap, FastFor, SlowCap, RefusedCap time.Duration

	attempt     int
	outageStart time.Time
	hint        time.Duration
	// Rand returns a number in [0, 1) (tests); nil = math/rand.
	Rand func() float64
	now  func() time.Time
}

// Defaults of Reconnect.
const (
	ReconnectFirst      = time.Second
	ReconnectFastCap    = 5 * time.Second
	ReconnectFastFor    = 3 * time.Minute
	ReconnectSlowCap    = 15 * time.Second
	ReconnectRefusedCap = 30 * time.Second
)

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (r *Reconnect) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// Connected: a session was established and worked; the next failure
// starts over at First.
func (r *Reconnect) Connected() {
	r.attempt, r.outageStart, r.hint = 0, time.Time{}, 0
}

// Restarting: the hub announced a restart (HubRestarting): the next wait
// is about after, and the outage that follows is a fresh one.
func (r *Reconnect) Restarting(after time.Duration) {
	r.Connected()
	if after <= 0 {
		after = or(r.First, ReconnectFirst)
	}
	r.hint = after
}

// Next is the wait before the next attempt after err ended the session or
// the attempt.
func (r *Reconnect) Next(err error) time.Duration {
	now := r.clock()
	if r.outageStart.IsZero() {
		r.outageStart = now
	}
	if h := r.hint; h > 0 {
		r.hint = 0
		// spread the fleet over [0.5, 1.5) x the hint
		return time.Duration(float64(h) * (0.5 + r.rand()))
	}
	limit := or(r.RefusedCap, ReconnectRefusedCap)
	if Unreachable(err) {
		limit = or(r.SlowCap, ReconnectSlowCap)
		if now.Sub(r.outageStart) < or(r.FastFor, ReconnectFastFor) {
			limit = or(r.FastCap, ReconnectFastCap)
		}
	}
	d := or(r.First, ReconnectFirst)
	for i := 0; i < r.attempt && d < limit; i++ {
		d *= 2
	}
	if d > limit {
		d = limit
	}
	r.attempt++
	return time.Duration(float64(d) * (0.8 + 0.4*r.rand()))
}

func (r *Reconnect) rand() float64 {
	if r.Rand != nil {
		return r.Rand()
	}
	return rand.Float64()
}

// Unreachable reports whether err means the hub could not be reached or the
// connection to it broke (as opposed to an answer such as PermissionDenied
// or FailedPrecondition).
func Unreachable(err error) bool {
	if err == nil {
		return true // a session that just ended
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Aborted:
			return true
		}
		return false
	}
	return true // not a gRPC answer: a transport failure
}
