package server

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fastSinks shortens the sinks' timing for a test.
func fastSinks(t *testing.T) {
	linger, retry := auditLinger, auditRetryMin
	auditLinger, auditRetryMin = 50*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { auditLinger, auditRetryMin = linger, retry })
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestAuditSinksValidate(t *testing.T) {
	for _, c := range []struct {
		cfg AuditSinks
		err string
	}{
		{AuditSinks{Syslog: "udp://logs.example.com:514", Webhook: "https://audit.example.com/x", WebhookTokenFile: "/t"}, ""},
		{AuditSinks{Syslog: "tcp://[::1]:6514", Webhook: "http://127.0.0.1:8080/"}, ""},
		{AuditSinks{Webhook: "http://localhost/x"}, ""},
		{AuditSinks{Syslog: "logs.example.com:514"}, "want udp://HOST:PORT"},
		{AuditSinks{Syslog: "udp://logs.example.com"}, "want udp://HOST:PORT"},
		{AuditSinks{Syslog: "http://logs.example.com:514"}, "want udp://HOST:PORT"},
		{AuditSinks{Webhook: "http://audit.example.com/"}, "https:// required"},
		{AuditSinks{Webhook: "audit.example.com"}, "not a URL"},
		{AuditSinks{WebhookTokenFile: "/t"}, "without webhook"},
	} {
		err := c.cfg.Validate()
		if (c.err == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), c.err) {
			t.Errorf("%+v: %v, want %q", c.cfg, err, c.err)
		}
	}
	err := AuditSinks{Syslog: "local"}.Validate()
	if localSyslogSupported != (err == nil) {
		t.Errorf("local syslog: %v", err)
	}
}

func TestAuditWebhook(t *testing.T) {
	fastSinks(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "token"), []byte("s3cret\n"), 0o600)
	var mu sync.Mutex
	var got []string
	calls, maxBatch := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.Header.Get("Authorization") != "Bearer s3cret" || r.Header.Get("Content-Type") != "application/x-ndjson" {
			t.Errorf("headers %v", r.Header)
		}
		if calls <= 2 {
			http.Error(w, "busy", http.StatusInternalServerError)
			return
		}
		b, _ := io.ReadAll(r.Body)
		lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		maxBatch = max(maxBatch, len(lines))
		got = append(got, lines...)
	}))
	defer srv.Close()

	path := filepath.Join(dir, "audit.log")
	a := NewAudit(path)
	if err := a.StartSinks(AuditSinks{Webhook: srv.URL, WebhookTokenFile: filepath.Join(dir, "token")}, discardLog); err != nil {
		t.Fatal(err)
	}
	for i := range 1200 {
		a.LogAs("x", "test", map[string]any{"i": i})
	}
	waitUntil(t, "1200 records", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) >= 1200 })
	a.Close(time.Second)
	mu.Lock()
	defer mu.Unlock()
	if maxBatch > auditBatch {
		t.Errorf("batch of %d", maxBatch)
	}
	// In order, without gaps, the very lines of audit.log.
	if want := readAuditLines(t, path); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("webhook got %d lines, audit.log has %d (or different)", len(got), len(want))
	}
}

func TestAuditSinkDropsOldest(t *testing.T) {
	fastSinks(t)
	lines := auditQueueLines
	auditQueueLines = 10
	t.Cleanup(func() { auditQueueLines = lines })
	release := make(chan struct{})
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
			got = append(got, record(t, l))
		}
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "audit.log")
	a := NewAudit(path)
	if err := a.StartSinks(AuditSinks{Webhook: srv.URL}, discardLog); err != nil {
		t.Fatal(err)
	}
	a.LogAs("x", "test", nil)
	waitUntil(t, "the first batch in flight", func() bool { return a.sinks[0].queued() == 0 })
	for range 49 {
		a.LogAs("x", "test", nil)
	}
	close(release)
	// Every record either arrived or is counted in an audit/dropped record.
	count := func() (normal, dropped int) {
		mu.Lock()
		defer mu.Unlock()
		for _, r := range got {
			if r["method"] == "audit/dropped" {
				if r["sink"] != "webhook" {
					t.Errorf("dropped record %v", r)
				}
				dropped += int(r["count"].(float64))
			} else {
				normal++
			}
		}
		return
	}
	waitUntil(t, "all records accounted for", func() bool { n, d := count(); return n+d >= 50 && a.sinks[0].queued() == 0 })
	n, d := count()
	if d < 39 || n+d != 50 {
		t.Fatalf("%d arrived, %d dropped", n, d)
	}
	// The newest records survived, the dropped records are chained locally.
	mu.Lock()
	last := got[len(got)-1]
	mu.Unlock()
	if v := verifyFile(t, path); v.Broken != nil || last["seq"].(float64) > float64(v.Records) {
		t.Fatalf("%+v, last %v", v, last)
	}
	a.Close(time.Second)
}

func TestAuditSyslogUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	path := filepath.Join(t.TempDir(), "audit.log")
	a := NewAudit(path)
	if err := a.StartSinks(AuditSinks{Syslog: "udp://" + pc.LocalAddr().String()}, discardLog); err != nil {
		t.Fatal(err)
	}
	defer a.Close(time.Second)
	for range 3 {
		a.LogAs("x", "test", nil)
	}
	want := readAuditLines(t, path)
	buf := make([]byte, 70000)
	pc.SetReadDeadline(time.Now().Add(10 * time.Second))
	for i := range 3 {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			t.Fatal(err)
		}
		checkSyslog(t, string(buf[:n]), want[i])
	}
}

func checkSyslog(t *testing.T, msg, line string) {
	t.Helper()
	// <PRI>1 TIMESTAMP HOSTNAME APP-NAME PROCID MSGID SD MSG
	f := strings.SplitN(msg, " ", 8)
	if len(f) != 8 || f[0] != "<38>1" || f[3] != "longarmd" || f[4] != strconv.Itoa(os.Getpid()) || f[6] != "-" || f[7] != line {
		t.Fatalf("syslog message %q, want MSG %q", msg, line)
	}
	if _, err := time.Parse(time.RFC3339Nano, f[1]); err != nil {
		t.Fatalf("timestamp: %v", err)
	}
}

func TestAuditSyslogTCP(t *testing.T) {
	fastSinks(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	path := filepath.Join(t.TempDir(), "audit.log")
	a := NewAudit(path)
	if err := a.StartSinks(AuditSinks{Syslog: "tcp://" + ln.Addr().String()}, discardLog); err != nil {
		t.Fatal(err)
	}
	defer a.Close(time.Second)
	for range 3 {
		a.LogAs("x", "test", map[string]any{"text": "with spaces\nand a newline"})
	}
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	want := readAuditLines(t, path)
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for i := range 3 { // octet counting: LEN SP MSG
		n, err := br.ReadString(' ')
		if err != nil {
			t.Fatal(err)
		}
		size, err := strconv.Atoi(strings.TrimSuffix(n, " "))
		if err != nil {
			t.Fatalf("frame length %q", n)
		}
		msg := make([]byte, size)
		if _, err := io.ReadFull(br, msg); err != nil {
			t.Fatal(err)
		}
		checkSyslog(t, string(msg), want[i])
	}
	// The sink reconnects when the server drops the connection.
	c.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	for {
		a.LogAs("x", "test", nil)
		select {
		case c := <-accepted:
			c.Close()
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestAuditFileSink(t *testing.T) {
	dir := t.TempDir()
	path, copyPath := filepath.Join(dir, "audit.log"), filepath.Join(dir, "copy.log")
	a := NewAudit(path)
	if err := a.StartSinks(AuditSinks{File: path}, discardLog); err == nil {
		t.Fatal("the audit log as its own sink")
	}
	if err := a.StartSinks(AuditSinks{File: copyPath}, discardLog); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		a.LogAs("x", "test", nil)
	}
	a.Close(2 * time.Second)
	b1, _ := os.ReadFile(path)
	b2, _ := os.ReadFile(copyPath)
	if string(b1) != string(b2) {
		t.Fatalf("copy differs:\n%s\n%s", b1, b2)
	}
	if v := verifyFile(t, copyPath); v.Broken != nil || v.Records != 5 {
		t.Fatalf("%+v", v)
	}
}

// Sinks never block a call, also when one hangs; Close gives up in time.
func TestAuditSinkHangingDoesNotBlock(t *testing.T) {
	fastSinks(t)
	lines := auditQueueLines
	auditQueueLines = 100
	t.Cleanup(func() { auditQueueLines = lines })
	hung := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hung <- struct{}{}:
		default:
		}
		io.ReadAll(r.Body) // then the server notices the client giving up
		<-r.Context().Done()
	}))
	defer srv.Close()
	a := NewAudit(filepath.Join(t.TempDir(), "audit.log"))
	if err := a.StartSinks(AuditSinks{Webhook: srv.URL}, discardLog); err != nil {
		t.Fatal(err)
	}
	a.LogAs("x", "test", nil)
	<-hung
	done := make(chan struct{})
	go func() {
		for range 300 { // beyond the queue: drops instead of waiting
			a.LogAs("x", "test", nil)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("Log blocked on a hanging sink")
	}
	start := time.Now()
	a.Close(100 * time.Millisecond)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Close took %v", d)
	}
}
