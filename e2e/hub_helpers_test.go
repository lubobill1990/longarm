package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
)

// longarm-hub is built on first use (TestMain builds longarmd/longarm/helper).
var hubBin struct {
	once sync.Once
	path string
	err  error
}

func buildHub(t *testing.T) string {
	t.Helper()
	hubBin.once.Do(func() {
		hubBin.path = filepath.Join(bins.dir, exeName("longarm-hub"))
		if os.Getenv("LONGARM_E2E_BIN_DIR") != "" { // prebuilt
			if _, err := os.Stat(hubBin.path); err != nil {
				hubBin.err = err
			}
			return
		}
		goBin := filepath.Join(runtime.GOROOT(), "bin", exeName("go"))
		if _, err := os.Stat(goBin); err != nil {
			goBin = "go"
		}
		if b, err := exec.Command(goBin, "build", "-o", hubBin.path, "../cmd/longarm-hub").CombinedOutput(); err != nil {
			hubBin.err = fmt.Errorf("build longarm-hub: %v\n%s", err, b)
		}
	})
	if hubBin.err != nil {
		t.Fatal(hubBin.err)
	}
	return hubBin.path
}

// hubEnv is an e2e env (CA, alice/root certs, worker cert w1) plus an
// longarm-hub using the same CA, and any number of longarmd workers.
type hubEnv struct {
	*env
	hubBin   string
	hubDir   string
	cfgPath  string
	port     int // tcp + udp
	httpPort int
	fp       string // CA fingerprint

	hmu     sync.Mutex
	hub     *exec.Cmd
	workers []*hubWorker
}

type hubWorker struct {
	name    string
	exe     string   // longarmd binary (default bins.longarmd)
	args    []string // extra longarmd run arguments
	env     []string // extra environment
	dataDir string
	port    int
	logPath string
	cmd     *exec.Cmd
	exited  chan struct{}
}

// newHubEnv creates the env and the hub config (extra is appended to
// hub.toml), imports the CA and starts the hub.
func newHubEnv(t *testing.T, extra string) *hubEnv {
	t.Helper()
	return newHubEnvBin(t, extra, buildHub(t))
}

