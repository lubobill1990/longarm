package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AuditSinks is the [audit] section of longarmd.toml: copies of every audit
// record to places the worker's owner controls and the hub can't reach
// (docs/security.md).
type AuditSinks struct {
	// File is an extra append-only copy (the same chained lines).
	File string `toml:"file"`
	// Syslog is udp://HOST:PORT, tcp://HOST:PORT (RFC 5424, octet-counting
	// framing) or "local" (the local syslog daemon; not on Windows).
	Syslog string `toml:"syslog"`
	// Webhook gets batches of lines by POST (application/x-ndjson); https
	// except to the loopback.
	Webhook string `toml:"webhook"`
	// WebhookTokenFile holds a bearer token for Webhook.
	WebhookTokenFile string `toml:"webhook_token_file"`
}

// Validate checks the syntax (StartSinks reads the token file).
func (c AuditSinks) Validate() error {
	if c.Syslog == "local" {
		if !localSyslogSupported {
			return errors.New(`syslog = "local": no local syslog daemon on this OS; use udp://HOST:PORT or tcp://HOST:PORT`)
		}
	} else if c.Syslog != "" {
		if _, _, err := parseSyslogURL(c.Syslog); err != nil {
			return err
		}
	}
	if c.Webhook != "" {
		u, err := url.Parse(c.Webhook)
		if err != nil || u.Host == "" {
			return fmt.Errorf("webhook %q: not a URL", c.Webhook)
		}
		switch h := u.Hostname(); {
		case u.Scheme == "https":
		case u.Scheme == "http" && (h == "127.0.0.1" || h == "localhost" || h == "::1"):
		default:
			return fmt.Errorf("webhook %q: https:// required (http:// only to 127.0.0.1 / localhost)", c.Webhook)
		}
	} else if c.WebhookTokenFile != "" {
		return errors.New("webhook_token_file without webhook")
	}
	return nil
}

func parseSyslogURL(s string) (network, addr string, err error) {
	u, err := url.Parse(s)
	if err == nil && (u.Scheme == "udp" || u.Scheme == "tcp") && u.Hostname() != "" && u.Port() != "" && (u.Path == "" || u.Path == "/") {
		if _, perr := strconv.ParseUint(u.Port(), 10, 16); perr == nil {
			return u.Scheme, u.Host, nil
		}
	}
	return "", "", fmt.Errorf(`syslog %q: want udp://HOST:PORT, tcp://HOST:PORT or "local"`, s)
}

// Sink queues and delivery; variables for the tests.
var (
	auditQueueLines = 10000    // per sink; beyond, the oldest records are dropped
	auditQueueBytes = 16 << 20 // also bounded by size: records reach 64 KiB
	auditBatch      = 500
	auditLinger     = time.Second // webhook: wait this long for a fuller batch
	auditRetryMin   = 500 * time.Millisecond
	auditRetryMax   = time.Minute
	auditErrEvery   = time.Minute // log a failing sink at most this often
)

