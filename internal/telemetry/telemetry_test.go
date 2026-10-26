package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pii"
)

// leaks fails the test when any of words occurs in s.
func leaks(t *testing.T, where, s string, words ...string) {
	t.Helper()
	for _, w := range words {
		if w != "" && strings.Contains(strings.ToLower(s), strings.ToLower(w)) {
			t.Errorf("%s contains %q:\n%s", where, w, s)
		}
	}
}

func testClient(t *testing.T, dir string, mod func(*Config)) *Client {
	t.Helper()
	cfg := Config{Component: "longarm", Version: "1.2.3", Dir: dir, Detailed: true,
		Identity: Identity{CA: []byte("fake-ca-der"), Node: "alice-laptop", User: "alice", Role: "client"}}
	if mod != nil {
		mod(&cfg)
	}
	return New(cfg)
}

func TestScrubBeforeQueue(t *testing.T) {
	dir := t.TempDir()
	c := testClient(t, dir, nil)
	host, _ := os.Hostname()
	if len(host) < 3 {
		host = ""
	}
	home, _ := os.UserHomeDir()
	ctx := WithSpan(context.Background(), NewSpan())
	cmdline := "pnpm test --filter secret-project-x"
	c.Event(ctx, "job.submitted", map[string]string{"argv": cmdline, "command": cmdline, "env": "GH_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123",
		"note": "alice ran it on alice-laptop from 10.20.30.40 at " + host}, nil)
	c.Error(ctx, "grpc.Unknown", fmt.Errorf(`exec: %q: executable file not found; user alice, mail alice@example.com, peer [2001:db8::7]:7701`, cmdline), nil)
	c.Exception(ctx, "panic", "open "+filepath.Join(home, "proj", "x.txt")+": denied", []Frame{{Function: "main.main", File: filepath.Join(home, "src", "longarm", "cmd", "longarm", "main.go"), Line: 12}}, nil)
	log := slog.New(NewHandler(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	SetDefault(c)
	defer SetDefault(nil)
	log.WarnContext(ctx, "exec failed", "cmd", cmdline, "err", errors.New("connect to 192.168.1.20:7701 as alice: refused"), "host", host)
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, QueueFile))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	leaks(t, "queue", s, "alice", "secret-project-x", "pnpm", "ghp_abcdef", "10.20.30.40", "192.168.1.20", "2001:db8", host, home,
		"example.com")
	items, _ := (&Queue{Dir: dir}).Items()
	if len(items) != 4 {
		t.Fatalf("%d items: %s", len(items), s)
	}
	if items[0].Props["argv"] != "<redacted>" || !strings.Contains(items[0].Props["note"], "<user:") || !strings.Contains(items[0].Props["note"], "<node:") {
		t.Fatalf("props %v", items[0].Props)
	}
	for _, it := range items {
		if it.Context.Node == "" || !strings.HasPrefix(it.Context.Node, "<node:") || it.Context.Fleet == "" || it.OperationID == "" {
			t.Fatalf("context %+v op %q", it.Context, it.OperationID)
		}
	}
	if f := items[2].Stack[0].File; f != "cmd/longarm/main.go" {
		t.Fatalf("stack file %q", f)
	}
	if items[3].Kind != KindTrace || items[3].Severity != SevWarning || items[3].Props["cmd"] != "<redacted>" {
		t.Fatalf("trace %+v", items[3])
	}
	// the same name hashes the same with the same fleet salt (the hub's)
	hub := pii.New(pii.Options{Salt: pii.FleetSalt([]byte("fake-ca-der")), NoLocal: true})
	if items[0].Context.Node != hub.Hash(pii.KindNode, "alice-laptop") {
		t.Fatalf("node %s", items[0].Context.Node)
	}
}

func TestSizeCaps(t *testing.T) {
	props := map[string]string{}
	for i := range 100 {
		props[fmt.Sprintf("k%03d", i)] = strings.Repeat("v", 5000)
	}
	it := Item{Kind: KindTrace, Message: strings.Repeat("m", 20000), Props: props}
	Prepare(&it, nil)
	if len(it.Message) > MaxMessage || len(it.Props) != MaxProps || len(it.Props["k000"]) > MaxProp {
		t.Fatalf("message %d props %d prop %d", len(it.Message), len(it.Props), len(it.Props["k000"]))
	}
}

