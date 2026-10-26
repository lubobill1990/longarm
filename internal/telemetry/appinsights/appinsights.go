// Package appinsights forwards telemetry items (internal/telemetry) to Azure
// Application Insights' ingestion API (<IngestionEndpoint>/v2.1/track, a
// JSON array of envelopes as the classic SDKs send). longarm-hub runs one
// Forwarder: bounded memory, batches, retries with backoff; Enqueue never
// blocks. With a managed identity (deploy/azure) requests carry a Microsoft
// Entra ID token (audience https://monitor.azure.com/), so the resource can
// have local (instrumentation key) authentication disabled.
package appinsights

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lubobill1990/longarm/internal/azauth"
	"github.com/lubobill1990/longarm/internal/telemetry"
)

// DefaultEndpoint is the global ingestion endpoint (connection strings
// without IngestionEndpoint).
const DefaultEndpoint = "https://dc.services.visualstudio.com"

// ConnString is a parsed connection string.
type ConnString struct {
	IKey     string // InstrumentationKey
	Endpoint string // IngestionEndpoint, no trailing slash
}

// ParseConnectionString parses "InstrumentationKey=...;IngestionEndpoint=...;..."
func ParseConnectionString(s string) (ConnString, error) {
	var c ConnString
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "instrumentationkey":
			c.IKey = strings.TrimSpace(v)
		case "ingestionendpoint":
			c.Endpoint = strings.TrimRight(strings.TrimSpace(v), "/")
		}
	}
	if c.IKey == "" {
		return c, errors.New("connection string without InstrumentationKey")
	}
	if c.Endpoint == "" {
		c.Endpoint = DefaultEndpoint
	}
	if !strings.HasPrefix(c.Endpoint, "https://") && !strings.HasPrefix(c.Endpoint, "http://") {
		return c, fmt.Errorf("IngestionEndpoint %q is not a URL", c.Endpoint)
	}
	return c, nil
}

// TrackURL is where envelopes are posted.
func (c ConnString) TrackURL() string { return c.Endpoint + "/v2.1/track" }

// Envelope is one Application Insights telemetry envelope.
type Envelope struct {
	Name string            `json:"name"`
	Time string            `json:"time"`
	IKey string            `json:"iKey"`
	Tags map[string]string `json:"tags"`
	Data Data              `json:"data"`
}

// Data is an envelope's payload.
type Data struct {
	BaseType string `json:"baseType"`
	BaseData any    `json:"baseData"`
}

// severityLevel of Application Insights (Verbose 0 ... Critical 4).
func severityLevel(s string) int {
	switch s {
	case telemetry.SevVerbose:
		return 0
	case telemetry.SevWarning:
		return 2
	case telemetry.SevError:
		return 3
	case telemetry.SevCritical:
		return 4
	}
	return 1
}

// timeSpan formats d as .NET TimeSpan "d.hh:mm:ss.fffffff".
func timeSpan(ms float64) string {
	d := time.Duration(ms * float64(time.Millisecond))
	if d < 0 {
		d = 0
	}
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	d -= s * time.Second
	return fmt.Sprintf("%d.%02d:%02d:%02d.%07d", days, h, m, s, d/100)
}