// StartSinks starts delivering every record from now on to the sinks of c.
// Call it once, before records are logged concurrently.
// Delivery runs in the background, so a slow or dead sink never delays a
// call; Close flushes.
func (a *Audit) StartSinks(c AuditSinks, log *slog.Logger) error {
	if err := c.Validate(); err != nil {
		return err
	}
	var qs []*sinkQueue
	add := func(name string, s auditSink, linger time.Duration) {
		qs = append(qs, &sinkQueue{name: name, sink: s, linger: linger, audit: a, log: log,
			wake: make(chan struct{}, 1), closing: make(chan struct{}), done: make(chan struct{})})
	}
	if c.File != "" {
		p1, _ := filepath.Abs(c.File)
		p2, _ := filepath.Abs(a.path)
		if strings.EqualFold(filepath.Clean(p1), filepath.Clean(p2)) {
			return fmt.Errorf("file %q is the audit log itself", c.File)
		}
		add("file", &fileSink{path: c.File}, 0)
	}
	switch c.Syslog {
	case "":
	case "local":
		add("syslog", &localSyslogSink{}, 0)
	default:
		network, addr, _ := parseSyslogURL(c.Syslog)
		add("syslog", &netSyslogSink{network: network, addr: addr, host: syslogHostname()}, 0)
	}
	if c.Webhook != "" {
		// no redirects: one could lead the token and the records to plain
		// http or another host; a 3xx is an error like any non-2xx
		w := &webhookSink{url: c.Webhook, client: &http.Client{Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
		if c.WebhookTokenFile != "" {
			b, err := os.ReadFile(c.WebhookTokenFile)
			if err != nil {
				return fmt.Errorf("webhook_token_file: %w", err)
			}
			if w.token = strings.TrimSpace(string(b)); w.token == "" {
				return fmt.Errorf("webhook_token_file %s is empty", c.WebhookTokenFile)
			}
		}
		add("webhook", w, auditLinger)
	}
	if len(qs) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.sinks, a.stopSinks = qs, cancel
	a.mu.Unlock()
	for _, q := range qs {
		q.ctx = ctx
		go q.run()
	}
	return nil
}

// Close stops the sinks after delivering what is queued, giving up after
// timeout (best effort: a sink may be down).
func (a *Audit) Close(timeout time.Duration) {
	if a == nil {
		return
	}
	a.mu.Lock()
	qs, stop := a.sinks, a.stopSinks
	a.sinks = nil
	a.mu.Unlock()
	for _, q := range qs {
		close(q.closing)
	}
	deadline := time.After(timeout)
	for _, q := range qs {
		select {
		case <-q.done:
		case <-deadline:
		}
	}
	if stop != nil {
		stop()
	}
	for _, q := range qs {
		<-q.done
		q.sink.close()
	}
}

// auditSink delivers lines (each ending in '\n') in order. It returns how
// many it delivered; the rest are retried.
type auditSink interface {
	send(ctx context.Context, lines [][]byte) (int, error)
	close()
}

// sinkQueue is the bounded queue and delivery loop of one sink.
type sinkQueue struct {
	name   string
	sink   auditSink
	linger time.Duration
	audit  *Audit
	log    *slog.Logger

	ctx     context.Context // canceled: stop now
	wake    chan struct{}
	closing chan struct{} // closed: flush and stop
	done    chan struct{}

	mu      sync.Mutex
	lines   [][]byte
	bytes   int
	dropped int

	failing   bool
	lastErrAt time.Time
	errs      int // since the last logged error
}

// push queues line, dropping the oldest records when the queue is full.
func (q *sinkQueue) push(line []byte) {
	q.mu.Lock()
	q.lines = append(q.lines, line)
	q.bytes += len(line)
	for len(q.lines) > 1 && (len(q.lines) > auditQueueLines || q.bytes > auditQueueBytes) {
		q.bytes -= len(q.lines[0])
		q.lines[0] = nil
		q.lines = q.lines[1:]
		q.dropped++
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *sinkQueue) queued() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.lines)
}

// take removes up to auditBatch records from the queue.
func (q *sinkQueue) take() [][]byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := min(len(q.lines), auditBatch)
	batch := make([][]byte, n)
	copy(batch, q.lines)
	clear(q.lines[:n])
	q.lines = q.lines[n:]
	for _, l := range batch {
		q.bytes -= len(l)
	}
	return batch
}

func (q *sinkQueue) run() {
	defer close(q.done)
	closing := false
	for {
		if q.queued() == 0 {
			if closing {
				return
			}
			select {
			case <-q.wake:
			case <-q.closing:
				closing = true
			case <-q.ctx.Done():
				return
			}
			continue
		}
		if q.linger > 0 && !closing && q.queued() < auditBatch {
			t := time.NewTimer(q.linger)
		wait:
			for q.queued() < auditBatch {
				select {
				case <-q.wake:
				case <-t.C:
					break wait
				case <-q.closing:
					closing = true
					break wait
				case <-q.ctx.Done():
					t.Stop()
					return
				}
			}
			t.Stop()
		}
		if !q.deliver(q.take()) {
			return
		}
		q.mu.Lock()
		dropped := q.dropped
		q.dropped = 0
		q.mu.Unlock()
		if dropped > 0 {
			// Through the chain, so the gap is on record everywhere.
			q.audit.LogAs("longarmd", "audit/dropped", map[string]any{"sink": q.name, "count": dropped})
		}
	}
}

// deliver sends batch, retrying with backoff; false: stopped.
func (q *sinkQueue) deliver(batch [][]byte) bool {
	backoff := auditRetryMin
	for {
		n, err := q.sink.send(q.ctx, batch)
		batch = batch[n:]
		if err == nil && len(batch) == 0 {
			if q.failing {
				q.failing = false
				q.log.Info("audit sink delivers again", "sink", q.name)
			}
			return true
		}
		if err == nil {
			err = errors.New("partial delivery")
		}
		q.failed(err)
		select {
		case <-time.After(backoff):
		case <-q.ctx.Done():
			return false
		}
		backoff = min(backoff*2, auditRetryMax)
	}
}

