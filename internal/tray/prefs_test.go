package tray

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/telemetry"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestRulesMine: only the user's own workers notify, and a session
// worker's admin worker (same machine) never twice.
func TestRulesMine(t *testing.T) {
	r := NewRules()
	now := time.Now()
	mk := func(name, owner string, admin bool, st string) *pb.PoolWorker {
		return &pb.PoolWorker{Name: name, State: st, Node: &pb.NodeInfo{Name: name, Role: "worker", State: "approved", Owner: owner,
			Hostname: strings.TrimSuffix(name, "-admin"), AdminWorker: admin}}
	}
	v := &pb.PoolView{User: "alice", Workers: []*pb.PoolWorker{
		mk("box", "alice", false, "online"), mk("box-admin", "alice", true, "online"), mk("bobs", "bob", false, "online"), mk("fleet", "", false, "online"),
	}}
	r.Pool(v, now)
	for _, w := range v.Workers {
		w.State = "offline"
	}
	var titles []string
	for _, n := range r.Pool(v, now) {
		titles = append(titles, n.Title)
	}
	if !slices.Equal(titles, []string{"box offline"}) {
		t.Fatalf("notifications: %v", titles)
	}
	// an older daemon (no user): every worker, as before
	r, v.User = NewRules(), ""
	r.Pool(v, now)
	for _, w := range v.Workers {
		w.State = "online"
	}
	if n := r.Pool(v, now); len(n) != 3 {
		t.Fatalf("no user: %+v", n)
	}
}

// TestIconMachines: the icon and tooltip show machines - a paired admin
// worker is no column of its own - and leave out long-offline workers.
func TestIconMachines(t *testing.T) {
	now := time.Now()
	v := &pb.PoolView{Time: timestamppb.New(now), Workers: []*pb.PoolWorker{
		{Name: "box", State: "online", Node: &pb.NodeInfo{Name: "box", Role: "worker", Owner: "a", Companion: "box-admin"}},
		{Name: "box-admin", State: "online", Node: &pb.NodeInfo{Name: "box-admin", Role: "worker", Owner: "a", AdminWorker: true, Companion: "box"}},
		{Name: "old", State: "offline", Node: &pb.NodeInfo{Name: "old", Role: "worker", LastSeen: timestamppb.New(now.Add(-26 * time.Hour))}},
		{Name: "blip", State: "offline", Node: &pb.NodeInfo{Name: "blip", Role: "worker", LastSeen: timestamppb.New(now.Add(-time.Minute))}},
	}}
	if got := IconWorkers(v); len(got) != 2 || got[0].State != "online" || got[1].State != "offline" {
		t.Fatalf("icon: %+v", got)
	}
	if tip := Tooltip(v); strings.Contains(tip, "box-admin") || strings.Contains(tip, "old") || !strings.Contains(tip, "blip: offline") {
		t.Fatalf("tooltip: %q", tip)
	}
}

// TestTraySettings: notification switches persist in tray.json, the
// telemetry switch is `longarm telemetry off|on`, folders are named (never
// a path from the page), the worker starts by its run mode.
func TestTraySettings(t *testing.T) {
	cfg, data := t.TempDir(), t.TempDir()
	s, err := NewServer(Options{LongarmConfigDir: cfg, LongarmdDataDir: data, LongarmExe: "longarm", LongarmdExe: "longarmd", Roles: "client,worker", Version: "1.2.3"}, &fakeDesk{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var opened []string
	var ran [][]string
	s.openURL = func(u string) error { opened = append(opened, u); return nil }
	s.runCmd = func(name string, args ...string) ([]byte, error) {
		ran = append(ran, append([]string{name}, args...))
		if len(args) > 2 && args[len(args)-3] == "diag" {
			os.WriteFile(args[len(args)-1], []byte("zip"), 0o600)
		}
		return []byte("ok"), nil
	}
	mode := "session"
	s.installedWorker = func() (string, longarmd.AdminInstall) { return mode, longarmd.AdminInstall{} }
	call := func(h http.HandlerFunc, body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("POST", "/", strings.NewReader(body)))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if _, got := call(s.getSettings, ""); got["version"] != "1.2.3" || got["mute_workers"] != nil || got["telemetry_disabled"] != false || got["client_logs"] != cfg {
		t.Fatalf("defaults: %v", got)
	}
	if code, got := call(s.setSettings, `{"mute_workers": true, "telemetry_disabled": true}`); code != 200 || got["mute_workers"] != true || got["mute_jobs"] != nil || got["telemetry_disabled"] != true {
		t.Fatalf("set: %d %v", code, got)
	}
	if p := LoadPrefs(cfg); !p.MuteWorkers || p.MuteJobs {
		t.Fatalf("prefs: %+v", p)
	}
	if ts, _ := telemetry.LoadSettings(filepath.Join(cfg, client.TelemetryDir)); !ts.Disabled {
		t.Fatal("telemetry still on")
	}
	call(s.setSettings, `{"mute_jobs": true}`)
	if p := LoadPrefs(cfg); !p.MuteWorkers || !p.MuteJobs {
		t.Fatalf("prefs after a partial change: %+v", p)
	}
	if code, _ := call(s.openFolder, `{"which": "worker-logs"}`); code != 200 || !slices.Equal(opened, []string{data}) {
		t.Fatalf("folder: %d %v", code, opened)
	}
	if code, _ := call(s.openFolder, `{"which": "C:\\Windows"}`); code != 400 {
		t.Fatalf("any folder: %d", code)
	}
	if code, got := call(s.startWorker, `{}`); code != 200 || got["mode"] != "session" || !slices.Equal(ran[len(ran)-1], []string{"longarmd", "session-mode", "start"}) {
		t.Fatalf("start (session): %d %v %v", code, got, ran)
	}
	mode = ""
	if code, _ := call(s.startWorker, `{}`); code != 200 || !slices.Equal(ran[len(ran)-1], []string{"longarmd", "service", "start"}) {
		t.Fatalf("start (service): %d %v", code, ran)
	}
	if code, got := call(s.collectDiag, `{}`); code != 200 || !strings.HasPrefix(got["zip"].(string), filepath.Join(cfg, "diag")) ||
		!slices.Contains(ran[len(ran)-1], "diag") || opened[len(opened)-1] != filepath.Join(cfg, "diag") {
		t.Fatalf("diag: %d %v %v", code, got, ran)
	}
}
