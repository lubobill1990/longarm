// Package telemetry records what maintainers need to find out what went
// wrong in the field - without personal data leaving the machine
// (docs/telemetry.md).
//
// Items (events, exceptions, requests, dependencies, traces, metrics; the
// Application Insights types) are scrubbed (internal/pii) and size-capped
// when they are recorded, buffered in memory and queued on disk
// (<dir>/queue.jsonl). Nodes push the queue to longarm-hub
// (internal/telemetry/tgrpc), which scrubs again and forwards to Azure
// Application Insights (internal/telemetry/appinsights). Nothing is
// collected when the hub has no telemetry destination or the local owner
// opted out.
//
// The package depends on the standard library and internal/pii only; a
// nil *Client (telemetry off) accepts every call and does nothing.
package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lubobill1990/longarm/internal/pii"
)

// Item kinds (Application Insights base types).
const (
	KindEvent      = "event"
	KindException  = "exception"
	KindRequest    = "request"
	KindDependency = "dependency"
	KindTrace      = "trace"
	KindMetric     = "metric"
)

// Severities of traces and exceptions.
const (
	SevVerbose  = "verbose"
	SevInfo     = "info"
	SevWarning  = "warning"
	SevError    = "error"
	SevCritical = "critical"
)

// Item is one telemetry item. Every string in it has been scrubbed.
type Item struct {
	Kind     string    `json:"kind"`
	Time     time.Time `json:"time"`
	Name     string    `json:"name,omitempty"`
	Message  string    `json:"message,omitempty"`
	Severity string    `json:"severity,omitempty"`

	Props        map[string]string  `json:"props,omitempty"`
	Measurements map[string]float64 `json:"measurements,omitempty"`

	OperationID string `json:"op,omitempty"`     // trace id
	ParentID    string `json:"parent,omitempty"` // parent span id
	ID          string `json:"id,omitempty"`     // own span id (request, dependency)

	DurationMS float64 `json:"duration_ms,omitempty"`
	Success    bool    `json:"success,omitempty"`
	ResultCode string  `json:"result_code,omitempty"`
	Type       string  `json:"type,omitempty"`   // dependency type
	Target     string  `json:"target,omitempty"` // dependency target kind (never an address)

	Stack []Frame `json:"stack,omitempty"`

	Value float64 `json:"value,omitempty"` // metric: sum
	Count int64   `json:"count,omitempty"`
	Min   float64 `json:"min,omitempty"`
	Max   float64 `json:"max,omitempty"`

	Context Context `json:"ctx"`
}

