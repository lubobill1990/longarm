package tray

import (
	"bufio"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/localapi"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

const giB = 1 << 30

func poolView() *pb.PoolView {
	now := time.Now()
	return &pb.PoolView{Workers: []*pb.PoolWorker{
		{Name: "w1", State: "online", Source: "hub", Metrics: &pb.MetricsSnapshot{CpuPercent: 25, MemTotalBytes: 128 * giB, MemUsedBytes: 40 * giB,
			Agents: map[string]*pb.AgentStat{"claude": {Sessions: 3}, "copilot": {Sessions: 1}}},
			Link: &pb.WorkerLink{RttMs: 3, Legs: []*pb.LegStatus{{Label: "tcp", Kind: "direct", Up: true, Active: true, RttMs: 3}}}},
		{Name: "w2", State: "online", Source: "direct", Metrics: &pb.MetricsSnapshot{CpuPercent: 95, MemTotalBytes: 64 * giB, MemUsedBytes: 60 * giB},
			Link: &pb.WorkerLink{RttMs: 351, Legs: []*pb.LegStatus{{Label: "longnet relay-udp", Kind: "relay", Up: true, Active: true, RttMs: 351,
				RelaySince: timestamppb.New(now.Add(-6 * time.Minute))}}}},
		{Name: "w3", State: "offline", Error: "connection refused"},
	}}
}

// TestRenderIcon checks the icon's layout (one column per worker, CPU
// and memory bars by level, offline = red cross) and writes PNGs to
// docs/ui-screenshots when LONGARM_SCREENSHOTS=1.
func TestRenderIcon(t *testing.T) {
	cases := map[string]*pb.PoolView{
		"three": poolView(),
		"one":   {Workers: poolView().Workers[:1]},
		"empty": {},
		"ten": func() *pb.PoolView {
			v := &pb.PoolView{}
			for i := 0; i < 10; i++ {
				w := proto.Clone(poolView().Workers[i%3]).(*pb.PoolWorker)
				w.Name = fmt.Sprintf("w%d", i)
				v.Workers = append(v.Workers, w)
			}
			return v
		}(),
	}
	for name, v := range cases {
		img := RenderIcon(IconWorkers(v))
		if img.Bounds().Dx() != 64 || img.Bounds().Dy() != 64 {
			t.Fatalf("%s: size %v", name, img.Bounds())
		}
		if a := img.RGBAAt(0, 0).A; a > 40 {
			t.Fatalf("%s: corner not transparent (rounded): alpha %d", name, a)
		}
		if c := img.RGBAAt(32, 3); c != colBG && c != colBorder {
			// inside the frame, above the bars: background
			if c.A != 255 {
				t.Fatalf("%s: frame not opaque: %v", name, c)
			}
		}
		if os.Getenv("LONGARM_SCREENSHOTS") == "1" {
			dir := filepath.Join("..", "..", "docs", "ui-screenshots")
			os.MkdirAll(dir, 0o755)
			// 4x for the docs (nearest neighbour)
			if err := os.WriteFile(filepath.Join(dir, "tray-icon-"+name+".png"), IconPNG(IconWorkers(v)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// three workers: w1 green bars, w2 red CPU, w3 a red cross
	img := RenderIcon(IconWorkers(poolView()))
	near := func(c, want color.RGBA) bool {
		d := func(a, b uint8) int { x := int(a) - int(b); return x * x }
		return d(c.R, want.R)+d(c.G, want.G)+d(c.B, want.B) < 3*20*20
	}
	colW := (64 - 14 - 10) / 3.0
	bar := func(i, j int, y int) color.RGBA {
		x0 := 7 + float64(i)*(colW+5)
		bw := (colW - 2) / 2
		return img.RGBAAt(int(x0+float64(j)*(bw+2)+bw/2), y)
	}
	if c := bar(0, 0, 54); !near(c, colGreen) { // CPU 25 %
		t.Fatalf("w1 cpu bar %v", c)
	}
	if c := bar(0, 0, 20); !near(c, colTrack) { // above 25 %: track
		t.Fatalf("w1 cpu track %v", c)
	}
	if c := bar(1, 0, 20); !near(c, colRed) { // CPU 95 %
		t.Fatalf("w2 cpu bar %v", c)
	}
	if c := bar(1, 1, 30); !near(c, colRed) { // mem 94 %
		t.Fatalf("w2 mem bar %v", c)
	}
	x0 := 7 + 2*(colW+5)
	if c := img.RGBAAt(int(x0+colW/2), 32); !near(c, colRed) { // the cross' centre
		t.Fatalf("w3 cross %v", c)
	}
}

func TestTooltip(t *testing.T) {
	tip := Tooltip(poolView())
	if !strings.Contains(tip, "w1: CPU 25% MEM 40/128G CC3 CP1 3ms") || !strings.Contains(tip, "w2:") || !strings.Contains(tip, "w3: offline") {
		t.Fatalf("tooltip %q", tip)
	}
	if !strings.Contains(tip, "351ms relay") && len([]rune(tip)) < 127 {
		t.Fatalf("relay not shown: %q", tip)
	}
	if n := len([]rune(tip)); n > 127 {
		t.Fatalf("tooltip too long: %d", n)
	}
	if Tooltip(&pb.PoolView{}) != "Remote Workers" {
		t.Fatal("empty tooltip")
	}
}

func TestRules(t *testing.T) {
	r := NewRules()
	now := time.Now()
	v := poolView()
	notes := r.Pool(v, now)
	// first view: memory of w2 (94 %) and its 6-minute relay are news; states are not
	if len(notes) != 2 || notes[0].Title != "w2 memory low" || notes[1].Title != "w2 link relayed" || !strings.Contains(notes[1].Body, "longnet relay-udp for 6m0s (351 ms)") {
		t.Fatalf("first: %+v", notes)
	}
	if n := r.Pool(v, now); len(n) != 0 {
		t.Fatalf("repeat: %+v", n)
	}
	v.Workers[0].State, v.Workers[0].Error = "offline", "worker went away"
	v.Workers[2].State = "online"
	notes = r.Pool(v, now)
	if len(notes) != 2 || notes[0].Title != "w1 offline" || notes[0].Body != "worker went away" || notes[1].Title != "w3 online" {
		t.Fatalf("offline: %+v", notes)
	}
	v.Workers[0].State = "online"
	v.Workers[2].State = "offline"
	notes = r.Pool(v, now)
	if len(notes) != 2 || notes[0].Title != "w1 online" || notes[1].Title != "w3 offline" {
		t.Fatalf("back: %+v", notes)
	}
	// memory recovers and crosses again; the relay ends and comes back
	v.Workers[1].Metrics.MemUsedBytes = 10 * giB
	v.Workers[1].Link.Legs[0].RelaySince = nil
	r.Pool(v, now)
	v.Workers[1].Metrics.MemUsedBytes = 60 * giB
	v.Workers[1].Link.Legs[0].RelaySince = timestamppb.New(now.Add(-time.Minute))
	notes = r.Pool(v, now)
	if len(notes) != 1 || notes[0].Title != "w2 memory low" {
		t.Fatalf("again: %+v", notes)
	}
	if notes = r.Pool(v, now.Add(5*time.Minute)); len(notes) != 1 || notes[0].Title != "w2 link relayed" {
		t.Fatalf("relay after 5 min: %+v", notes)
	}

	job := func(id string, st pb.JobState, exit int32) *pb.FleetJob {
		return &pb.FleetJob{Worker: "w1", Job: &pb.Job{Spec: &pb.JobSpec{Id: id, Argv: []string{"pnpm", "test"}}, State: st, HasExitCode: exit >= 0, ExitCode: exit}}
	}
	if n := r.Jobs([]*pb.FleetJob{job("a", pb.JobState_JOB_STATE_RUNNING, -1), job("old", pb.JobState_JOB_STATE_DONE, 0)}); len(n) != 0 {
		t.Fatalf("first jobs: %+v", n)
	}
	n := r.Jobs([]*pb.FleetJob{job("a", pb.JobState_JOB_STATE_FAILED, 3), job("old", pb.JobState_JOB_STATE_DONE, 0), job("b", pb.JobState_JOB_STATE_DONE, 0)})
	if len(n) != 2 || n[0].Title != "job failed on w1" || n[0].Body != "exit 3: pnpm test" || n[1].Title != "job done on w1" {
		t.Fatalf("jobs: %+v", n)
	}
}

// fakeLocal serves a local API (gateway) on a real local channel.
func fakeLocal(t *testing.T, dir, addrFile string) {
	t.Helper()
	ln, _, err := localapi.Listen("longarm", dir, filepath.Join(dir, addrFile))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"identity":"client/alice","cookie":%q}`, r.Header.Get("Cookie"))
	})
	mux.HandleFunc("GET /v1/pool/watch", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 2; i++ {
			fmt.Fprintf(w, "event: pool\ndata: {\"n\":%d}\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(200 * time.Millisecond)
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

type fakeDesk struct{ pinned, hidden bool }

func (d *fakeDesk) Hide()            { d.hidden = true }
func (d *fakeDesk) SetPinned(p bool) { d.pinned = p }
func (d *fakeDesk) Pinned() bool     { return d.pinned }

func TestServer(t *testing.T) {
	cfg := t.TempDir()
	fakeLocal(t, cfg, "daemon.addr")
	desk := &fakeDesk{}
	s, err := NewServer(Options{LongarmConfigDir: cfg, LongarmdDataDir: t.TempDir(), LongarmExe: "longarm", LongarmdExe: "longarmd", Roles: "client"}, desk, "")
	if err != nil {
		t.Fatal(err)
	}
	var term [][]string
	var opened []string
	var ran [][]string
	s.term = func(argv []string) error { term = append(term, argv); return nil }
	s.openURL = func(u string) error { opened = append(opened, u); return nil }
	s.runCmd = func(name string, args ...string) ([]byte, error) {
		ran = append(ran, append([]string{name}, args...))
		return []byte("ok"), nil
	}
	go s.Serve()
	defer s.Close()
	base := "http://" + s.Addr()

	plain := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, _ := plain.Get(base + "/tray"); resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp, _ := plain.Get(base + "/tray?token=wrong"); resp.StatusCode != 403 {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", s.URL("/tray"), nil)
	req.Host = "evil.example:" + strings.Split(s.Addr(), ":")[1]
	if resp, _ := plain.Do(req); resp.StatusCode != 403 {
		t.Fatalf("rebinding host: %d", resp.StatusCode)
	}

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	once := s.URL("/tray?tab=jobs")
	resp, err := c.Get(once)
	if err != nil || resp.StatusCode != 200 || resp.Request.URL.RawQuery != "tab=jobs" {
		t.Fatalf("token handshake: %v %v", err, resp)
	}
	resp.Body.Close()
	// the bootstrap code is single-use: worthless to anyone else afterwards
	if resp, _ := plain.Get(once); resp.StatusCode != 403 {
		t.Fatalf("bootstrap code reused: %d", resp.StatusCode)
	}
	// ... but the session that redeemed it may open the link again
	if resp, err := c.Get(once); err != nil || resp.StatusCode != 200 {
		t.Fatalf("reload with the session: %v %v", err, resp)
	}
	get := func(p string) (int, string) {
		resp, err := c.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	post := func(p, ct, body string) (int, string) {
		resp, err := c.Post(base+p, ct, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// The proxy: path stripped, cookie not forwarded.
	if code, body := get("/local/longarm/v1/status"); code != 200 || !strings.Contains(body, `"client/alice"`) || !strings.Contains(body, `"cookie":""`) {
		t.Fatalf("proxy: %d %s", code, body)
	}
	// SSE streams through (flushed per event).
	resp, err = c.Get(base + "/local/longarm/v1/pool/watch")
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data:") {
				first <- sc.Text()
				return
			}
		}
	}()
	select {
	case l := <-first:
		if l != `data: {"n":0}` {
			t.Fatalf("sse %q", l)
		}
	case <-time.After(150 * time.Millisecond * 3):
		t.Fatal("SSE event not flushed through the proxy")
	}
	resp.Body.Close()
	// longarmd not running: a JSON 502
	if code, body := get("/local/longarmd/v1/status"); code != 502 || !strings.Contains(body, "local API unreachable") {
		t.Fatalf("longarmd down: %d %s", code, body)
	}
	// info
	if code, body := get("/trayapi/info"); code != 200 || !strings.Contains(body, `"running":true`) || !strings.Contains(body, `"client":true`) {
		t.Fatalf("info: %d %s", code, body)
	}
	// CSRF: JSON only, same origin only.
	if code, _ := post("/trayapi/pin", "text/plain", `{"pinned":true}`); code != 415 {
		t.Fatalf("non-JSON post: %d", code)
	}
	r2, _ := http.NewRequest("POST", base+"/trayapi/pin", strings.NewReader(`{"pinned":true}`))
	r2.Header.Set("Content-Type", "application/json")
	r2.Header.Set("Origin", "http://evil.example")
	if resp, _ := c.Do(r2); resp.StatusCode != 403 {
		t.Fatalf("cross-origin post: %d", resp.StatusCode)
	}
	// WebSocket upgrades (GETs): our own Origin only - a page on another
	// localhost port is same-site and would carry the cookie.
	for _, origin := range []string{"http://127.0.0.1:1", "http://evil.example", ""} {
		r3, _ := http.NewRequest("GET", base+"/local/longarm/v1/workers/w1/sessions/s1/attach", nil)
		r3.Header.Set("Upgrade", "websocket")
		r3.Header.Set("Connection", "Upgrade")
		if origin != "" {
			r3.Header.Set("Origin", origin)
		}
		if resp, _ := c.Do(r3); resp.StatusCode != 403 {
			t.Fatalf("upgrade from %q: %d", origin, resp.StatusCode)
		}
	}
	if code, body := post("/trayapi/pin", "application/json", `{"pinned":true}`); code != 200 || !desk.pinned || !strings.Contains(body, `"pinned":true`) {
		t.Fatalf("pin: %d %s", code, body)
	}
	post("/trayapi/hide", "application/json", `{}`)
	if !desk.hidden {
		t.Fatal("hide")
	}
	// actions
	if code, _ := post("/trayapi/attach", "application/json", `{"worker":"w1; rm -rf /"}`); code != 400 {
		t.Fatalf("bad worker accepted: %d", code)
	}
	if code, _ := post("/trayapi/attach", "application/json", `{"worker":"w1","session":"dev"}`); code != 200 ||
		strings.Join(term[0], " ") != "longarm --config-dir "+cfg+" attach w1 --session dev" {
		t.Fatalf("attach: %d %v", code, term)
	}
	post("/trayapi/ssh", "application/json", `{"worker":"w1"}`)
	if strings.Join(term[1], " ") != "longarm --config-dir "+cfg+" ssh w1" {
		t.Fatalf("ssh: %v", term)
	}
	if code, _ := post("/trayapi/open", "application/json", `{"url":"file:///etc/passwd"}`); code != 400 {
		t.Fatalf("file url opened: %d", code)
	}
	if code, _ := post("/trayapi/open", "application/json", `{"url":"http://127.0.0.1:3131/"}`); code != 200 || opened[0] != "http://127.0.0.1:3131/" {
		t.Fatalf("open: %d %v", code, opened)
	}
	if code, _ := post("/trayapi/join", "application/json", `{"hub":"h:7443","code":"K7P-Q2M","fingerprint":"ab12","name":"--evil"}`); code != 400 {
		t.Fatalf("bad join name: %d", code)
	}
	if code, _ := post("/trayapi/join", "application/json", `{"hub":"h:7443","code":"K7P-Q2M","fingerprint":"ab12","name":"box1"}`); code != 200 ||
		!strings.HasPrefix(strings.Join(ran[0], " "), "longarmd join h:7443 K7P-Q2M --no-wait --data-dir ") || !strings.HasSuffix(strings.Join(ran[0], " "), "--ca-fingerprint ab12 --name box1") {
		t.Fatalf("join: %d %v", code, ran)
	}
	// the UI itself (embedded build)
	if code, body := get("/tray"); code != 200 || !strings.Contains(body, `id="root"`) {
		t.Fatalf("ui: %d", code)
	}

}

func TestNonLoopbackRefused(t *testing.T) {
	if _, err := NewServer(Options{}, nil, "0.0.0.0:0"); err == nil {
		t.Fatal("listening on all interfaces accepted")
	}
}

// TestLevelsShared: the icon's colours come from levels.json, the same table
// the web UI imports, with the thresholds (green < 60 <= amber <
// 85 <= red); 82 % CPU is amber in the icon as in the popup.
func TestLevelsShared(t *testing.T) {
	for pct, want := range map[float64]color.RGBA{0: colGreen, 59.9: colGreen, 60: colAmber, 82: colAmber, 84.99: colAmber, 85: colRed, 100: colRed, 140: colRed} {
		if got := levelColor(pct); got != want {
			t.Errorf("levelColor(%v) = %v, want %v", pct, got, want)
		}
	}
	img := RenderIcon([]IconWorker{{State: "online", Valid: true, CPU: 82, Mem: 30}})
	colW := 64 - 14.0
	bw := (colW - 2) / 2
	if c := img.RGBAAt(int(7+bw/2), 54); c != colAmber {
		t.Fatalf("82 %% CPU bar is %v, want amber %v", c, colAmber)
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "lib", "format.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "internal/tray/levels.json") {
		t.Fatal("web/src/lib/format.ts no longer takes its levels from internal/tray/levels.json")
	}
}