// newHubEnvBin is newHubEnv with another longarm-hub binary (an older
// release: TestOldReleaseCompat).
func newHubEnvBin(t *testing.T, extra, hubBinary string) *hubEnv {
	t.Helper()
	e := newEnv(t)
	h := &hubEnv{env: e, hubBin: hubBinary, hubDir: filepath.Join(e.root, "hub"), port: freePort(t), httpPort: freePort(t)}
	h.cfgPath = filepath.Join(e.root, "hub.toml")
	cfg := fmt.Sprintf(`data_dir = %q
listen = "127.0.0.1:%d"
relay_udp = "127.0.0.1:%d"
relay_stun2 = "127.0.0.1:0"   # not port+1: parallel tests hand out neighbouring ports
http = "127.0.0.1:%d"
[heartbeat]
interval = "500ms"
offline = "4s"
[scheduler]
gate_free_gb = 0.0
gate_cpu = 101.0
[web]
insecure_cookie = true
`, filepath.ToSlash(h.hubDir), h.port, h.port, h.httpPort) + extra
	if err := os.WriteFile(h.cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out := h.hubCmd(t, "ca", "import", filepath.Join(e.client, "ca"))
	ca, err := pki.LoadCA(filepath.Join(e.client, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	h.fp = pki.Fingerprint(ca.Cert)
	if !strings.Contains(out, h.fp) {
		t.Fatalf("ca import output: %s", out)
	}
	// alice and root talk to the hub
	for _, d := range []string{e.client, e.admin} {
		os.WriteFile(filepath.Join(d, "hub.toml"), []byte(fmt.Sprintf("address = \"127.0.0.1:%d\"\n", h.port)), 0o600)
	}
	h.startHub(t)
	t.Cleanup(h.stopAll)
	return h
}

func (h *hubEnv) addr() string { return fmt.Sprintf("127.0.0.1:%d", h.port) }

func (h *hubEnv) httpURL() string { return fmt.Sprintf("http://127.0.0.1:%d", h.httpPort) }

// hubCmd runs a longarm-hub subcommand with the test config.
func (h *hubEnv) hubCmd(t *testing.T, args ...string) string {
	t.Helper()
	return h.hubCmdIn(t, "", args...)
}

func (h *hubEnv) hubCmdIn(t *testing.T, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(h.hubBin, append([]string{"--config", h.cfgPath}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("longarm-hub %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

func (h *hubEnv) startHub(t *testing.T, args ...string) {
	t.Helper()
	ep := filepath.Join(h.hubDir, "hub.endpoints")
	os.Remove(ep)
	// longarm remembers a down hub for 30 s (hub-state.json); a test that brings
	// the hub back does not wait that out.
	for _, d := range []string{h.client, h.admin} {
		if d != "" {
			os.Remove(filepath.Join(d, "hub-state.json"))
		}
	}
	logf, err := os.OpenFile(filepath.Join(h.root, "hub.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(h.hubBin, append([]string{"--config", h.cfgPath, "serve"}, args...)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	logf.Close()
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	h.hmu.Lock()
	h.hub = cmd
	h.hmu.Unlock()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(ep); err == nil {
			return
		}
		select {
		case <-exited:
			b, _ := os.ReadFile(filepath.Join(h.root, "hub.log"))
			t.Fatalf("longarm-hub exited during startup:\n%s", b)
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("longarm-hub did not start in time")
		}
	}
}

func (h *hubEnv) killHub() {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	if h.hub != nil {
		h.hub.Process.Kill()
		h.hub.Process.Wait()
		h.hub = nil
	}
}

func (h *hubEnv) stopAll() {
	h.killHub()
	h.hmu.Lock()
	ws := h.workers
	h.hmu.Unlock()
	for _, w := range ws {
		w.kill()
		// Jobs outlive longarmd (their shims, also a finished job's for a moment):
		// end them and wait until they are gone, so TempDir cleanup can
		// remove their logs (Windows keeps a file in use until then).
		killShims(w.dataDir)
	}
	if h.t.Failed() || os.Getenv("LONGARM_E2E_LOGS") == "1" { // LONGARM_E2E_LOGS=1: always (timing analysis)
		b, _ := os.ReadFile(filepath.Join(h.root, "hub.log"))
		h.t.Logf("hub log:\n%s", tail(string(b), 12000))
		for _, w := range ws {
			b, _ := os.ReadFile(w.logPath)
			h.t.Logf("longarmd %s log:\n%s", w.name, tail(string(b), 6000))
		}
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return "...\n" + s[len(s)-n:]
	}
	return s
}

// newWorkerDir prepares a worker data dir with longarmd.toml (extra appended).
func (h *hubEnv) newWorkerDir(t *testing.T, name, extra string) *hubWorker {
	t.Helper()
	w := &hubWorker{name: name, dataDir: filepath.Join(h.root, "w-"+name), port: freePort(t)}
	w.logPath = filepath.Join(h.root, "longarmd-"+name+".log")
	os.MkdirAll(w.dataDir, 0o700)
	toml := fmt.Sprintf("listen = [\"tcp://127.0.0.1:%d\"]\n", w.port) + extra
	if err := os.WriteFile(filepath.Join(w.dataDir, "longarmd.toml"), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	h.hmu.Lock()
	h.workers = append(h.workers, w)
	h.hmu.Unlock()
	return w
}

// start runs longarmd for the worker; waitListen waits for its endpoints file.
func (w *hubWorker) start(t *testing.T, waitListen bool) {
	t.Helper()
	ep := filepath.Join(w.dataDir, "longarmd.endpoints")
	os.Remove(ep)
	logf, err := os.OpenFile(w.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	exe := bins.longarmd
	if w.exe != "" {
		exe = w.exe
	}
	cmd := exec.Command(exe, append([]string{"run", "--data-dir", w.dataDir}, w.args...)...)
	if len(w.env) > 0 {
		cmd.Env = append(os.Environ(), w.env...)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	logf.Close()
	w.cmd, w.exited = cmd, make(chan struct{})
	go func(c chan struct{}) { cmd.Wait(); close(c) }(w.exited)
	if !waitListen {
		return
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(ep); err == nil {
			return
		}
		select {
		case <-w.exited:
			b, _ := os.ReadFile(w.logPath)
			t.Fatalf("longarmd %s exited during startup:\n%s", w.name, b)
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("longarmd %s did not start listening", w.name)
		}
	}
}

func (w *hubWorker) kill() {
	if w.cmd != nil && w.cmd.Process != nil {
		w.cmd.Process.Kill()
		<-w.exited
		w.cmd = nil
	}
	// shims of jobs started by this worker
	killShims(w.dataDir)
}

// longarmdCmd runs a longarmd subcommand (join, status, drain ...).
func longarmdCmd(t *testing.T, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bins.longarmd, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("longarmd %v: %v", args, err)
		}
	}
	return result{out.String(), errb.String(), code}
}

// joinWorker enrolls a worker data dir with a fresh join code (pending ->
// approved by root) without starting it.
func (h *hubEnv) joinWorker(t *testing.T, w *hubWorker, approve bool) {
	t.Helper()
	code := strings.TrimSpace(h.mustLongarm(h.admin, "admin", "join-code", "--role", "worker", "--labels", "pool=e2e,tier=test"))
	r := longarmdCmd(t, "join", h.addr(), code, "--data-dir", w.dataDir, "--name", w.name, "--ca-fingerprint", h.fp, "--no-wait")
	if r.code != 0 || !strings.Contains(r.stdout, "pending approval") || !strings.Contains(r.stdout, "hub CA verified") {
		t.Fatalf("longarmd join: %+v", r)
	}
	if approve {
		if out := h.mustLongarm(h.admin, "admin", "approve", w.name); !strings.Contains(out, "approved") {
			t.Fatalf("approve: %s", out)
		}
	}
}

// waitNode waits until the hub reports cond for node (via longarm admin nodes --json).
func (h *hubEnv) waitNode(t *testing.T, name, what string, cond func(n map[string]any) bool) map[string]any {
	t.Helper()
	var last map[string]any
	waitFor(t, name+" "+what, 30*time.Second, func() bool {
		for _, n := range h.nodes(t) {
			if n["name"] == name {
				last = n
				return cond(n)
			}
		}
		return false
	})
	return last
}

// compactJSON removes insignificant whitespace (protojson randomizes it).
func compactJSON(s string) string {
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}
