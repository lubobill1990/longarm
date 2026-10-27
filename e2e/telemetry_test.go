package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAppInsights is an Application Insights ingestion endpoint
// (/v2.1/track) that keeps every envelope and the raw bodies.
type fakeAppInsights struct {
	*httptest.Server
	mu   sync.Mutex
	raw  []string
	envs []map[string]any
}

func newFakeAppInsights(t *testing.T) *fakeAppInsights {
	f := &fakeAppInsights{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var envs []map[string]any
		if r.URL.Path != "/v2.1/track" || json.Unmarshal(b, &envs) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		f.mu.Lock()
		f.raw = append(f.raw, string(b))
		f.envs = append(f.envs, envs...)
		f.mu.Unlock()
		fmt.Fprintf(w, `{"itemsReceived":%d,"itemsAccepted":%d,"errors":[]}`, len(envs), len(envs))
	}))
	t.Cleanup(f.Close)
	return f
}

// find returns the envelopes for which match is true.
func (f *fakeAppInsights) find(match func(baseType, role string, base map[string]any, tags map[string]any) bool) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, e := range f.envs {
		data, _ := e["data"].(map[string]any)
		tags, _ := e["tags"].(map[string]any)
		base, _ := data["baseData"].(map[string]any)
		bt, _ := data["baseType"].(string)
		role, _ := tags["ai.cloud.role"].(string)
		if match(bt, role, base, tags) {
			out = append(out, e)
		}
	}
	return out
}

// TestTelemetry: with [telemetry] on the hub, a worker and a client run a
// job; the hub forwards their telemetry to Application Insights (a fake
// here): the CLI command (request) and the worker's job events share one
// operation id, and no command line or node / user / host name arrives.
func TestTelemetry(t *testing.T) {
	t.Parallel()
	ai := newFakeAppInsights(t)
	h := newHubEnv(t, fmt.Sprintf("[telemetry]\nconnection_string = %q\n", "InstrumentationKey=e2e-ikey;IngestionEndpoint="+ai.URL+"/"))

	const worker = "telemetry-box"
	w := h.newWorkerDir(t, worker, fmt.Sprintf("[hub]\naddress = %q\n", h.addr()))
	h.mustLongarm(h.client, "admin", "issue", "--role", "worker", "--name", worker, "--out", filepath.Join(w.dataDir, "tls"))
	w.env = []string{"LONGARM_TELEMETRY_INTERVAL=1s"}
	w.start(t, true)
	h.waitNode(t, worker, "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	f, err := os.OpenFile(filepath.Join(h.client, "workers.toml"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "\n[[worker]]\nname = %q\naddress = \"127.0.0.1:%d\"\ntransport = \"tcp\"\n", worker, w.port)
	f.Close()

	const marker = "TELEMETRYMARKER7Q"
	r := h.longarm(h.client, "run", worker, "--wait", "--env", marker+"=secret-value-"+marker, "--", bins.helper, "env", marker)
	if r.code != 0 {
		t.Fatalf("run: %+v", r)
	}
	if out := h.mustLongarm(h.client, "logs", worker, strings.TrimSpace(r.stdout)); !strings.Contains(out, "secret-value-"+marker) {
		t.Fatalf("job output: %q", out)
	}
	// an error names its trace id
	r = h.longarm(h.client, "exec", worker, "--", "no-such-program-"+marker)
	if r.code == 0 {
		t.Fatalf("exec of a missing program: %+v", r)
	}
	if (r.code == 255 || r.code == 127) && !strings.Contains(r.stderr, "(trace ") {
		t.Fatalf("no trace id in the error: %q", r.stderr)
	}

	// what is queued is what is sent (commands that had the hub open sent
	// it at exit already): no command line in it
	if show := h.mustLongarm(h.client, "telemetry", "show"); strings.Contains(show, marker) {
		t.Fatalf("telemetry show:\n%s", show)
	}
	st := h.mustLongarm(h.client, "telemetry", "status")
	if !strings.Contains(st, "telemetry  on") || !strings.Contains(st, "<node:") || !strings.Contains(st, "queue") {
		t.Fatalf("telemetry status:\n%s", st)
	}
	if out := h.mustLongarm(h.client, "telemetry", "flush"); !strings.Contains(out, "sent ") {
		t.Fatalf("flush: %s", out)
	}

	// the CLI's "run" request and the worker's job events, one operation
	var op string
	waitFor(t, "the CLI's run request", 60*time.Second, func() bool {
		reqs := ai.find(func(bt, role string, base, _ map[string]any) bool {
			return bt == "RequestData" && role == "longarm" && base["name"] == "run"
		})
		if len(reqs) == 0 {
			return false
		}
		op, _ = reqs[0]["tags"].(map[string]any)["ai.operation.id"].(string)
		return op != ""
	})
	for _, name := range []string{"job.submitted", "job.ended"} {
		waitFor(t, "the worker's "+name+" in the CLI's operation", 60*time.Second, func() bool {
			return len(ai.find(func(bt, role string, base, tags map[string]any) bool {
				return bt == "EventData" && role == "longarmd" && base["name"] == name && tags["ai.operation.id"] == op
			})) > 0
		})
	}
	ev := ai.find(func(bt, role string, base, _ map[string]any) bool { return base["name"] == "job.ended" })[0]
	props := ev["data"].(map[string]any)["baseData"].(map[string]any)["properties"].(map[string]any)
	tags := ev["tags"].(map[string]any)
	if props["exit"] != "ok" || props["state"] != "done" || props["fleet"] == nil || props["role"] != "worker" ||
		!strings.HasPrefix(fmt.Sprint(tags["ai.cloud.roleInstance"]), "<node:") {
		t.Fatalf("job.ended %v", ev)
	}
	waitFor(t, "the worker's process.start", 30*time.Second, func() bool {
		return len(ai.find(func(bt, role string, base, _ map[string]any) bool {
			return bt == "EventData" && role == "longarmd" && base["name"] == "process.start"
		})) > 0
	})

	// nothing personal arrived
	host, _ := os.Hostname()
	ai.mu.Lock()
	all := strings.Join(ai.raw, "\n")
	ai.mu.Unlock()
	for _, leak := range []string{marker, "secret-value", worker, "alice", host} {
		if len(leak) >= 3 && strings.Contains(strings.ToLower(all), strings.ToLower(leak)) {
			i := strings.Index(strings.ToLower(all), strings.ToLower(leak))
			t.Fatalf("Application Insights received %q: ...%s...", leak, all[max(0, i-300):min(len(all), i+200)])
		}
	}
}