// failed logs err, at most once per auditErrEvery.
func (q *sinkQueue) failed(err error) {
	q.failing = true
	q.errs++
	if time.Since(q.lastErrAt) < auditErrEvery || q.ctx.Err() != nil {
		return
	}
	q.log.Warn("audit sink failing; retrying", "sink", q.name, "err", err, "errors", q.errs, "queued", q.queued())
	q.lastErrAt, q.errs = time.Now(), 0
}

// fileSink appends to a file of the owner's (e.g. another disk).
type fileSink struct {
	path string
	f    *os.File
}

func (s *fileSink) send(_ context.Context, lines [][]byte) (int, error) {
	if s.f == nil {
		f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return 0, err
		}
		s.f = f
	}
	if _, err := s.f.Write(bytes.Join(lines, nil)); err != nil {
		s.close() // reopen next time (a remounted disk)
		return 0, err
	}
	return len(lines), nil
}

func (s *fileSink) close() {
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
}

// netSyslogSink sends RFC 5424 messages to a remote syslog server: one
// datagram per record over UDP, octet-counting framing (RFC 6587) over TCP.
type netSyslogSink struct {
	network, addr, host string
	conn                net.Conn
}

// maxSyslogUDP is what fits a datagram.
const maxSyslogUDP = 65000

func (s *netSyslogSink) send(ctx context.Context, lines [][]byte) (int, error) {
	for i, l := range lines {
		if s.conn == nil {
			d := net.Dialer{Timeout: 10 * time.Second}
			c, err := d.DialContext(ctx, s.network, s.addr)
			if err != nil {
				return i, err
			}
			s.conn = c
		}
		msg := syslogMessage(s.host, l)
		if s.network == "udp" && len(msg) > maxSyslogUDP {
			// Can never fit: send what identifies the record in audit.log.
			sum := sha256.Sum256(bytes.TrimSuffix(l, []byte{'\n'}))
			b, _ := json.Marshal(map[string]any{"method": "audit/oversized", "bytes": len(l) - 1, "sha256": hex.EncodeToString(sum[:])})
			msg = syslogMessage(s.host, b)
		}
		if s.network == "tcp" {
			msg = append([]byte(strconv.Itoa(len(msg))+" "), msg...)
		}
		s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := s.conn.Write(msg); err != nil {
			s.close() // reconnect
			return i, err
		}
	}
	return len(lines), nil
}

func (s *netSyslogSink) close() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

// syslogMessage is an RFC 5424 message: facility auth (4), severity info
// (6), APP-NAME longarmd, MSG the JSON record.
func syslogMessage(host string, line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000000Z07:00")
	b := make([]byte, 0, len(line)+100)
	b = fmt.Appendf(b, "<%d>1 %s %s longarmd %d audit - ", 4*8+6, ts, host, os.Getpid())
	return append(b, line...)
}

// syslogHostname is the HOSTNAME field: printable ASCII without spaces, at
// most 255 characters, "-" if unknown.
func syslogHostname() string {
	h, _ := os.Hostname()
	h = strings.Map(func(r rune) rune {
		if r <= ' ' || r > '~' {
			return -1
		}
		return r
	}, h)
	if len(h) > 255 {
		h = h[:255]
	}
	if h == "" {
		return "-"
	}
	return h
}

// localSyslogSink writes to the local syslog daemon (unix).
type localSyslogSink struct{ w localSyslog }

type localSyslog interface {
	Info(string) error
	Close() error
}

func (s *localSyslogSink) send(_ context.Context, lines [][]byte) (int, error) {
	if s.w == nil {
		w, err := dialLocalSyslog()
		if err != nil {
			return 0, err
		}
		s.w = w
	}
	for i, l := range lines {
		if err := s.w.Info(string(bytes.TrimSuffix(l, []byte{'\n'}))); err != nil {
			return i, err
		}
	}
	return len(lines), nil
}

func (s *localSyslogSink) close() {
	if s.w != nil {
		s.w.Close()
		s.w = nil
	}
}

// webhookSink POSTs batches of lines as NDJSON.
type webhookSink struct {
	url, token string
	client     *http.Client
}

func (s *webhookSink) send(ctx context.Context, lines [][]byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(bytes.Join(lines, nil)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return 0, fmt.Errorf("webhook: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // reuse the connection
	return len(lines), nil
}

func (s *webhookSink) close() { s.client.CloseIdleConnections() }
