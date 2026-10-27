package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
)

// Span is a W3C trace context position: the operation (trace) id shared by
// everything one CLI command or incoming request causes, this span's id
// and its parent's. Items recorded with a context carrying a Span link to it
// (Application Insights: ai.operation.id / ai.operation.parentId).
type Span struct {
	TraceID  string // 32 hex
	SpanID   string // 16 hex
	ParentID string // 16 hex; "" = root
}

// TraceparentKey is the gRPC metadata key (and HTTP header) carrying the
// trace context.
const TraceparentKey = "traceparent"

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// NewSpan starts a new operation.
func NewSpan() Span { return Span{TraceID: randHex(16), SpanID: randHex(8)} }

// Child is a new span below s (an outgoing call, a sub-operation).
func (s Span) Child() Span {
	if s.TraceID == "" {
		return NewSpan()
	}
	return Span{TraceID: s.TraceID, SpanID: randHex(8), ParentID: s.SpanID}
}

// Valid reports whether s has a trace id.
func (s Span) Valid() bool { return s.TraceID != "" }

// Short is the first 8 hex digits of the trace id: what the CLI prints on
// errors ("(trace 4bf92f35)") and a user quotes in a bug report.
func (s Span) Short() string {
	if len(s.TraceID) < 8 {
		return s.TraceID
	}
	return s.TraceID[:8]
}

// Traceparent formats s as a W3C traceparent header value.
func (s Span) Traceparent() string {
	return "00-" + s.TraceID + "-" + s.SpanID + "-01"
}

// ParseTraceparent parses a traceparent value. The result is the remote
// span: ServerSpan derives the local one from it.
func ParseTraceparent(v string) (Span, bool) {
	p := strings.Split(strings.TrimSpace(v), "-")
	if len(p) < 4 || len(p[0]) != 2 || p[0] == "ff" || !isHex(p[1], 32) || !isHex(p[2], 16) || !isHex(p[3], 2) {
		return Span{}, false
	}
	if strings.Trim(p[1], "0") == "" || strings.Trim(p[2], "0") == "" {
		return Span{}, false
	}
	return Span{TraceID: strings.ToLower(p[1]), SpanID: strings.ToLower(p[2])}, true
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ServerSpan is the span of an incoming request whose caller sent
// traceparent (remote, ok) - or a new operation.
func ServerSpan(remote Span, ok bool) Span {
	if !ok {
		return NewSpan()
	}
	return remote.Child()
}

type spanKey struct{}

// WithSpan returns ctx carrying s.
func WithSpan(ctx context.Context, s Span) context.Context {
	return context.WithValue(ctx, spanKey{}, s)
}

// SpanFrom returns the span of ctx, else the process fallback (SetFallback),
// else ok = false.
func SpanFrom(ctx context.Context) (Span, bool) {
	if ctx != nil {
		if s, ok := ctx.Value(spanKey{}).(Span); ok && s.Valid() {
			return s, true
		}
	}
	if f := fallback.Load(); f != nil && f.Valid() {
		return *f, true
	}
	return Span{}, false
}

var fallback atomic.Pointer[Span]

// SetFallback makes s the span of every context without one: a CLI
// invocation is one operation, also where code uses context.Background().
func SetFallback(s Span) { fallback.Store(&s) }

// TraceID is the operation id of ctx ("" = none).
func TraceID(ctx context.Context) string {
	s, _ := SpanFrom(ctx)
	return s.TraceID
}

// ops remembers the operation that started something longer-lived (a
// job), so later items about it link to the same operation.
var ops = struct {
	sync.Mutex
	m     map[string]Span
	order []string
}{m: map[string]Span{}}

const maxOps = 10000

// Remember records the span of ctx under key (e.g. "job/<id>").
func Remember(ctx context.Context, key string) {
	s, ok := SpanFrom(ctx)
	if !ok {
		return
	}
	ops.Lock()
	defer ops.Unlock()
	if _, had := ops.m[key]; !had {
		ops.order = append(ops.order, key)
	}
	ops.m[key] = s
	for len(ops.order) > maxOps {
		delete(ops.m, ops.order[0])
		ops.order = ops.order[1:]
	}
}

// Recall returns a context carrying the span remembered under key (or
// ctx unchanged).
func Recall(ctx context.Context, key string) context.Context {
	ops.Lock()
	s, ok := ops.m[key]
	ops.Unlock()
	if !ok {
		return ctx
	}
	return WithSpan(ctx, s)
}
