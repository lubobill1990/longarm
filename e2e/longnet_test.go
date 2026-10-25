package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// longarm-relay is built on first use (TestMain in e2e_test.go builds longarmd/longarm).
var relayBin struct {
	once sync.Once
	path string
	err  error
}

func buildRelay(t *testing.T) string {
	t.Helper()
	relayBin.once.Do(func() {
		relayBin.path = filepath.Join(bins.dir, exeName("longarm-relay"))
		goBin := filepath.Join(runtime.GOROOT(), "bin", exeName("go"))
		if _, err := os.Stat(goBin); err != nil {
			goBin = "go"
		}
		if b, err := exec.Command(goBin, "build", "-o", relayBin.path, "../cmd/longarm-relay").CombinedOutput(); err != nil {
			relayBin.err = fmt.Errorf("build longarm-relay: %v\n%s", err, b)
		}
	})
	if relayBin.err != nil {
		t.Fatal(relayBin.err)
	}
	return relayBin.path
}

// longnetEnv is an e2e env plus a local longarm-relay, and a longarmd listening on both
// tcp and longnet.
type longnetEnv struct {
	*env
	relayAddr   string
	metricsAddr string
	relay       *exec.Cmd
}

var relayLineRE = regexp.MustCompile(`longarm-relay listening udp=(\S+) tcp=(\S+) metrics=(\S+)`)

