package hub

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pii"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/telemetry"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// telHub runs a hub in-process with [telemetry] pointing at fake (""
// = telemetry off) and returns it with a dialer for certificates.
func telHub(t *testing.T, connString string) (*Hub, func(o pki.IssueOptions) *grpc.ClientConn) {
	t.Helper()
	dir := t.TempDir()
	ca, err := InitCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: dir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "off"}
	cfg.Telemetry.ConnectionString = connString
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	h, err := Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; h.Close(); telemetry.SetDefault(nil) })
	for h.Addr("grpc") == "" {
		time.Sleep(10 * time.Millisecond)
	}
	return h, func(o pki.IssueOptions) *grpc.ClientConn {
		certPEM, keyPEM, err := ca.Issue(o)
		if err != nil {
			t.Fatal(err)
		}
		bdir := filepath.Join(dir, "c-"+o.Name)
		if err := pki.WriteBundle(bdir, ca.CertPEM, certPEM, keyPEM); err != nil {
			t.Fatal(err)
		}
		b, err := pki.LoadBundle(bdir)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
}

func TestTelemetryOff(t *testing.T) {
	_, dial := telHub(t, "")
	c := dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "worker-one"})
	ack, err := pb.NewTelemetryClient(c).Push(context.Background(), &pb.TelemetryBatch{Items: []*pb.TelemetryItem{{Kind: "event", Name: "x"}}})
	if err != nil || ack.Collecting || ack.RetryAfterSeconds <= 0 {
		t.Fatalf("%v %v", ack, err)
	}
}

// TestTelemetryForward: a node's items are re-scrubbed with every name the
// hub knows, stamped with the fleet and the caller's pseudonyms, and
// forwarded to Application Insights.
func TestTelemetryForward(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		io.WriteString(w, `{"itemsReceived":1,"itemsAccepted":1,"errors":[]}`)
	}))
	defer fake.Close()
	telForwardInterval = 50 * time.Millisecond
	defer func() { telForwardInterval = 5 * time.Second }()
	h, dial := telHub(t, "InstrumentationKey=ikey-1;IngestionEndpoint="+fake.URL+"/")

	// carol-desktop is known to the hub (it called), the pushing node isn't
	// sending its name
	carol := dial(pki.IssueOptions{Role: pki.RoleClient, Name: "carol-desktop", User: "carol"})
	if _, err := pb.NewFleetClient(carol).Whoami(context.Background(), &pb.WhoamiRequest{}); err != nil {
		t.Fatal(err)
	}
	bob := dial(pki.IssueOptions{Role: pki.RoleClient, Name: "bob-laptop", User: "bobby"})
	item := &pb.TelemetryItem{Kind: "trace", Name: "", Message: "talking to carol-desktop failed for bob-laptop", Severity: "warning",
		OperationId: "4bf92f3577b34da6a3ce929d0e0e4736", Props: map[string]string{"peer": "carol-desktop", "note": "carol said hi"},
		Context: &pb.TelemetryContext{Component: "longarm", Version: "1.0", Node: "<node:spoofed>", Fleet: "other"}}
	ack, err := pb.NewTelemetryClient(bob).Push(context.Background(), &pb.TelemetryBatch{Items: []*pb.TelemetryItem{item, {Kind: "bogus"}}})
	if err != nil || !ack.Collecting || ack.MaxBatch <= 0 || ack.Dropped != 1 {
		t.Fatalf("%v %v", ack, err)
	}
	var env map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for env == nil {
		if time.Now().After(deadline) {
			t.Fatal("nothing forwarded")
		}
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		for _, b := range bodies {
			var envs []map[string]any
			json.Unmarshal([]byte(b), &envs)
			for _, e := range envs {
				if e["data"].(map[string]any)["baseType"] == "MessageData" && e["tags"].(map[string]any)["ai.cloud.role"] == "longarm" {
					env = e
				}
			}
		}
		mu.Unlock()
	}
	raw, _ := json.Marshal(env)
	for _, leak := range []string{"carol-desktop", "bob-laptop", "bobby", "spoofed"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("forwarded %q: %s", leak, raw)
		}
	}
	scr := pii.New(pii.Options{Salt: pii.FleetSalt(h.ca.Cert.Raw), NoLocal: true})
	tags := env["tags"].(map[string]any)
	props := env["data"].(map[string]any)["baseData"].(map[string]any)["properties"].(map[string]any)
	if tags["ai.cloud.roleInstance"] != scr.Hash(pii.KindNode, "bob-laptop") || tags["ai.user.id"] != scr.Hash(pii.KindUser, "bobby") ||
		tags["ai.operation.id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || props["fleet"] != telemetry.FleetID(h.ca.Cert.Raw) ||
		props["role"] != "client" || props["peer"] != "<redacted>" || !strings.Contains(props["note"].(string), scr.Hash(pii.KindUser, "carol")) {
		t.Fatalf("envelope %s", raw)
	}
	msg := env["data"].(map[string]any)["baseData"].(map[string]any)["message"].(string)
	if msg != "talking to "+scr.Hash(pii.KindNode, "carol-desktop")+" failed for "+scr.Hash(pii.KindNode, "bob-laptop") {
		t.Fatalf("registry name not hashed: %s", raw)
	}
}
