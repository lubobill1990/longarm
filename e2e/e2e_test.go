// Package e2e runs real longarmd / longarm binaries against each other over the tcp
// transport on 127.0.0.1 with a temporary data dir.
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/platform"
)

var bins struct {
	dir                       string
	longarmd, longarm, helper string
	buildErr                  error
}

func exeName(s string) string {
	if runtime.GOOS == "windows" {
		return s + ".exe"
	}
	return s
}

func TestMain(m *testing.M) {
	// Client-side placement must not depend on this machine's load.
	os.Setenv("LONGARM_GATE_CPU", "101")
	os.Setenv("LONGARM_GATE_FREE_GB", "0")
	// LONGARM_E2E_BIN_DIR: prebuilt longarmd, longarm, helper and longarm-hub
	// (CI runs the per-user tests as root from a test binary and must not
	// build with root's - empty - Go caches).
	prebuilt := os.Getenv("LONGARM_E2E_BIN_DIR")
	dir := prebuilt
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "longarm-e2e-bin-"); err != nil {
			panic(err)
		}
	}
	bins.dir = dir
	bins.longarmd, bins.longarm, bins.helper = filepath.Join(dir, exeName("longarmd")), filepath.Join(dir, exeName("longarm")), filepath.Join(dir, exeName("helper"))
	goBin := filepath.Join(runtime.GOROOT(), "bin", exeName("go"))
	if _, err := os.Stat(goBin); err != nil {
		goBin = "go"
	}
	for pkg, out := range map[string]string{"../cmd/longarmd": bins.longarmd, "../cmd/longarm": bins.longarm, "./helper": bins.helper} {
		if prebuilt != "" {
			if _, err := os.Stat(out); err != nil {
				bins.buildErr = fmt.Errorf("LONGARM_E2E_BIN_DIR: %v", err)
			}
			continue
		}
		if b, err := exec.Command(goBin, "build", "-o", out, pkg).CombinedOutput(); err != nil {
			bins.buildErr = fmt.Errorf("build %s: %v\n%s", pkg, err, b)
			break
		}
	}
	code := m.Run()
	if prebuilt == "" {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

// env is one longarmd plus client config dirs.
type env struct {
	t        *testing.T
	root     string
	dataDir  string
	port     int
	client   string // config dir with a client cert
	admin    string // config dir with an admin cert
	stranger string // config dir with a client cert from another CA

	longarmdSysProcAttr *syscall.SysProcAttr // optional, for startLongarmd

	mu       sync.Mutex
	longarmd *exec.Cmd
}

// freePort returns a port that is free on 127.0.0.1 for TCP and UDP (the
// hub's relay uses both). The caller binds it later, so it must not come
// back from the kernel's ephemeral allocator to another parallel test or
// outgoing connection in the meantime (that broke TestTransportRace on a
// busy Linux runner): ports are drawn at random below the ephemeral ranges
// (Linux 32768+, Windows 49152+) and never handed out twice per process.
func freePort(t *testing.T) int {
	t.Helper()
	portsMu.Lock()
	defer portsMu.Unlock()
	for range 1000 {
		p := 20000 + mrand.IntN(12000)
		if portsUsed[p] {
			continue
		}
		addr := fmt.Sprintf("127.0.0.1:%d", p)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		u, err := net.ListenPacket("udp", addr)
		l.Close()
		if err != nil {
			continue
		}
		u.Close()
		portsUsed[p] = true
		return p
	}
	t.Fatal("no free port in 20000-31999")
	return 0
}

var (
	portsMu   sync.Mutex
	portsUsed = map[int]bool{}
)

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvAt(t, t.TempDir())
}