func newLongNetEnv(t *testing.T) *longnetEnv {
	t.Helper()
	relayExe := buildRelay(t)
	e := newEnv(t)
	re := &longnetEnv{env: e}
	relayTLS := filepath.Join(e.root, "relay-tls")
	e.mustLongarm(e.client, "admin", "issue", "--role", "relay", "--name", "relay1", "--host", "127.0.0.1", "--out", relayTLS)

	// longarm-relay on a free port (UDP and TCP on the same number).
	port := freePort(t)
	logf, err := os.Create(filepath.Join(e.root, "relay.out"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(relayExe, "--listen", fmt.Sprintf("127.0.0.1:%d", port), "--tls-dir", relayTLS,
		"--metrics", "127.0.0.1:0", "--log-interval", "0", "--rate", "0")
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	re.relay = cmd
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		logf.Close()
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(e.root, "relay.out"))
			t.Logf("longarm-relay log:\n%s", b)
		}
	})
	line := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if m := relayLineRE.FindStringSubmatch(sc.Text()); m != nil {
				line <- sc.Text()
			}
		}
		io.Copy(io.Discard, stdout)
	}()
	select {
	case l := <-line:
		m := relayLineRE.FindStringSubmatch(l)
		re.relayAddr, re.metricsAddr = m[1], m[3]
	case <-time.After(60 * time.Second):
		t.Fatal("longarm-relay did not start")
	}

	// Client config: the same worker over tcp ("w1") and over longnet ("w1-net").
	workers := fmt.Sprintf(`[longnet]
relay = %q

[[worker]]
name = "w1"
address = "127.0.0.1:%d"

[[worker]]
name = "w1-net"
cert_name = "w1"
transport = "longnet"
`, re.relayAddr, e.port)
	for _, d := range []string{e.client, e.admin} {
		if err := os.WriteFile(filepath.Join(d, "workers.toml"), []byte(workers), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return re
}

// startLongarmdLongNet starts longarmd listening on tcp and longnet at once.
func (re *longnetEnv) startLongarmdLongNet(extraEnv ...string) {
	e := re.env
	e.t.Helper()
	endpoints := filepath.Join(e.dataDir, "longarmd.endpoints")
	os.Remove(endpoints)
	logf, err := os.OpenFile(filepath.Join(e.root, "longarmd.out"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		e.t.Fatal(err)
	}
	cmd := exec.Command(bins.longarmd, "run", "--data-dir", e.dataDir,
		"--listen", fmt.Sprintf("tcp://127.0.0.1:%d", e.port),
		"--listen", fmt.Sprintf("longnet://%s?name=w1", re.relayAddr))
	cmd.Env = append(os.Environ(), extraEnv...)
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
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(endpoints); err == nil {
			break
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
	// Listening doesn't mean registered with the relay yet.
	waitFor(e.t, "longarmd's relay session", 30*time.Second, func() bool {
		st := re.relayStats()
		for _, s := range st.Sessions {
			if s.Node == "worker/w1" {
				return true
			}
		}
		return false
	})
}

type relayStats struct {
	Sessions []struct {
		Node      string   `json:"node"`
		Transport string   `json:"transport"`
		NAT       string   `json:"nat"`
		Reflexive string   `json:"reflexive"`
		Mapped    []string `json:"mapped"`
	} `json:"sessions"`
	BytesRelayed uint64 `json:"bytes_relayed"`
	STUNRequests uint64 `json:"stun_requests"`
}

func (re *longnetEnv) relayStats() relayStats {
	re.t.Helper()
	resp, err := http.Get("http://" + re.metricsAddr + "/sessions")
	if err != nil {
		re.t.Fatal(err)
	}
	defer resp.Body.Close()
	var st relayStats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		re.t.Fatal(err)
	}
	return st
}

// longarmWith runs longarm with extra environment variables.
func (e *env) longarmWith(extraEnv []string, cfgDir string, args ...string) result {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bins.longarm, args...)
	cmd.Env = append(append(os.Environ(), "LONGARM_CONFIG_DIR="+cfgDir), extraEnv...)
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

func TestLongNetE2E(t *testing.T) {
	re := newLongNetEnv(t)
	re.startLongarmdLongNet()
	e := re.env

	modes := []struct {
		name     string
		env      []string
		pathRE   string // expected PATH column / path: line
		relayed  bool   // bulk data must go through the relay
		directOK bool   // bulk data may (should) go direct
	}{
		{"auto", nil, `longnet/(direct|relay) `, false, true},
		{"forced-relay-udp", []string{"LONGARM_NET_DISABLE_DIRECT=1"}, `longnet/relay \S+ \(udp 127\.0\.0\.1:\d+\)`, true, false},
		{"forced-relay-tcp", []string{"LONGARM_NET_DISABLE_DIRECT=1", "LONGARM_NET_RELAY_MODE=tcp"}, `longnet/relay \S+ \(tcp 127\.0\.0\.1:\d+\)`, true, false},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			longarm := func(args ...string) result { return e.longarmWith(m.env, e.client, args...) }
			must := func(args ...string) string {
				t.Helper()
				r := longarm(args...)
				if r.code != 0 {
					t.Fatalf("longarm %v: exit %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.stderr)
				}
				return r.stdout
			}

			// ls: both paths to the same worker, longnet's PATH column.
			out := must("ls")
			if strings.Contains(out, "ERROR") || !regexp.MustCompile(`(?m)^w1-net .*`+m.pathRE).MatchString(out) ||
				!regexp.MustCompile(`(?m)^w1 .*tcp/direct`).MatchString(out) {
				t.Fatalf("ls:\n%s", out)
			}
			info := must("info", "w1-net")
			if !strings.Contains(info, `"callerRole"`) || !regexp.MustCompile(`path: `+m.pathRE).MatchString(info) ||
				!strings.Contains(info, "longnet://"+re.relayAddr+"?name=w1") {
				t.Fatalf("info:\n%s", info)
			}

			// exec: exit code and output (bulk: ~600 KB of stdout).
			r := longarm("exec", "w1-net", "--", bins.helper, "print", "5", "0", "7")
			if r.code != 7 || r.stdout != lines(1, 5) || r.stderr != "err 5\n" {
				t.Fatalf("exec: %+v", r)
			}
			before := re.relayStats().BytesRelayed
			// The session starts on the relay and moves to the direct path
			// once it is up (a few ms to ~0.2 s); a fast machine prints all
			// of it before that, so the output starts after a second.
			startMS := "0"
			if m.directOK {
				startMS = "1000"
			}
			r = longarm("exec", "w1-net", "--", bins.helper, "print", "50000", "0", "0", startMS)
			if r.code != 0 || r.stdout != lines(1, 50000) {
				t.Fatalf("bulk exec: exit %d, %d bytes (%s)", r.code, len(r.stdout), r.stderr)
			}
			relayed := re.relayStats().BytesRelayed - before
			t.Logf("bulk exec: %d bytes of output, %d bytes through the relay", len(r.stdout), relayed)
			if m.relayed && relayed < uint64(len(r.stdout)) {
				t.Fatalf("forced relay but only %d bytes relayed", relayed)
			}
			if m.directOK && relayed > uint64(len(r.stdout))/2 {
				t.Errorf("direct path expected on loopback, but %d of %d bytes were relayed", relayed, len(r.stdout))
			}

			// run + logs -f + wait, the long-stream case.
			id := strings.TrimSpace(must("run", "w1-net", "--name", "longnet-"+m.name, "--", bins.helper, "print", "30", "100", "4"))
			lf := longarm("logs", "-f", "w1-net", id)
			if lf.code != 0 || stripErr(lf.stdout) != lines(1, 30) {
				t.Fatalf("logs -f: %+v", lf)
			}
			if w := longarm("wait", "w1-net", id); w.code != 4 {
				t.Fatalf("wait: %+v", w)
			}
			// Same job seen over tcp.
			if got := e.mustLongarm(e.client, "logs", "w1", id); got != lf.stdout {
				t.Fatalf("tcp logs differ:\n%s\nvs\n%s", got, lf.stdout)
			}
			jl := must("jobs", "w1-net")
			if !regexp.MustCompile(id + `\s+failed\s+4`).MatchString(jl) {
				t.Fatalf("jobs:\n%s", jl)
			}
			// run -f (submit + follow) over longnet.
			r = longarm("run", "w1-net", "-f", "--", bins.helper, "print", "6", "50", "0")
			if r.code != 0 || stripErr(r.stdout[strings.Index(r.stdout, "\n")+1:]) != lines(1, 6) {
				t.Fatalf("run -f: %+v", r)
			}
		})
	}

	t.Run("nat_detection", func(t *testing.T) {
		// longarmd STUNs the relay's two ports; on loopback its public address
		// is its own socket: NAT type "open", shown in /sessions.
		waitFor(t, "worker NAT type in /sessions", 15*time.Second, func() bool {
			st := re.relayStats()
			for _, s := range st.Sessions {
				if s.Node == "worker/w1" {
					return s.NAT == "open" && len(s.Mapped) == 1 && s.Reflexive != "" && st.STUNRequests >= 2
				}
			}
			return false
		})
		// LONGARM_NET_LOG=1 makes longarm print its longnet events (longarm has no logger).
		r := e.longarmWith([]string{"LONGARM_NET_LOG=1"}, e.client, "info", "w1-net")
		if r.code != 0 || !strings.Contains(r.stderr, "longnet: relay session up") {
			t.Fatalf("LONGARM_NET_LOG=1 longarm info: %+v", r)
		}
	})

	t.Run("authorization", func(t *testing.T) {
		// Admin-only RPC over longnet with a client cert is denied by longarmd.
		r := e.longarm(e.client, "admin", "shutdown", "w1-net")
		if r.code == 0 || !strings.Contains(r.stderr, "PermissionDenied") {
			t.Fatalf("shutdown as client: %+v", r)
		}
		// A certificate from another CA can't even reach the relay.
		stranger := filepath.Join(e.root, "stranger")
		b, _ := os.ReadFile(filepath.Join(e.client, "workers.toml"))
		os.WriteFile(filepath.Join(stranger, "workers.toml"), b, 0o644)
		r = e.longarm(stranger, "info", "w1-net")
		if r.code == 0 {
			t.Fatalf("stranger reached w1 over longnet: %+v", r)
		}
	})

	t.Run("unknown_worker_fails_fast", func(t *testing.T) {
		cfg := filepath.Join(e.root, "client2")
		os.MkdirAll(cfg, 0o755)
		copyDir(t, filepath.Join(e.client, "tls"), filepath.Join(cfg, "tls"))
		os.WriteFile(filepath.Join(cfg, "workers.toml"), []byte(fmt.Sprintf(
			"[[worker]]\nname = \"ghost\"\ntransport = \"longnet\"\naddress = %q\n", re.relayAddr)), 0o644)
		start := time.Now()
		r := e.longarm(cfg, "ls")
		if !strings.Contains(r.stdout, "not connected to the relay") || time.Since(start) > 20*time.Second {
			t.Fatalf("ls ghost after %v: %+v", time.Since(start), r)
		}
	})

	t.Run("admin_shutdown_over_longnet", func(t *testing.T) {
		e.mustLongarm(e.admin, "admin", "shutdown", "w1-net")
		waitFor(t, "longarmd exit", 20*time.Second, func() bool {
			_, err := os.Stat(filepath.Join(e.dataDir, "longarmd.endpoints"))
			return os.IsNotExist(err)
		})
	})
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	os.MkdirAll(to, 0o700)
	ents, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range ents {
		b, err := os.ReadFile(filepath.Join(from, de.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, de.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