// ToEnvelope converts an item.
func ToEnvelope(it telemetry.Item, ikey string) Envelope {
	c := it.Context
	tags := map[string]string{
		"ai.cloud.role":          c.Component,
		"ai.application.ver":     c.Version,
		"ai.internal.sdkVersion": "longarm:" + c.Version,
	}
	set := func(k, v string) {
		if v != "" {
			tags[k] = v
		}
	}
	set("ai.cloud.roleInstance", c.Node)
	set("ai.session.id", c.Session)
	set("ai.user.id", c.User)
	set("ai.device.osVersion", strings.Trim(c.OS+"/"+c.Arch, "/"))
	set("ai.operation.id", it.OperationID)
	set("ai.operation.parentId", it.ParentID)
	props := maps.Clone(it.Props)
	if props == nil {
		props = map[string]string{}
	}
	for k, v := range map[string]string{"fleet": c.Fleet, "role": c.Role, "os": c.OS, "arch": c.Arch, "component": c.Component} {
		if v != "" {
			props[k] = v
		}
	}
	var typ string
	var base any
	switch it.Kind {
	case telemetry.KindRequest:
		set("ai.operation.name", it.Name)
		typ = "Request"
		base = map[string]any{"ver": 2, "id": nonEmpty(it.ID, "0"), "name": it.Name, "duration": timeSpan(it.DurationMS),
			"responseCode": nonEmpty(it.ResultCode, "0"), "success": it.Success, "properties": props, "measurements": it.Measurements}
	case telemetry.KindDependency:
		typ = "RemoteDependency"
		base = map[string]any{"ver": 2, "id": it.ID, "name": it.Name, "duration": timeSpan(it.DurationMS), "resultCode": it.ResultCode,
			"success": it.Success, "type": it.Type, "target": it.Target, "data": it.Name, "properties": props, "measurements": it.Measurements}
	case telemetry.KindException:
		var frames []map[string]any
		for i, f := range it.Stack {
			frames = append(frames, map[string]any{"level": i, "method": f.Function, "assembly": c.Component, "fileName": f.File, "line": f.Line})
		}
		ex := map[string]any{"id": 1, "outerId": 0, "typeName": nonEmpty(it.Name, "error"), "message": nonEmpty(it.Message, it.Name),
			"hasFullStack": len(frames) > 0}
		if len(frames) > 0 {
			ex["parsedStack"] = frames
		}
		typ = "Exception"
		base = map[string]any{"ver": 2, "exceptions": []any{ex}, "severityLevel": severityLevel(it.Severity), "properties": props,
			"measurements": it.Measurements}
	case telemetry.KindTrace:
		typ = "Message"
		base = map[string]any{"ver": 2, "message": it.Message, "severityLevel": severityLevel(it.Severity), "properties": props}
	case telemetry.KindMetric:
		m := map[string]any{"name": it.Name, "value": it.Value}
		if it.Count > 1 {
			m["kind"], m["count"], m["min"], m["max"] = 1, it.Count, it.Min, it.Max
		} else {
			m["kind"] = 0
		}
		typ = "Metric"
		base = map[string]any{"ver": 2, "metrics": []any{m}, "properties": props}
	default: // event
		typ = "Event"
		base = map[string]any{"ver": 2, "name": nonEmpty(it.Name, it.Kind), "properties": props, "measurements": it.Measurements}
	}
	t := it.Time
	if t.IsZero() {
		t = time.Now()
	}
	return Envelope{
		Name: "Microsoft.ApplicationInsights." + strings.ReplaceAll(ikey, "-", "") + "." + typ,
		Time: t.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
		IKey: ikey,
		Tags: tags,
		Data: Data{BaseType: typ + "Data", BaseData: base},
	}
}

func nonEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Forwarder sends items to Application Insights.
type Forwarder struct {
	Conn ConnString
	// Tokens, when set, authenticates with Entra ID (ResourceMonitor);
	// otherwise the instrumentation key alone does (local auth).
	Tokens    azauth.TokenSource
	HTTP      *http.Client
	Log       *slog.Logger
	MaxQueue  int           // items in memory (default 10000)
	BatchSize int           // items per request (default 500)
	Interval  time.Duration // longest wait before a partial batch goes (default 5 s)
	// MaxAttempts per batch for retriable failures (default 6).
	MaxAttempts int
	// MinBackoff is the first retry delay (default 1 s).
	MinBackoff time.Duration

	mu    sync.Mutex
	q     []telemetry.Item
	wake  chan struct{}
	once  sync.Once
	sent  atomic.Int64
	drops atomic.Int64
	fails atomic.Int64
}

func (f *Forwarder) init() {
	f.once.Do(func() {
		if f.MaxQueue <= 0 {
			f.MaxQueue = 10000
		}
		if f.BatchSize <= 0 {
			f.BatchSize = 500
		}
		if f.Interval <= 0 {
			f.Interval = 5 * time.Second
		}
		if f.MaxAttempts <= 0 {
			f.MaxAttempts = 6
		}
		if f.MinBackoff <= 0 {
			f.MinBackoff = time.Second
		}
		if f.HTTP == nil {
			f.HTTP = &http.Client{Timeout: 30 * time.Second}
		}
		if f.Log == nil {
			f.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
		}
		f.wake = make(chan struct{}, 1)
	})
}

// Enqueue queues items; it never blocks. When the queue is full the oldest
// items are dropped (Stats().Dropped).
func (f *Forwarder) Enqueue(items []telemetry.Item) {
	f.init()
	f.mu.Lock()
	f.q = append(f.q, items...)
	if over := len(f.q) - f.MaxQueue; over > 0 {
		f.q = append(f.q[:0:0], f.q[over:]...)
		f.drops.Add(int64(over))
	}
	full := len(f.q) >= f.BatchSize
	f.mu.Unlock()
	if full {
		select {
		case f.wake <- struct{}{}:
		default:
		}
	}
}

// Stats are the forwarder's counters.
type Stats struct {
	Queued, Sent, Dropped, Failed int64
}

// Stats returns the counters.
func (f *Forwarder) Stats() Stats {
	f.init()
	f.mu.Lock()
	n := len(f.q)
	f.mu.Unlock()
	return Stats{Queued: int64(n), Sent: f.sent.Load(), Dropped: f.drops.Load(), Failed: f.fails.Load()}
}

func (f *Forwarder) take() []telemetry.Item {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := min(len(f.q), f.BatchSize)
	b := append([]telemetry.Item(nil), f.q[:n]...)
	f.q = append(f.q[:0:0], f.q[n:]...)
	return b
}