// newEnvAt creates CA, certificates and client config dirs under root.
func newEnvAt(t *testing.T, root string) *env {
	t.Helper()
	if bins.buildErr != nil {
		t.Fatal(bins.buildErr)
	}
	e := &env{
		t: t, root: root, dataDir: filepath.Join(root, "longarmd"), port: freePort(t),
		client: filepath.Join(root, "client"), admin: filepath.Join(root, "admin"), stranger: filepath.Join(root, "stranger"),
	}
	e.mustLongarm(e.client, "admin", "ca", "init")
	e.mustLongarm(e.client, "admin", "issue", "--role", "client", "--name", "alice", "--install")
	e.mustLongarm(e.client, "admin", "issue", "--role", "admin", "--name", "root", "--out", filepath.Join(e.admin, "tls"))
	e.mustLongarm(e.client, "admin", "issue", "--role", "worker", "--name", "w1", "--out", filepath.Join(e.dataDir, "tls"))
	e.mustLongarm(e.stranger, "admin", "ca", "init")
	e.mustLongarm(e.stranger, "admin", "issue", "--role", "admin", "--name", "mallory", "--install")
	workers := fmt.Sprintf("[[worker]]\nname = \"w1\"\naddress = \"127.0.0.1:%d\"\ntransport = \"tcp\"\n", e.port)
	for _, d := range []string{e.client, e.admin, e.stranger} {
		if err := os.WriteFile(filepath.Join(d, "workers.toml"), []byte(workers), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(e.cleanup)
	return e
}

type result struct {
	stdout, stderr string
	code           int
}

func (e *env) longarm(cfgDir string, args ...string) result {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bins.longarm, args...)
	cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+cfgDir)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			e.t.Fatalf("longarm %v: %v", args, err)
		}
	}
	return result{out.String(), errb.String(), code}
}