func TestQueueBound(t *testing.T) {
	q := &Queue{Dir: t.TempDir(), MaxItems: 10, MaxBytes: 1 << 20}
	total := 0
	for b := range 5 {
		var items []Item
		for i := range 5 {
			items = append(items, Item{Kind: KindEvent, Name: fmt.Sprintf("e%d-%d", b, i), Context: Context{Component: "longarm"}})
		}
		if _, err := q.Append(items); err != nil {
			t.Fatal(err)
		}
		total += len(items)
	}
	items, err := q.Items()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) > 10 {
		t.Fatalf("%d items queued", len(items))
	}
	kept, dropped := 0, 0.0
	for _, it := range items {
		if it.Name == MetricDropped {
			dropped += it.Value
		} else {
			kept++
		}
	}
	// the newest survive; every lost item is counted (the dropped items
	// themselves can be dropped again: count those as lost events)
	if items[len(items)-1].Name != MetricDropped || kept == 0 || items[len(items)-2].Name != "e4-4" {
		t.Fatalf("queue %+v", items)
	}
	if float64(kept)+dropped < float64(total) {
		t.Fatalf("kept %d + dropped %v < %d", kept, dropped, total)
	}
	// bytes bound
	qb := &Queue{Dir: t.TempDir(), MaxItems: 10000, MaxBytes: 4 << 10}
	for i := range 100 {
		qb.Append([]Item{{Kind: KindTrace, Message: strings.Repeat("x", 200), Name: fmt.Sprint(i)}})
	}
	if _, size := qb.Stat(); size > 4<<10 {
		t.Fatalf("queue is %d bytes", size)
	}
	// Take removes oldest first
	got, _ := qb.Take(2)
	if len(got) != 2 {
		t.Fatalf("took %d", len(got))
	}
	n, _ := qb.Stat()
	rest, _ := qb.Items()
	if n != len(rest) || len(rest) == 0 {
		t.Fatalf("rest %d/%d", n, len(rest))
	}
}

func TestMemoryBufferBound(t *testing.T) {
	c := testClient(t, "", func(c *Config) { c.MaxQueueItems = 5 })
	for i := range 12 {
		c.Event(context.Background(), fmt.Sprint(i), nil, nil)
	}
	c.Tick()
	p := c.Pending()
	if len(p) != 5 || p[len(p)-1].Name != MetricDropped {
		t.Fatalf("%+v", p)
	}
}