// Run sends batches until ctx ends, then tries to send what is left
// within a few seconds.
func (f *Forwarder) Run(ctx context.Context) {
	f.init()
	t := time.NewTicker(f.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			f.Flush(fctx)
			cancel()
			return
		case <-t.C:
		case <-f.wake:
		}
		f.drain(ctx)
	}
}

// drain sends batches until the queue is empty or a batch gave up.
func (f *Forwarder) drain(ctx context.Context) {
	for ctx.Err() == nil {
		b := f.take()
		if len(b) == 0 {
			return
		}
		if !f.sendRetry(ctx, b) {
			return
		}
	}
}

// Flush sends everything queued (one attempt per batch).
func (f *Forwarder) Flush(ctx context.Context) error {
	f.init()
	for ctx.Err() == nil {
		b := f.take()
		if len(b) == 0 {
			return nil
		}
		if _, err := f.Send(ctx, b); err != nil {
			f.fails.Add(int64(len(b)))
			return err
		}
	}
	return ctx.Err()
}

// sendRetry sends b, retrying retriable failures with backoff; false when
// it gave up (the items are dropped).
func (f *Forwarder) sendRetry(ctx context.Context, b []telemetry.Item) bool {
	wait := f.MinBackoff
	for attempt := 1; ; attempt++ {
		retry, err := f.Send(ctx, b)
		if err == nil && len(retry) == 0 {
			return true
		}
		if len(retry) == 0 { // not retriable
			f.Log.Warn("telemetry: Application Insights refused a batch", "items", len(b), "err", err)
			f.fails.Add(int64(len(b)))
			return true
		}
		if attempt >= f.MaxAttempts || ctx.Err() != nil {
			f.Log.Warn("telemetry: giving up on a batch", "items", len(retry), "attempts", attempt, "err", err)
			f.fails.Add(int64(len(retry)))
			return false
		}
		var ra *retryAfter
		if errors.As(err, &ra) && ra.d > wait {
			wait = ra.d
		}
		select {
		case <-ctx.Done():
			f.fails.Add(int64(len(retry)))
			return false
		case <-time.After(wait):
		}
		b = retry
		if wait *= 2; wait > time.Minute {
			wait = time.Minute
		}
	}
}

type retryAfter struct {
	d   time.Duration
	err error
}

func (r *retryAfter) Error() string { return r.err.Error() }
func (r *retryAfter) Unwrap() error { return r.err }

// retriable HTTP status codes (whole request or per item).
func retriable(code int) bool {
	switch code {
	case 408, 429, 439, 500, 502, 503, 504:
		return true
	}
	return false
}

// trackResponse is the ingestion API's answer.
type trackResponse struct {
	ItemsReceived int `json:"itemsReceived"`
	ItemsAccepted int `json:"itemsAccepted"`
	Errors        []struct {
		Index      int    `json:"index"`
		StatusCode int    `json:"statusCode"`
		Message    string `json:"message"`
	} `json:"errors"`
}

// Send posts one batch. It returns the items worth retrying (all on a
// network error or a retriable status, the retriable ones of a partial
// success) and an error when not everything was accepted.
func (f *Forwarder) Send(ctx context.Context, items []telemetry.Item) ([]telemetry.Item, error) {
	f.init()
	envs := make([]Envelope, len(items))
	for i, it := range items {
		envs[i] = ToEnvelope(it, f.Conn.IKey)
	}
	body, err := json.Marshal(envs)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.Conn.TrackURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if f.Tokens != nil {
		tok, err := f.Tokens.Token(ctx, azauth.ResourceMonitor)
		if err != nil {
			return items, fmt.Errorf("token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return items, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr trackResponse
	json.Unmarshal(rb, &tr)
	switch {
	case resp.StatusCode == http.StatusOK:
		f.sent.Add(int64(len(items)))
		return nil, nil
	case resp.StatusCode == http.StatusPartialContent || resp.StatusCode == http.StatusBadRequest && len(tr.Errors) > 0:
		var retry []telemetry.Item
		for _, e := range tr.Errors {
			if e.Index >= 0 && e.Index < len(items) && retriable(e.StatusCode) {
				retry = append(retry, items[e.Index])
			}
		}
		f.sent.Add(int64(tr.ItemsAccepted))
		f.fails.Add(int64(len(tr.Errors) - len(retry)))
		return retry, fmt.Errorf("%d of %d items accepted", tr.ItemsAccepted, len(items))
	case retriable(resp.StatusCode) || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		err := fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(rb)))
		if s := resp.Header.Get("Retry-After"); s != "" {
			if n, perr := strconv.Atoi(s); perr == nil {
				return items, &retryAfter{d: time.Duration(n) * time.Second, err: err}
			}
		}
		return items, err
	}
	return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(rb)))
}
