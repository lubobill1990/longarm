package hub

import (
	"net"
	"slices"
	"sync"
	"time"

	"golang.org/x/time/rate"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// bus fans fleet events out to subscribers (Fleet.Events, WaitJob, SSE).
// Publishing never blocks: a subscriber that falls behind by more than its
// buffer is dropped (its stream ends and the client re-subscribes).
type bus struct {
	mu   sync.Mutex
	subs map[*sub]struct{}
}

type sub struct {
	ch     chan *pb.FleetEvent
	kinds  []string
	jobID  string
	closed bool
	// filter (multi-tenancy) drops (nil) or redacts events for this
	// subscriber; owner is the user an event is about ("" = none). It runs
	// with the bus locked and must not take the hub's lock.
	filter func(ev *pb.FleetEvent, owner string) *pb.FleetEvent
}

func newBus() *bus { return &bus{subs: map[*sub]struct{}{}} }

func (b *bus) subscribe(kinds []string, jobID string) *sub {
	return b.subscribeFiltered(kinds, jobID, nil)
}

func (b *bus) subscribeFiltered(kinds []string, jobID string, filter func(*pb.FleetEvent, string) *pb.FleetEvent) *sub {
	s := &sub{ch: make(chan *pb.FleetEvent, 512), kinds: kinds, jobID: jobID, filter: filter}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (b *bus) unsubscribe(s *sub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[s]; ok {
		delete(b.subs, s)
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
	}
}

func (b *bus) publish(ev *pb.FleetEvent) { b.publishOwned(ev, "") }

// publishOwned publishes an event about owner's things (queue events).
func (b *bus) publishOwned(ev *pb.FleetEvent, owner string) {
	if ev.Time == nil {
		ev.Time = timestamppb.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		if len(s.kinds) > 0 && !slices.Contains(s.kinds, ev.Kind) {
			continue
		}
		if s.jobID != "" && (ev.Job == nil || ev.Job.Job.GetSpec().GetId() != s.jobID) {
			continue
		}
		out := ev
		if s.filter != nil {
			if out = s.filter(ev, owner); out == nil {
				continue
			}
		}
		select {
		case s.ch <- out:
		default: // too slow: drop the subscriber
			delete(b.subs, s)
			s.closed = true
			close(s.ch)
		}
	}
}

func (b *bus) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		delete(b.subs, s)
		if !s.closed {
			s.closed = true
			close(s.ch)
		}
	}
}

// ipLimiter is a per-IP token bucket (join and login attempts).
type ipLimiter struct {
	mu    sync.Mutex
	r     rate.Limit
	burst int
	m     map[string]*limEntry
}

type limEntry struct {
	l    *rate.Limiter
	seen time.Time
}

func newIPLimiter(perSec float64, burst int) *ipLimiter {
	return &ipLimiter{r: rate.Limit(perSec), burst: burst, m: map[string]*limEntry{}}
}

// allow takes a token for addr's IP.
func (l *ipLimiter) allow(addr string) bool {
	ip := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		ip = h
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.m) > 10000 {
		for k, e := range l.m {
			if now.Sub(e.seen) > 10*time.Minute {
				delete(l.m, k)
			}
		}
	}
	e := l.m[ip]
	if e == nil {
		e = &limEntry{l: rate.NewLimiter(l.r, l.burst)}
		l.m[ip] = e
	}
	e.seen = now
	return e.l.Allow()
}

func tspb(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// blocked reports whether addr's IP has no token left (without taking one).
func (l *ipLimiter) blocked(addr string) bool {
	ip := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		ip = h
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.m[ip]
	return e != nil && e.l.Tokens() < 1
}