func TestOptOut(t *testing.T) {
	dir := t.TempDir()
	c := testClient(t, dir, func(c *Config) { c.Disabled = true })
	c.Event(context.Background(), "x", nil, nil)
	c.Flush()
	if c.Enabled() || len(c.Pending()) != 0 {
		t.Fatal("disabled client recorded")
	}
	if _, err := os.Stat(filepath.Join(dir, QueueFile)); err == nil {
		t.Fatal("disabled client queued")
	}
	if stop := c.WatchCrashes(); stop != nil {
		stop()
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "crash-*")); len(m) != 0 {
		t.Fatal("disabled client watches crashes")
	}

	t.Setenv(EnvOptOut, "0")
	if !EnvDisabled() {
		t.Fatal("LONGARM_TELEMETRY=0 not honored")
	}
	t.Setenv(EnvOptOut, "1")
	if EnvDisabled() {
		t.Fatal("LONGARM_TELEMETRY=1 disables")
	}

	// longarm telemetry off drops the queue
	c = testClient(t, dir, nil)
	c.Event(context.Background(), "x", nil, nil)
	c.Flush()
	if n, _ := c.Queue().Stat(); n != 1 {
		t.Fatalf("queue %d", n)
	}
	if err := SaveSettings(dir, Settings{Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.Queue().Stat(); n != 0 {
		t.Fatal("telemetry off kept the queue")
	}
	if s, _ := LoadSettings(dir); !s.Disabled {
		t.Fatal("setting not saved")
	}

	// the hub doesn't collect: queue gone, nothing recorded, state shared
	c.Event(context.Background(), "y", nil, nil)
	c.Flush()
	c.SetCollecting(false, 50*time.Millisecond)
	if c.Enabled() {
		t.Fatal("enabled after the hub said no")
	}
	if n, _ := c.Queue().Stat(); n != 0 {
		t.Fatal("queue kept")
	}
	c2 := testClient(t, dir, nil)
	if c2.Enabled() {
		t.Fatal("next process ignores the hub's answer")
	}
	time.Sleep(60 * time.Millisecond)
	if !c2.RecheckDue() {
		t.Fatal("no recheck due")
	}
	c2.SetCollecting(true, 0)
	if !c2.Enabled() || !testClient(t, dir, nil).Enabled() {
		t.Fatal("not enabled after the hub said yes")
	}
}

func TestRequestSampling(t *testing.T) {
	var got []Item
	c := New(Config{Component: "longarmd", Sink: func(it []Item) { got = append(got, it...) }})
	ctx := context.Background()
	c.Request(ctx, Request{Name: "Jobs/Submit", Duration: time.Millisecond, Code: "OK", Success: true})
	c.Request(ctx, Request{Name: "Jobs/Submit", Duration: 3 * time.Second, Code: "OK", Success: true})
	c.Request(ctx, Request{Name: "Jobs/Get", Duration: time.Millisecond, Code: "Internal", Success: false})
	c.Request(ctx, Request{Name: "Jobs/Logs", Duration: time.Hour, Code: "OK", Success: true, Stream: true})
	if len(got) != 2 || got[0].DurationMS < 2999 || got[1].ResultCode != "Internal" {
		t.Fatalf("individual requests %+v", got)
	}
	got = nil
	c.Count("rpc.denied", map[string]string{"method": "x"}, 1)
	c.Count("rpc.denied", map[string]string{"method": "x"}, 2)
	c.Tick()
	byName := map[string]Item{}
	for _, it := range got {
		byName[it.Name+"/"+it.Props["name"]] = it
	}
	sub := byName["request.duration_ms/Jobs/Submit"]
	if sub.Count != 2 || sub.Max < 2999 || sub.Props["p50"] == "" {
		t.Fatalf("aggregate %+v", got)
	}
	if d := byName["rpc.denied/"]; d.Value != 3 {
		t.Fatalf("counter %+v", got)
	}
}

func TestTraceparent(t *testing.T) {
	s := NewSpan()
	p, ok := ParseTraceparent(s.Traceparent())
	if !ok || p.TraceID != s.TraceID || p.SpanID != s.SpanID {
		t.Fatalf("%v %+v", ok, p)
	}
	srv := ServerSpan(p, ok)
	if srv.TraceID != s.TraceID || srv.ParentID != s.SpanID || srv.SpanID == s.SpanID {
		t.Fatalf("%+v", srv)
	}
	for _, bad := range []string{"", "00-xyz-abc-01", "00-00000000000000000000000000000000-0000000000000001-01", "ff-" + s.TraceID + "-" + s.SpanID + "-01"} {
		if _, ok := ParseTraceparent(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
	if len(s.Short()) != 8 {
		t.Fatal(s.Short())
	}
}

func TestSlogHandler(t *testing.T) {
	var got []Item
	c := New(Config{Component: "longarmd", Sink: func(it []Item) { got = append(got, it...) }})
	SetDefault(c)
	defer SetDefault(nil)
	var buf bytes.Buffer
	log := slog.New(NewHandler(slog.NewTextHandler(&buf, nil))).With("component", "hub")
	sp := NewSpan()
	ctx := WithSpan(context.Background(), sp)
	log.InfoContext(ctx, "hello")
	if !strings.Contains(buf.String(), "trace="+sp.TraceID) {
		t.Fatalf("no trace id in %q", buf.String())
	}
	for range 15 {
		log.Warn("same warning", "n", 1)
	}
	log.Error("other")
	if len(got) != 11 || got[0].Props["component"] != "hub" || got[10].Severity != SevError {
		t.Fatalf("%d items: %+v", len(got), got)
	}
}

func TestCrashFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	stack := []byte("goroutine 7 [running]:\nmain.work(0x1)\n\t/home/alice/src/longarm/internal/hub/hub.go:42 +0x1d\ncreated by main.main in goroutine 1\n\t/home/alice/src/longarm/cmd/longarm-hub/main.go:9 +0x25\n")
	if err := WriteCrash(dir, "longarmd", "1.0.0", "index out of range for alice", stack); err != nil {
		t.Fatal(err)
	}
	c := testClient(t, dir, nil)
	if n := c.ReportCrashes(); n != 1 {
		t.Fatalf("%d crashes reported", n)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "crash-*")); len(m) != 0 {
		t.Fatal("crash file kept")
	}
	p := c.Pending()
	if len(p) != 1 {
		t.Fatalf("%+v", p)
	}
	it := p[0]
	if it.Kind != KindException || it.Name != "panic" || it.Context.Component != "longarmd" || it.Context.Version != "1.0.0" ||
		len(it.Stack) != 2 || it.Stack[0].Function != "main.work" || it.Stack[0].File != "internal/hub/hub.go" || it.Stack[0].Line != 42 {
		t.Fatalf("%+v", it)
	}
	leaks(t, "crash item", fmt.Sprintf("%+v", it), "alice")
}

// TestCrashOutput crashes a child process (an unrecovered panic in a
// goroutine) with WatchCrashes on; the next client reports it.
func TestCrashOutput(t *testing.T) {
	if d := os.Getenv("TELEMETRY_CRASH_DIR"); d != "" {
		c := New(Config{Component: "longarmd", Version: "9.9.9", Dir: d})
		c.WatchCrashes()
		go func() { panic("kaboom") }()
		time.Sleep(10 * time.Second)
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashOutput$")
	cmd.Env = append(os.Environ(), "TELEMETRY_CRASH_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("child did not crash: %s", out)
	}
	c := testClient(t, dir, nil)
	if n := c.ReportCrashes(); n != 1 {
		m, _ := filepath.Glob(filepath.Join(dir, "*"))
		t.Fatalf("%d crashes reported (%v)", n, m)
	}
	it := c.Pending()[0]
	if it.Name != "panic" || it.Message != "kaboom" || it.Context.Version != "9.9.9" || len(it.Stack) == 0 ||
		!strings.Contains(it.Stack[0].Function, "TestCrashOutput") || !strings.HasPrefix(it.Stack[0].File, "internal/telemetry/") {
		t.Fatalf("%+v", it)
	}
}