// Frame is a stack frame: function and file:line, no directories outside
// the module.
type Frame struct {
	Function string `json:"fn"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// Context says who sent an item, pseudonymously.
type Context struct {
	Component string `json:"component"` // longarm | longarmd | longarm-hub | longarm-tray
	Version   string `json:"version,omitempty"`
	OS        string `json:"os,omitempty"`
	Arch      string `json:"arch,omitempty"`
	Session   string `json:"session,omitempty"` // random per process
	Fleet     string `json:"fleet,omitempty"`   // FleetID
	Node      string `json:"node,omitempty"`    // <node:hash>
	User      string `json:"user,omitempty"`    // <user:hash>
	Role      string `json:"role,omitempty"`    // worker | client | admin | hub
}

// Identity is the node an item comes from; it becomes the pseudonyms of
// Context and the names the scrubber replaces.
type Identity struct {
	CA   []byte // the fleet CA certificate (DER); nil = unknown fleet
	Node string // certificate name
	User string // owner (devices of a user)
	Role string
}

// FleetID is the fleet's pseudonym: a short hash of the CA fingerprint.
func FleetID(caDER []byte) string {
	if len(caDER) == 0 {
		return ""
	}
	fp := sha256.Sum256(caDER)
	h := sha256.Sum256(append([]byte("longarm-fleet-v1\x00"), hex.EncodeToString(fp[:])...))
	return hex.EncodeToString(h[:6])
}

// NewScrubber is the scrubber of a node: its own names (node, user) plus
// the local host and OS user (pii's defaults), keyed by the fleet salt.
func NewScrubber(id Identity, extra map[string]string) *pii.Scrubber {
	names := map[string]string{}
	for k, v := range extra {
		names[k] = v
	}
	if id.Node != "" {
		names[id.Node] = pii.KindNode
	}
	if id.User != "" {
		names[id.User] = pii.KindUser
	}
	var salt []byte
	if len(id.CA) > 0 {
		salt = pii.FleetSalt(id.CA)
	}
	return pii.New(pii.Options{Salt: salt, Names: names})
}

// Config configures a Client.
type Config struct {
	Component string
	Version   string
	// Dir holds the queue, the state from the hub and crash reports
	// (<data>/telemetry, <config>/telemetry); "" = no disk.
	Dir string
	// Sink, when set, receives every item at once instead of the queue
	// (longarm-hub: its forwarder).
	Sink     func([]Item)
	Identity Identity
	// Names are more names to scrub (name -> pii kind).
	Names map[string]string
	// Scrubber replaces the one built from Identity and Names.
	Scrubber *pii.Scrubber
	// Disabled: the local owner opted out. Nothing is recorded or queued.
	Disabled bool
	// Detailed sends every request and dependency (short-lived CLI runs);
	// otherwise only failed and slow ones, plus per-minute aggregates.
	Detailed bool
	// SlowRequest: requests taking longer are sent individually (default 2 s).
	SlowRequest time.Duration
	// MaxQueueBytes / MaxQueueItems bound the disk queue (default 5 MiB /
	// 10000) and the memory buffer (MaxQueueItems).
	MaxQueueBytes int64
	MaxQueueItems int
}

// Client records items. Safe for concurrent use; nil = off.
type Client struct {
	cfg     Config
	ctx     Context
	queue   *Queue
	scrub   atomic.Pointer[pii.Scrubber]
	agg     *aggregator
	started time.Time
	// watching: WatchCrashes is on
	watching atomic.Bool

	mu      sync.Mutex
	buf     []Item
	dropped int64
	// off: the hub doesn't collect (until recheck).
	off     bool
	recheck time.Time
}

// New builds a Client. It does not start anything; Start also makes it
// the default and reports crashes of earlier runs.
func New(cfg Config) *Client {
	if cfg.SlowRequest <= 0 {
		cfg.SlowRequest = 2 * time.Second
	}
	if cfg.MaxQueueBytes <= 0 {
		cfg.MaxQueueBytes = 5 << 20
	}
	if cfg.MaxQueueItems <= 0 {
		cfg.MaxQueueItems = 10000
	}
	c := &Client{cfg: cfg, agg: newAggregator(), started: time.Now()}
	c.ctx = Context{
		Component: cfg.Component, Version: cfg.Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Session: randHex(8), Fleet: FleetID(cfg.Identity.CA), Role: cfg.Identity.Role,
	}
	s := cfg.Scrubber
	if s == nil {
		s = NewScrubber(cfg.Identity, cfg.Names)
	}
	c.scrub.Store(s)
	if cfg.Identity.Node != "" {
		c.ctx.Node = s.Hash(pii.KindNode, cfg.Identity.Node)
	}
	if cfg.Identity.User != "" {
		c.ctx.User = s.Hash(pii.KindUser, cfg.Identity.User)
	}
	if cfg.Dir != "" {
		c.queue = &Queue{Dir: cfg.Dir, MaxBytes: cfg.MaxQueueBytes, MaxItems: cfg.MaxQueueItems}
		if st, err := LoadState(cfg.Dir); err == nil && st.Collecting != nil && !*st.Collecting && time.Now().Before(st.Recheck) {
			c.off, c.recheck = true, st.Recheck
		}
	}
	return c
}

var def atomic.Pointer[Client]

// Default is the process's client (nil = telemetry off).
func Default() *Client { return def.Load() }

// SetDefault makes c the process's client.
func SetDefault(c *Client) { def.Store(c) }

// Start builds a Client, makes it the default and queues crash reports
// left by earlier runs in its dir.
func Start(cfg Config) *Client {
	c := New(cfg)
	SetDefault(c)
	c.ReportCrashes()
	return c
}

// Context is the context stamped on every item.
func (c *Client) Context() Context {
	if c == nil {
		return Context{}
	}
	return c.ctx
}

// Config returns the client's configuration.
func (c *Client) Config() Config {
	if c == nil {
		return Config{}
	}
	return c.cfg
}

// Queue is the disk queue (nil without a dir).
func (c *Client) Queue() *Queue {
	if c == nil {
		return nil
	}
	return c.queue
}

// Scrubber is the scrubber applied to every item.
func (c *Client) Scrubber() *pii.Scrubber {
	if c == nil {
		return nil
	}
	return c.scrub.Load()
}

// SetScrubber replaces the scrubber (the hub's, when its registry changed).
func (c *Client) SetScrubber(s *pii.Scrubber) {
	if c != nil && s != nil {
		c.scrub.Store(s)
	}
}

// Enabled reports whether items are recorded: not opted out, and the hub
// did not say it doesn't collect.
func (c *Client) Enabled() bool {
	if c == nil || c.cfg.Disabled {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.off
}

// Uptime is the time since the client was created.
func (c *Client) Uptime() time.Duration {
	if c == nil {
		return 0
	}
	return time.Since(c.started)
}

// record finishes it (time, context, operation, scrubbing, caps) and
// buffers it.
func (c *Client) record(ctx context.Context, it Item) {
	if !c.Enabled() {
		return
	}
	if it.Time.IsZero() {
		it.Time = time.Now()
	}
	it.Time = it.Time.UTC()
	if it.OperationID == "" {
		if s, ok := SpanFrom(ctx); ok {
			it.OperationID, it.ParentID = s.TraceID, s.SpanID
		}
	}
	if it.Context.Component == "" {
		it.Context = c.ctx
	}
	Prepare(&it, c.scrub.Load())
	if c.cfg.Sink != nil {
		c.cfg.Sink([]Item{it})
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf = append(c.buf, it)
	if over := len(c.buf) - c.cfg.MaxQueueItems; over > 0 {
		c.buf = append(c.buf[:0:0], c.buf[over:]...)
		c.dropped += int64(over)
	}
}

// Event records a named event.
func (c *Client) Event(ctx context.Context, name string, props map[string]string, meas map[string]float64) {
	if c == nil {
		return
	}
	c.record(ctx, Item{Kind: KindEvent, Name: name, Props: props, Measurements: meas})
}

// Exception records an error or panic: its type, message (scrubbed) and
// stack (function, file:line).
func (c *Client) Exception(ctx context.Context, typ, msg string, stack []Frame, props map[string]string) {
	if c == nil {
		return
	}
	c.record(ctx, Item{Kind: KindException, Name: typ, Message: msg, Severity: SevError, Stack: stack, Props: props})
}

// Error records err as an exception of type class (e.g. a gRPC code).
func (c *Client) Error(ctx context.Context, class string, err error, props map[string]string) {
	if c == nil || err == nil {
		return
	}
	if class == "" {
		class = fmt.Sprintf("%T", errors.Unwrap(err))
		if class == "<nil>" {
			class = fmt.Sprintf("%T", err)
		}
	}
	c.Exception(ctx, class, err.Error(), nil, props)
}

// Trace records a log message.
func (c *Client) Trace(ctx context.Context, severity, msg string, props map[string]string) {
	if c == nil {
		return
	}
	c.record(ctx, Item{Kind: KindTrace, Message: msg, Severity: severity, Props: props})
}

// Metric records one value.
func (c *Client) Metric(name string, value float64, props map[string]string) {
	if c == nil {
		return
	}
	c.record(context.Background(), Item{Kind: KindMetric, Name: name, Value: value, Count: 1, Min: value, Max: value, Props: props})
}

// Count adds n to a counter sent once a minute as a metric (name, props).
func (c *Client) Count(name string, props map[string]string, n float64) {
	if c == nil || !c.Enabled() {
		return
	}
	c.agg.count(name, props, n)
}

// Request is an incoming request (an RPC, a CLI command).
type Request struct {
	Name     string
	Span     Span // the request's own span (ID, parent)
	Duration time.Duration
	Code     string
	Success  bool
	Props    map[string]string
	// Stream: long-lived; sent individually only when it failed.
	Stream bool
	// Always sends it individually.
	Always bool
}

// Request records r: individually when it failed, was slow (or Detailed /
// Always), and always in the per-minute aggregate of its name.
func (c *Client) Request(ctx context.Context, r Request) {
	if c == nil || !c.Enabled() {
		return
	}
	ms := float64(r.Duration) / float64(time.Millisecond)
	if !c.cfg.Detailed {
		c.agg.duration(KindRequest, r.Name, ms, r.Success, r.Stream)
	}
	if !(r.Always || c.cfg.Detailed || !r.Success || !r.Stream && r.Duration > c.cfg.SlowRequest) {
		return
	}
	it := Item{Kind: KindRequest, Name: r.Name, ID: r.Span.SpanID, OperationID: r.Span.TraceID, ParentID: r.Span.ParentID,
		DurationMS: ms, Success: r.Success, ResultCode: r.Code, Props: r.Props, Time: time.Now().Add(-r.Duration)}
	c.record(ctx, it)
}

// Dependency is an outgoing call.
type Dependency struct {
	Type     string // grpc | http | dial
	Target   string // the kind of peer: worker | hub | appinsights - never an address
	Name     string // method
	Span     Span   // the call's span
	Duration time.Duration
	Code     string
	Success  bool
	Props    map[string]string
	Stream   bool
}

// Dependency records d like Request.
func (c *Client) Dependency(ctx context.Context, d Dependency) {
	if c == nil || !c.Enabled() {
		return
	}
	ms := float64(d.Duration) / float64(time.Millisecond)
	if !c.cfg.Detailed {
		c.agg.duration(KindDependency, d.Name, ms, d.Success, d.Stream)
	}
	if !(c.cfg.Detailed || !d.Success || !d.Stream && d.Duration > c.cfg.SlowRequest) {
		return
	}
	it := Item{Kind: KindDependency, Name: d.Name, Type: d.Type, Target: d.Target, DurationMS: ms, Success: d.Success,
		ResultCode: d.Code, Props: d.Props, Time: time.Now().Add(-d.Duration)}
	if d.Span.Valid() {
		it.ID, it.OperationID, it.ParentID = d.Span.SpanID, d.Span.TraceID, d.Span.ParentID
	}
	c.record(ctx, it)
}

// Tick turns the aggregates of the last period into metric items.
func (c *Client) Tick() {
	if c == nil {
		return
	}
	for _, it := range c.agg.drain() {
		c.record(context.Background(), it)
	}
	c.mu.Lock()
	d := c.dropped
	c.dropped = 0
	c.mu.Unlock()
	if d > 0 {
		c.Metric(MetricDropped, float64(d), map[string]string{"where": "memory"})
	}
}

// MetricDropped counts items dropped because a buffer or queue was full.
const MetricDropped = "telemetry.dropped"

// Run ticks the aggregates once a minute until ctx ends.
func (c *Client) Run(ctx context.Context) {
	if c == nil {
		return
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Tick()
		}
	}
}

// Flush moves the memory buffer to the disk queue.
func (c *Client) Flush() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	buf := c.buf
	c.buf = nil
	c.mu.Unlock()
	if len(buf) == 0 || c.queue == nil {
		return nil
	}
	if _, err := c.queue.Append(buf); err != nil {
		c.mu.Lock()
		c.buf = append(buf, c.buf...) // try again with the next flush
		c.mu.Unlock()
		return err
	}
	return nil
}

// Pending returns and clears the memory buffer (no disk queue).
func (c *Client) Pending() []Item {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.buf
	c.buf = nil
	return b
}

// Close sends the aggregates to the buffer and the buffer to the queue.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.Tick()
	return c.Flush()
}

// SetCollecting applies the hub's answer: off drops the buffer and the
// queue and stops recording until retry has passed.
func (c *Client) SetCollecting(on bool, retry time.Duration) {
	if c == nil {
		return
	}
	if retry <= 0 {
		retry = time.Hour
	}
	c.mu.Lock()
	changed := c.off == on
	c.off = !on
	c.recheck = time.Now().Add(retry)
	if !on {
		c.buf = nil
	}
	c.mu.Unlock()
	if c.cfg.Dir == "" {
		return
	}
	st := State{Collecting: &on, Checked: time.Now().UTC()}
	if !on {
		st.Recheck = c.recheck.UTC()
		if c.queue != nil {
			c.queue.Clear()
		}
	}
	if changed || !on {
		SaveState(c.cfg.Dir, st)
	} else if old, err := LoadState(c.cfg.Dir); err != nil || old.Collecting == nil || time.Since(old.Checked) > time.Hour {
		SaveState(c.cfg.Dir, st)
	}
}

// RecheckDue reports whether the hub said it doesn't collect and it is time
// to ask again.
func (c *Client) RecheckDue() bool {
	if c == nil || c.cfg.Disabled {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.off && time.Now().After(c.recheck)
}

// State is what the hub last said (<dir>/state.json), shared by the
// processes using one dir.
type State struct {
	Collecting *bool     `json:"collecting,omitempty"`
	Checked    time.Time `json:"checked"`
	Recheck    time.Time `json:"recheck,omitzero"`
}

// StateFile is the name of State in a telemetry dir.
const StateFile = "state.json"

// LoadState reads dir's State.
func LoadState(dir string) (State, error) {
	var s State
	b, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// SaveState writes dir's State.
func SaveState(dir string, s State) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(s)
	return writeAtomic(filepath.Join(dir, StateFile), b)
}

func writeAtomic(path string, b []byte) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	var err error
	for range 20 { // Windows: a reader may hold the target for a moment
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	os.Remove(tmp)
	return err
}

// ---------------------------------------------------------------- package-level helpers (the default client)

// Event records an event with the default client.
func Event(ctx context.Context, name string, props map[string]string, meas map[string]float64) {
	Default().Event(ctx, name, props, meas)
}

// Count adds to a per-minute counter of the default client.
func Count(name string, props map[string]string, n float64) { Default().Count(name, props, n) }

// Error records err with the default client.
func Error(ctx context.Context, class string, err error, props map[string]string) {
	Default().Error(ctx, class, err, props)
}

// RecordDependency records d with the default client.
func RecordDependency(ctx context.Context, d Dependency) { Default().Dependency(ctx, d) }

// RecordRequest records r with the default client.
func RecordRequest(ctx context.Context, r Request) { Default().Request(ctx, r) }
