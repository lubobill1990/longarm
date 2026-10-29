package hubclient

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestReconnectBackoff(t *testing.T) {
	now := time.Unix(1000, 0)
	mid := func() float64 { return 0.5 } // no jitter: x1.0
	r := &Reconnect{Rand: mid, now: func() time.Time { return now }}
	down := status.Error(codes.Unavailable, "connection refused")
	var got []time.Duration
	for range 6 {
		got = append(got, r.Next(down))
	}
	want := []time.Duration{1, 2, 4, 5, 5, 5}
	for i, w := range want {
		if got[i] != w*time.Second {
			t.Fatalf("connection errors: waits %v, want %v s", got, want)
		}
	}
	// a long outage slows down to SlowCap
	now = now.Add(ReconnectFastFor + time.Second)
	for range 3 {
		r.Next(down)
	}
	if d := r.Next(down); d != ReconnectSlowCap {
		t.Fatalf("after %s down: %s, want %s", ReconnectFastFor, d, ReconnectSlowCap)
	}
	// a session that worked starts over
	r.Connected()
	if d := r.Next(down); d != time.Second {
		t.Fatalf("after Connected: %s", d)
	}

	// refusals back off further
	r2 := &Reconnect{Rand: mid, now: func() time.Time { return now }}
	refused := status.Error(codes.FailedPrecondition, "node is not approved")
	var last time.Duration
	for range 8 {
		last = r2.Next(refused)
	}
	if last != ReconnectRefusedCap {
		t.Fatalf("refused: %s, want %s", last, ReconnectRefusedCap)
	}

	// a restart notice: the hint, spread over [0.5, 1.5) x, then fast again
	r2.Restarting(2 * time.Second)
	if d := r2.Next(nil); d != 2*time.Second {
		t.Fatalf("restart hint: %s", d)
	}
	if d := r2.Next(down); d != time.Second {
		t.Fatalf("after the hint: %s", d)
	}

	// jitter stays within ±20%
	r3 := &Reconnect{}
	for range 200 {
		r3.Connected()
		if d := r3.Next(down); d < 800*time.Millisecond || d > 1200*time.Millisecond {
			t.Fatalf("jittered first wait %s", d)
		}
	}
}

func TestUnreachable(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, true},
		{status.Error(codes.Unavailable, "x"), true},
		{status.Error(codes.DeadlineExceeded, "x"), true},
		{errors.New("dial tcp: i/o timeout"), true},
		{status.Error(codes.PermissionDenied, "x"), false},
		{status.Error(codes.FailedPrecondition, "x"), false},
		{status.Error(codes.Unauthenticated, "x"), false},
	} {
		if got := Unreachable(c.err); got != c.want {
			t.Errorf("Unreachable(%v) = %v", c.err, got)
		}
	}
}