func (e *env) mustLongarm(cfgDir string, args ...string) string {
	e.t.Helper()
	r := e.longarm(cfgDir, args...)
	if r.code != 0 {
		e.t.Fatalf("longarm %v: exit %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

// startLongarmd starts longarmd and waits until it listens.
func (e *env) startLongarmd() {
	e.t.Helper()
	endpoints := filepath.Join(e.dataDir, "longarmd.endpoints")
	os.Remove(endpoints)
	logf, err := os.OpenFile(filepath.Join(e.root, "longarmd.out"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	cmd := exec.Command(bins.longarmd, "run", "--data-dir", e.dataDir, "--listen", fmt.Sprintf("tcp://127.0.0.1:%d", e.port))
	cmd.SysProcAttr = e.longarmdSysProcAttr
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	logf.Close()
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	e.mu.Lock()
	e.longarmd = cmd
	e.mu.Unlock()
	deadline := time.Now().Add(60 * time.Second) // first start of a new exe can be slow (AV scan)
	for {
		if _, err := os.Stat(endpoints); err == nil {
			return
		}
		select {
		case <-exited:
			b, _ := os.ReadFile(filepath.Join(e.root, "longarmd.out"))
			e.t.Fatalf("longarmd exited during startup:\n%s", b)
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			e.t.Fatal("longarmd did not start listening in time")
		}
	}
}

// killLongarmd hard-kills longarmd (no graceful shutdown), like a crash.
func (e *env) killLongarmd() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.longarmd != nil {
		e.longarmd.Process.Kill()
		e.longarmd.Process.Wait()
		e.longarmd = nil
	}
}

func (e *env) cleanup() {
	e.killLongarmd()
	killShims(e.dataDir)
	time.Sleep(300 * time.Millisecond)
	if e.t.Failed() {
		b, _ := os.ReadFile(filepath.Join(e.root, "longarmd.out"))
		e.t.Logf("longarmd log:\n%s", b)
	}
}

// killShims kills the shims and job processes of a data dir still alive
// so the temp dir can go.
func killShims(dataDir string) {
	type proc struct {
		pid   int
		start uint64
	}
	var procs []proc
	for _, kind := range []string{"jobs", "sessions"} {
		ents, _ := os.ReadDir(filepath.Join(dataDir, kind))
		for _, de := range ents {
			st, err := jobs.Dir(filepath.Join(dataDir, kind, de.Name())).ReadStatus()
			if err != nil {
				continue
			}
			for _, p := range []proc{{st.ShimPID, st.ShimStart}, {st.PID, st.PIDStart}} {
				if platform.ProcessAlive(p.pid, p.start) {
					platform.KillProcess(p.pid)
					procs = append(procs, p)
				}
			}
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, p := range procs {
		for platform.ProcessAlive(p.pid, p.start) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func (e *env) status(id string) *jobs.Status {
	e.t.Helper()
	st, err := jobs.Dir(filepath.Join(e.dataDir, "jobs", id)).ReadStatus()
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var ulidRE = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func (e *env) run(args ...string) string {
	e.t.Helper()
	id := strings.TrimSpace(e.mustLongarm(e.client, append([]string{"run", "w1"}, args...)...))
	if !jobs.ValidID(id) {
		e.t.Fatalf("bad job id %q", id)
	}
	return id
}

func lines(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func stripErr(s string) string {
	var out []string
	for _, l := range strings.SplitAfter(s, "\n") {
		if !strings.HasPrefix(l, "err ") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "")
}

func TestE2E(t *testing.T) {
	e := newEnv(t)
	e.startLongarmd()

	t.Run("ls_and_info", func(t *testing.T) {
		out := e.mustLongarm(e.client, "ls")
		if !strings.Contains(out, "w1") || strings.Contains(out, "ERROR") {
			t.Fatalf("ls:\n%s", out)
		}
		var js map[string]map[string]any
		if err := json.Unmarshal([]byte(e.mustLongarm(e.client, "ls", "--json")), &js); err != nil {
			t.Fatal(err)
		}
		if js["w1"]["memTotalBytes"] == nil || js["w1"]["agents"] == nil {
			t.Fatalf("ls --json: %v", js)
		}
		// protojson randomizes whitespace per build, so decode instead of matching text;
		// the JSON object is followed by a "path: ..." line.
		info := e.mustLongarm(e.client, "info", "w1")
		var ni map[string]any
		if err := json.NewDecoder(strings.NewReader(info)).Decode(&ni); err != nil ||
			ni["callerRole"] != "client" || ni["name"] != "w1" {
			t.Fatalf("info (%v):\n%s", err, info)
		}
	})

	t.Run("exec", func(t *testing.T) {
		r := e.longarm(e.client, "exec", "w1", "--", bins.helper, "print", "5", "0", "7")
		if r.code != 7 || r.stdout != lines(1, 5) || r.stderr != "err 5\n" {
			t.Fatalf("exec: %+v", r)
		}
		r = e.longarm(e.client, "exec", "w1", "--combined", "--", bins.helper, "print", "5", "0", "0")
		if r.code != 0 || !strings.Contains(r.stdout, "err 5") {
			t.Fatalf("exec combined: %+v", r)
		}
		start := time.Now()
		r = e.longarm(e.client, "exec", "w1", "--timeout", "1s", "--", bins.helper, "sleep", "60")
		if r.code != platform.KilledExitCode || time.Since(start) > 20*time.Second {
			t.Fatalf("exec timeout: %+v after %v", r, time.Since(start))
		}
		r = e.longarm(e.client, "exec", "w1", "--", "definitely-not-a-command-xyz")
		if r.code != 127 && r.code != 255 {
			t.Fatalf("missing command: %+v", r)
		}
	})

	t.Run("job_logs_wait_exit_code", func(t *testing.T) {
		id := e.run("--name", "printer", "--", bins.helper, "print", "20", "50", "3")
		r := e.longarm(e.client, "wait", "w1", id)
		if r.code != 3 {
			t.Fatalf("wait exit = %d, want 3 (%s)", r.code, r.stderr)
		}
		full := e.mustLongarm(e.client, "logs", "w1", id)
		if stripErr(full) != lines(1, 20) || strings.Count(full, "err ") != 4 {
			t.Fatalf("logs:\n%s", full)
		}
		// From a byte offset, and from the end.
		off := len(lines(1, 10))
		if !strings.Contains(full[:off+20], "err") { // stderr interleaving shifts offsets; use real bytes
			off = len(full) / 2
		}
		if got := e.mustLongarm(e.client, "logs", "w1", id, "--from", strconv.Itoa(off)); got != full[off:] {
			t.Fatalf("logs --from %d = %q, want %q", off, got, full[off:])
		}
		if got := e.mustLongarm(e.client, "logs", "w1", id, "--from", "-9"); got != full[len(full)-9:] {
			t.Fatalf("logs --from -9 = %q", got)
		}
		jl := e.mustLongarm(e.client, "jobs", "w1")
		if !regexp.MustCompile(id + `\s+failed\s+3`).MatchString(jl) {
			t.Fatalf("jobs:\n%s", jl)
		}
		// Compat files of the job dir layout.
		d := jobs.Dir(filepath.Join(e.dataDir, "jobs", id))
		// (written after the state the API reports: wait for them)
		for name, want := range map[string]string{"state": "failed", "exit": "3"} {
			var b []byte
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
				if b, _ = os.ReadFile(d.Path(name)); string(b) == want || time.Now().After(deadline) {
					break
				}
			}
			if string(b) != want {
				t.Errorf("%s = %q, want %q", name, b, want)
			}
		}
		for _, f := range []string{"job.json", "status.json", "out.log", "pid"} {
			if _, err := os.Stat(d.Path(f)); err != nil {
				t.Errorf("missing %s", f)
			}
		}
		// --follow from an offset while the job runs; exit code mirrored.
		r = e.longarm(e.client, "run", "w1", "-f", "--", bins.helper, "print", "6", "100", "0")
		if r.code != 0 || stripErr(r.stdout[strings.Index(r.stdout, "\n")+1:]) != lines(1, 6) {
			t.Fatalf("run -f: %+v", r)
		}
	})

	t.Run("secrets_redacted_never_on_disk", func(t *testing.T) {
		const secret = "s3cr3t-Value-0xDEADBEEF"
		os.Setenv("E2E_TOKEN", secret)
		defer os.Unsetenv("E2E_TOKEN")
		id := e.run("--secret-env", "E2E_TOKEN", "--env", "PLAIN=visible", "--", bins.helper, "env", "E2E_TOKEN")
		if r := e.longarm(e.client, "wait", "w1", id); r.code != 0 {
			t.Fatalf("wait: %+v", r)
		}
		if got := e.mustLongarm(e.client, "logs", "w1", id); got != "E2E_TOKEN=***\n" {
			t.Fatalf("log = %q", got)
		}
		filepath.Walk(e.dataDir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || strings.HasSuffix(p, ".lock") {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(secret)) {
				t.Errorf("secret found on disk in %s", p)
			}
			return nil
		})
		spec, _ := jobs.Dir(filepath.Join(e.dataDir, "jobs", id)).ReadSpec()
		if len(spec.SecretEnvNames) != 1 || spec.SecretEnvNames[0] != "E2E_TOKEN" || spec.Env["PLAIN"] != "visible" {
			t.Fatalf("spec = %+v", spec)
		}
		// Exec redacts too.
		r := e.longarm(e.client, "exec", "w1", "--secret-env", "E2E_TOKEN", "--", bins.helper, "env", "E2E_TOKEN")
		if r.stdout != "E2E_TOKEN=***\n" {
			t.Fatalf("exec secret: %+v", r)
		}
	})

	t.Run("stdin_payload_never_on_disk", func(t *testing.T) {
		const marker = "PAYLOAD-Marker-0xFEEDFACE"
		payload := []byte("prompt with a secret: " + marker + "\nsecond line\n")
		file := filepath.Join(e.root, "payload.txt")
		os.WriteFile(file, payload, 0o600)
		want := fmt.Sprintf("%d %x\n", len(payload), sha256.Sum256(payload))
		r := e.longarm(e.client, "run", "w1", "--stdin-file", file, "--wait", "--", bins.helper, "stdin-sha")
		if r.code != 0 {
			t.Fatalf("run --stdin-file: %+v", r)
		}
		id := strings.TrimSpace(r.stdout)
		if got := e.mustLongarm(e.client, "logs", "w1", id); got != want {
			t.Fatalf("the job read %q, want %q", got, want)
		}
		// longarm's own stdin ("-"), detached (no --wait)
		cmd := exec.Command(bins.longarm, "run", "w1", "--stdin-file", "-", "--", bins.helper, "stdin-sha")
		cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+e.client)
		cmd.Stdin = bytes.NewReader(payload)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("run --stdin-file -: %v", err)
		}
		id2 := strings.TrimSpace(string(out))
		e.mustLongarm(e.client, "wait", "w1", id2)
		if got := e.mustLongarm(e.client, "logs", "w1", id2); got != want {
			t.Fatalf("stdin -: the job read %q, want %q", got, want)
		}
		// Neither the job record (job.json, the API, jobs --json), the audit
		// log, longarmd's log nor anything else on the worker has it.
		spec, _ := jobs.Dir(filepath.Join(e.dataDir, "jobs", id)).ReadSpec()
		if spec.StdinBytes != int64(len(payload)) {
			t.Fatalf("spec.StdinBytes = %d", spec.StdinBytes)
		}
		if jl := e.mustLongarm(e.client, "jobs", "w1", "--json", "--id", id); strings.Contains(jl, marker) ||
			!strings.Contains(jl, fmt.Sprintf(`"stdinBytes": "%d"`, len(payload))) {
			t.Fatalf("jobs --json: %s", jl)
		}
		filepath.Walk(e.dataDir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || strings.HasSuffix(p, ".lock") {
				return nil
			}
			if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(marker)) {
				t.Errorf("payload found on disk in %s", p)
			}
			return nil
		})
		if b, _ := os.ReadFile(filepath.Join(e.root, "longarmd.out")); bytes.Contains(b, []byte(marker)) {
			t.Error("payload in longarmd's log")
		}
		// at most 1 MiB, checked before anything is sent
		big := filepath.Join(e.root, "big.txt")
		os.WriteFile(big, bytes.Repeat([]byte("x"), jobs.MaxStdinBytes+1), 0o600)
		if r := e.longarm(e.client, "run", "w1", "--stdin-file", big, "--", bins.helper, "stdin-sha"); r.code != 2 || r.stdout != "" {
			t.Fatalf("oversized --stdin-file: %+v", r)
		}
	})

	t.Run("exit_codes_and_jobs_json", func(t *testing.T) {
		for _, c := range []struct {
			args []string
			code int
		}{
			{[]string{"run", "w1", "--no-such-flag", "--", "x"}, 2}, // cobra flag error
			{[]string{"wait"}, 2},                                      // argument count
			{[]string{"no-such-command"}, 2},                           // unknown command
			{[]string{"run", "w1", "--env", "NOEQUALS", "--", "x"}, 2}, // local validation
			{[]string{"run", "w1", "--stdin-file", filepath.Join(e.root, "missing"), "--", "x"}, 2},
			{[]string{"run", "w1", "--id", "bad/id", "--", "x"}, 2},    // InvalidArgument from the worker
			{[]string{"wait", "w1", "01NOSUCHJOB0000000000000000"}, 3}, // NotFound from the worker
			{[]string{"run", "no-such-worker", "--", "x"}, 3},          // unknown worker
			{[]string{"jobs", "--hub-only"}, 69},                       // no hub configured
			{[]string{"admin", "shutdown", "w1"}, 77},                  // a client may not
		} {
			if r := e.longarm(e.client, c.args...); r.code != c.code || r.stdout != "" {
				t.Errorf("longarm %v: exit %d, want %d (%s)", c.args, r.code, c.code, strings.TrimSpace(r.stderr))
			}
		}
		// jobs --json says where the list came from; --id of an unknown job
		// prints the (empty) list and exits 3
		var got struct {
			Jobs   []map[string]any `json:"jobs"`
			Source string           `json:"source"`
		}
		if err := json.Unmarshal([]byte(e.mustLongarm(e.client, "jobs", "w1", "--json", "--limit", "1")), &got); err != nil ||
			got.Source != "workers" || len(got.Jobs) != 1 {
			t.Fatalf("jobs --json: %+v %v", got, err)
		}
		known := got.Jobs[0]["job"].(map[string]any)["spec"].(map[string]any)["id"].(string)
		r := e.longarm(e.client, "jobs", "w1", "--json", "--id", "01NOSUCHJOB0000000000000000")
		if err := json.Unmarshal([]byte(r.stdout), &got); err != nil || r.code != 3 || got.Source != "workers" || len(got.Jobs) != 0 {
			t.Fatalf("jobs --id <unknown>: %+v", r)
		}
		// a job that exists but doesn't pass --state: an empty list, exit 0
		r = e.longarm(e.client, "jobs", "w1", "--json", "--id", known, "--state", "queued")
		if err := json.Unmarshal([]byte(r.stdout), &got); err != nil || r.code != 0 || len(got.Jobs) != 0 {
			t.Fatalf("jobs --id <known> --state <other>: %+v", r)
		}
		if r = e.longarm(e.client, "jobs", "w1", "--json", "--id", known); r.code != 0 || !strings.Contains(r.stdout, known) {
			t.Fatalf("jobs --id <known>: %+v", r)
		}
	})

	t.Run("idempotent_submit", func(t *testing.T) {
		id := "01JTESTIDEMPOTENT000000000"
		a := strings.TrimSpace(e.mustLongarm(e.client, "run", "w1", "--id", id, "--", bins.helper, "print", "1", "0", "0"))
		b := strings.TrimSpace(e.mustLongarm(e.client, "run", "w1", "--id", id, "--", bins.helper, "print", "1", "0", "0"))
		if a != id || b != id {
			t.Fatalf("ids %q %q", a, b)
		}
		e.mustLongarm(e.client, "wait", "w1", id)
		if got := e.mustLongarm(e.client, "logs", "w1", id); got != "line 1\n" {
			t.Fatalf("job ran twice? log %q", got)
		}
		r := e.longarm(e.client, "run", "w1", "--id", id, "--", bins.helper, "print", "2", "0", "0")
		if r.code == 0 || !strings.Contains(r.stderr, "AlreadyExists") {
			t.Fatalf("conflicting resubmit: %+v", r)
		}
	})

	t.Run("kill_whole_tree", func(t *testing.T) {
		pidfile := filepath.Join(e.root, "tree.pids")
		id := e.run("--", bins.helper, "tree", pidfile)
		var pids []int
		waitFor(t, "5 tree processes", 30*time.Second, func() bool {
			b, _ := os.ReadFile(pidfile)
			pids = pids[:0]
			for _, f := range strings.Fields(string(b)) {
				p, _ := strconv.Atoi(f)
				pids = append(pids, p)
			}
			return len(pids) == 5
		})
		for _, p := range pids {
			if !platform.ProcessAlive(p, 0) {
				t.Fatalf("pid %d not alive before kill", p)
			}
		}
		out := e.mustLongarm(e.client, "kill", "w1", id)
		if !strings.Contains(out, "killed") {
			t.Fatalf("kill: %s", out)
		}
		waitFor(t, "all tree processes to die", 10*time.Second, func() bool {
			for _, p := range pids {
				if platform.ProcessAlive(p, 0) {
					return false
				}
			}
			return true
		})
		if r := e.longarm(e.client, "wait", "w1", id); r.code != 137 {
			t.Fatalf("wait after kill: %+v", r)
		}
	})

	t.Run("kill_pwsh_ping_tree", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("windows only")
		}
		pwsh, err := exec.LookPath("pwsh")
		if err != nil {
			t.Skip("pwsh not installed")
		}
		pidfile := filepath.Join(e.root, "ping.pid")
		script := fmt.Sprintf(`$p = Start-Process -FilePath ping.exe -ArgumentList '-n','300','127.0.0.1' -NoNewWindow -PassThru; Set-Content -Path '%s' -Value $p.Id; Wait-Process -Id $p.Id`, pidfile)
		id := e.run("--", pwsh, "-NoProfile", "-Command", script)
		var ping int
		waitFor(t, "ping to start", 30*time.Second, func() bool {
			b, _ := os.ReadFile(pidfile)
			ping, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			return ping > 0
		})
		st := e.status(id)
		if !platform.ProcessAlive(ping, 0) || !platform.ProcessAlive(st.PID, st.PIDStart) {
			t.Fatal("pwsh/ping not running")
		}
		e.mustLongarm(e.client, "kill", "w1", id)
		waitFor(t, "ping and pwsh to die", 10*time.Second, func() bool {
			return !platform.ProcessAlive(ping, 0) && !platform.ProcessAlive(st.PID, st.PIDStart)
		})
	})

	t.Run("authorization", func(t *testing.T) {
		r := e.longarm(e.client, "admin", "shutdown", "w1")
		if r.code == 0 || !strings.Contains(r.stderr, "PermissionDenied") {
			t.Fatalf("client called admin RPC: %+v", r)
		}
		r = e.longarm(e.stranger, "ls")
		if r.code == 0 || !strings.Contains(r.stdout+r.stderr, "ERROR") {
			t.Fatalf("foreign-CA cert accepted: %+v", r)
		}
		r = e.longarm(e.stranger, "info", "w1")
		if r.code == 0 || !(strings.Contains(r.stderr, "certificate") || strings.Contains(r.stderr, "tls")) {
			t.Fatalf("foreign-CA cert accepted: %+v", r)
		}
		// longarmd is still up and serving the legitimate client.
		e.mustLongarm(e.client, "info", "w1")
	})

	t.Run("shim_crash_marks_lost", func(t *testing.T) {
		id := e.run("--", bins.helper, "sleep", "120")
		st := e.status(id)
		waitFor(t, "running", 10*time.Second, func() bool { st = e.status(id); return st.State == jobs.Running })
		platform.KillProcess(st.ShimPID)
		r := e.longarm(e.client, "wait", "w1", id)
		if runtime.GOOS == "windows" {
			// KILL_ON_JOB_CLOSE: the tree died with the shim.
			if r.code != 254 || !strings.Contains(r.stderr, "lost") {
				t.Fatalf("wait: %+v", r)
			}
			if platform.ProcessAlive(st.PID, st.PIDStart) {
				t.Fatal("job process survived its shim")
			}
		}
	})

	t.Run("admin_shutdown", func(t *testing.T) {
		e.mustLongarm(e.admin, "admin", "shutdown", "w1")
		e.mu.Lock()
		cmd := e.longarmd
		e.mu.Unlock()
		done := make(chan struct{})
		go func() { cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("longarmd did not exit after admin shutdown")
		}
		e.mu.Lock()
		e.longarmd = nil
		e.mu.Unlock()
	})
}

// TestRestartReadopt kills longarmd mid-job (hard, like a crash), checks the job
// keeps running under its shim, restarts longarmd, and verifies the job is
// re-adopted: a `longarm logs -f` started before the crash reconnects and gets
// every line exactly once, and `longarm wait` gets the real exit code.
func TestRestartReadopt(t *testing.T) {
	e := newEnv(t)
	e.startLongarmd()

	const n = 40
	id := e.run("--name", "survivor", "--", bins.helper, "print", strconv.Itoa(n), "150", "5")
	var st *jobs.Status
	waitFor(t, "running", 10*time.Second, func() bool { st = e.status(id); return st.State == jobs.Running })

	// A follower that must survive the longarmd restart. Start from offset 7
	// ("line 1\n" is 7 bytes) to exercise --from with -f.
	follow := exec.Command(bins.longarm, "logs", "w1", id, "-f", "--from", "7")
	follow.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+e.client)
	var fout, ferr bytes.Buffer
	follow.Stdout, follow.Stderr = &fout, &ferr
	if err := follow.Start(); err != nil {
		t.Fatal(err)
	}
	defer follow.Process.Kill()
	followDone := make(chan error, 1)
	go func() { followDone <- follow.Wait() }()

	time.Sleep(1200 * time.Millisecond)
	e.killLongarmd()
	logPath := filepath.Join(e.dataDir, "jobs", id, "out.log")
	size := func() int64 { fi, _ := os.Stat(logPath); return fi.Size() }
	before := size()
	if !platform.ProcessAlive(st.ShimPID, st.ShimStart) || !platform.ProcessAlive(st.PID, st.PIDStart) {
		t.Fatal("job died with longarmd")
	}
	time.Sleep(1 * time.Second)
	if after := size(); after <= before {
		t.Fatalf("job output stopped while longarmd was down (%d -> %d)", before, after)
	}

	e.startLongarmd()
	jl := e.mustLongarm(e.client, "jobs", "w1", "--state", "running")
	if !strings.Contains(jl, id) {
		t.Fatalf("job not re-adopted as running:\n%s", jl)
	}
	if r := e.longarm(e.client, "wait", "w1", id); r.code != 5 {
		t.Fatalf("wait after restart: %+v", r)
	}
	select {
	case err := <-followDone:
		if err != nil {
			t.Fatalf("follower: %v\n%s", err, ferr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("follower did not finish; stderr:\n%s", ferr.String())
	}
	if got := stripErr(fout.String()); got != lines(2, n) {
		t.Fatalf("follower output wrong:\n%q\nstderr:\n%s", got, ferr.String())
	}
	if !strings.Contains(ferr.String(), "unreachable") {
		t.Logf("note: follower never saw the outage (stderr %q)", ferr.String())
	}
	if s := e.status(id); s.State != jobs.Failed || *s.ExitCode != 5 || s.Orphaned {
		t.Fatalf("final status %+v", s)
	}

	// longarmd down + shim gone => lost on the next start (reconcile).
	id2 := e.run("--", bins.helper, "sleep", "120")
	waitFor(t, "running", 10*time.Second, func() bool { st = e.status(id2); return st.State == jobs.Running })
	e.killLongarmd()
	platform.KillProcess(st.ShimPID)
	if runtime.GOOS != "windows" {
		platform.KillProcess(st.PID)
	}
	waitFor(t, "job process gone", 10*time.Second, func() bool { return !platform.ProcessAlive(st.PID, st.PIDStart) })
	e.startLongarmd()
	if r := e.longarm(e.client, "wait", "w1", id2); r.code != 254 {
		t.Fatalf("expected lost (254), got %+v", r)
	}
	if s := e.status(id2); s.State != jobs.Lost {
		t.Fatalf("status %+v", s)
	}
}
