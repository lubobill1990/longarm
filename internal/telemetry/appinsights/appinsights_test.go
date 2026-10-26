package appinsights

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/azauth"
	"github.com/lubobill1990/longarm/internal/telemetry"
)

func TestParseConnectionString(t *testing.T) {
	c, err := ParseConnectionString("InstrumentationKey=00000000-1111-2222-3333-444444444444;IngestionEndpoint=https://westeurope-5.in.applicationinsights.azure.com/;LiveEndpoint=https://x/")
	if err != nil || c.IKey != "00000000-1111-2222-3333-444444444444" || c.TrackURL() != "https://westeurope-5.in.applicationinsights.azure.com/v2.1/track" {
		t.Fatalf("%+v %v", c, err)
	}
	if c, err := ParseConnectionString("InstrumentationKey=abc"); err != nil || c.Endpoint != DefaultEndpoint {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{"", "IngestionEndpoint=https://x", "InstrumentationKey=a;IngestionEndpoint=ftp.example"} {
		if _, err := ParseConnectionString(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestTimeSpan(t *testing.T) {
	for ms, want := range map[float64]string{0: "0.00:00:00.0000000", 1234.5: "0.00:00:01.2345000", 90061001: "1.01:01:01.0010000"} {
		if got := timeSpan(ms); got != want {
			t.Errorf("%v: %s, want %s", ms, got, want)
		}
	}
}

// fakeIngestion records envelopes; respond decides the answer per request.
type fakeIngestion struct {
	mu      sync.Mutex
	reqs    []*http.Request
	bodies  [][]map[string]any
	respond func(n int, envs []map[string]any) (int, string)
}

func (f *fakeIngestion) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var envs []map[string]any
	if err := json.Unmarshal(b, &envs); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.bodies = append(f.bodies, envs)
	n := len(f.reqs)
	f.mu.Unlock()
	code, body := 200, ""
	if f.respond != nil {
		code, body = f.respond(n, envs)
	}
	if body == "" {
		body = `{"itemsReceived":` + itoa(len(envs)) + `,"itemsAccepted":` + itoa(len(envs)) + `,"errors":[]}`
	}
	w.WriteHeader(code)
	io.WriteString(w, body)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func items() []telemetry.Item {
	ctx := telemetry.Context{Component: "longarmd", Version: "1.2.3", OS: "linux", Arch: "amd64", Session: "s1", Fleet: "f1",
		Node: "<node:abc123>", User: "<user:def456>", Role: "worker"}
	t0 := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	return []telemetry.Item{
		{Kind: telemetry.KindRequest, Time: t0, Name: "Jobs/Submit", ID: "00f067aa0ba902b7", OperationID: "4bf92f3577b34da6a3ce929d0e0e4736",
			ParentID: "b7ad6b7169203331", DurationMS: 2500, Success: false, ResultCode: "Internal", Context: ctx, Props: map[string]string{"caller_role": "client"}},
		{Kind: telemetry.KindEvent, Time: t0, Name: "job.submitted", OperationID: "4bf92f3577b34da6a3ce929d0e0e4736", ParentID: "00f067aa0ba902b7",
			Context: ctx, Measurements: map[string]float64{"mem_limit_gb": 2}},
		{Kind: telemetry.KindException, Time: t0, Name: "panic", Message: "boom", Severity: telemetry.SevCritical, Context: ctx,
			Stack: []telemetry.Frame{{Function: "main.main", File: "cmd/longarmd/main.go", Line: 3}}},
		{Kind: telemetry.KindTrace, Time: t0, Message: "hub: session ended", Severity: telemetry.SevWarning, Context: ctx},
		{Kind: telemetry.KindDependency, Time: t0, Name: "dial", Type: "dial", Target: "worker", DurationMS: 12, Success: true, Context: ctx},
		{Kind: telemetry.KindMetric, Time: t0, Name: "request.duration_ms", Value: 30, Count: 3, Min: 5, Max: 20, Context: ctx},
	}
}

func TestEnvelopes(t *testing.T) {
	f := &fakeIngestion{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	fw := &Forwarder{Conn: ConnString{IKey: "11111111-2222-3333-4444-555555555555", Endpoint: srv.URL}}
	if _, err := fw.Send(context.Background(), items()); err != nil {
		t.Fatal(err)
	}
	r, envs := f.reqs[0], f.bodies[0]
	if r.URL.Path != "/v2.1/track" || r.Header.Get("Authorization") != "" || r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("request %s %v", r.URL, r.Header)
	}
	want := []string{"RequestData", "EventData", "ExceptionData", "MessageData", "RemoteDependencyData", "MetricData"}
	for i, e := range envs {
		data := e["data"].(map[string]any)
		tags := e["tags"].(map[string]any)
		if data["baseType"] != want[i] || e["iKey"] != "11111111-2222-3333-4444-555555555555" || e["time"] != "2026-10-26T12:00:00.000Z" ||
			!strings.HasPrefix(e["name"].(string), "Microsoft.ApplicationInsights.11111111222233334444555555555555.") {
			t.Fatalf("envelope %d: %v", i, e)
		}
		if tags["ai.cloud.role"] != "longarmd" || tags["ai.application.ver"] != "1.2.3" || tags["ai.cloud.roleInstance"] != "<node:abc123>" ||
			tags["ai.user.id"] != "<user:def456>" {
			t.Fatalf("tags %d: %v", i, tags)
		}
		bd := data["baseData"].(map[string]any)
		if bd["ver"] != 2.0 {
			t.Fatalf("baseData %v", bd)
		}
		if props, ok := bd["properties"].(map[string]any); !ok || props["fleet"] != "f1" || props["role"] != "worker" {
			t.Fatalf("properties %d: %v", i, bd["properties"])
		}
	}
	req := envs[0]["data"].(map[string]any)["baseData"].(map[string]any)
	tags := envs[0]["tags"].(map[string]any)
	if tags["ai.operation.id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || tags["ai.operation.parentId"] != "b7ad6b7169203331" ||
		req["id"] != "00f067aa0ba902b7" || req["duration"] != "0.00:00:02.5000000" || req["success"] != false || req["responseCode"] != "Internal" {
		t.Fatalf("request %v %v", tags, req)
	}
	if envs[1]["tags"].(map[string]any)["ai.operation.parentId"] != "00f067aa0ba902b7" {
		t.Fatal("event not linked to the request")
	}
	ex := envs[2]["data"].(map[string]any)["baseData"].(map[string]any)
	exs := ex["exceptions"].([]any)[0].(map[string]any)
	if exs["typeName"] != "panic" || exs["message"] != "boom" || ex["severityLevel"] != 4.0 ||
		exs["parsedStack"].([]any)[0].(map[string]any)["fileName"] != "cmd/longarmd/main.go" {
		t.Fatalf("exception %v", ex)
	}
	m := envs[5]["data"].(map[string]any)["baseData"].(map[string]any)["metrics"].([]any)[0].(map[string]any)
	if m["value"] != 30.0 || m["count"] != 3.0 || m["max"] != 20.0 {
		t.Fatalf("metric %v", m)
	}
}

func TestBearerToken(t *testing.T) {
	f := &fakeIngestion{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	fw := &Forwarder{Conn: ConnString{IKey: "k", Endpoint: srv.URL}, Tokens: azauth.Static("tok123")}
	if _, err := fw.Send(context.Background(), items()[:1]); err != nil {
		t.Fatal(err)
	}
	if got := f.reqs[0].Header.Get("Authorization"); got != "Bearer tok123" {
		t.Fatalf("Authorization %q", got)
	}
}

func TestRetry(t *testing.T) {
	f := &fakeIngestion{respond: func(n int, envs []map[string]any) (int, string) {
		switch n {
		case 1:
			return 503, "busy"
		case 2: // partial: item 1 retriable, item 2 refused for good
			return 206, `{"itemsReceived":3,"itemsAccepted":1,"errors":[{"index":1,"statusCode":429,"message":"throttled"},{"index":2,"statusCode":400,"message":"bad"}]}`
		}
		return 200, ""
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	fw := &Forwarder{Conn: ConnString{IKey: "k", Endpoint: srv.URL}, MinBackoff: 10 * time.Millisecond, Interval: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { fw.Run(ctx); close(done) }()
	fw.Enqueue(items()[:3])
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.bodies)
		f.mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d requests", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies[0]) != 3 || len(f.bodies[1]) != 3 || len(f.bodies[2]) != 1 ||
		f.bodies[2][0]["data"].(map[string]any)["baseType"] != "EventData" {
		t.Fatalf("batches %d %d %d", len(f.bodies[0]), len(f.bodies[1]), len(f.bodies[2]))
	}
	if st := fw.Stats(); st.Sent != 2 || st.Failed != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestEnqueueBounded(t *testing.T) {
	fw := &Forwarder{Conn: ConnString{IKey: "k", Endpoint: "http://127.0.0.1:1"}, MaxQueue: 4}
	for range 3 {
		fw.Enqueue(items()[:3])
	}
	if st := fw.Stats(); st.Queued != 4 || st.Dropped != 5 {
		t.Fatalf("%+v", st)
	}
}
