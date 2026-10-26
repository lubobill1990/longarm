package telemetry

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// Handler wraps a slog.Handler: records logged while handling an
// operation get trace=<trace id> (longarm diag finds them by it), and
// WARN / ERROR records become trace items of the default client (scrubbed,
// attributes as properties, rate limited per message).
type Handler struct {
	inner  slog.Handler
	attrs  []slog.Attr // WithAttrs, keys qualified by groups
	groups string
	lim    *limiter
}

// NewHandler wraps inner.
func NewHandler(inner slog.Handler) *Handler {
	return &Handler{inner: inner, lim: newLimiter(10, 10*time.Minute)}
}

// Enabled implements slog.Handler.
func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l) || l >= slog.LevelWarn && Default().Enabled()
}

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if id := TraceID(ctx); id != "" {
		r = r.Clone()
		r.AddAttrs(slog.String("trace", id))
	}
	var err error
	if h.inner.Enabled(ctx, r.Level) {
		err = h.inner.Handle(ctx, r)
	}
	if r.Level < slog.LevelWarn {
		return err
	}
	c := Default()
	if !c.Enabled() {
		return err
	}
	suppressed, ok := h.lim.allow(r.Message)
	if !ok {
		return err
	}
	props := map[string]string{}
	var add func(prefix string, a slog.Attr)
	add = func(prefix string, a slog.Attr) {
		v := a.Value.Resolve()
		key := prefix + a.Key
		if v.Kind() == slog.KindGroup {
			for _, g := range v.Group() {
				add(key+".", g)
			}
			return
		}
		if a.Key == "trace" || len(props) >= MaxProps {
			return
		}
		props[key] = v.String()
	}
	for _, a := range h.attrs {
		add("", a)
	}
	r.Attrs(func(a slog.Attr) bool { add(h.groups, a); return true })
	if suppressed > 0 {
		props["suppressed"] = strconv.Itoa(suppressed)
	}
	sev := SevWarning
	if r.Level >= slog.LevelError {
		sev = SevError
	}
	c.Trace(ctx, sev, r.Message, props)
	return err
}

// WithAttrs implements slog.Handler.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	n := *h
	n.inner = h.inner.WithAttrs(as)
	n.attrs = append([]slog.Attr(nil), h.attrs...)
	for _, a := range as {
		a.Key = h.groups + a.Key
		n.attrs = append(n.attrs, a)
	}
	return &n
}

// WithGroup implements slog.Handler.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	n := *h
	n.inner = h.inner.WithGroup(name)
	n.groups = h.groups + name + "."
	return &n
}

// limiter allows n records per message template per period and counts
// the ones it suppressed.
type limiter struct {
	n      int
	period time.Duration
	mu     sync.Mutex
	m      map[string]*limEntry
}

type limEntry struct {
	start      time.Time
	used       int
	suppressed int
}

func newLimiter(n int, period time.Duration) *limiter {
	return &limiter{n: n, period: period, m: map[string]*limEntry{}}
}

// allow reports whether a record with msg may go, and how many were
// suppressed before it.
func (l *limiter) allow(msg string) (int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e := l.m[msg]
	if e == nil {
		if len(l.m) >= 1000 {
			for k, v := range l.m {
				if now.Sub(v.start) > l.period {
					delete(l.m, k)
				}
			}
			if len(l.m) >= 1000 {
				return 0, false
			}
		}
		e = &limEntry{start: now}
		l.m[msg] = e
	}
	if now.Sub(e.start) > l.period {
		e.start, e.used = now, 0
	}
	if e.used >= l.n {
		e.suppressed++
		return 0, false
	}
	e.used++
	s := e.suppressed
	e.suppressed = 0
	return s, true
}
