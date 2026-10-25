package e2e

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// containerHub is longarm-hub started the way the image starts it
// (`longarm-hub container`, configured only by the environment of
// deploy/azure), with the e2e helper as a fake litestream.
type containerHub struct {
	t                  *testing.T
	root, share, local string
	port, httpPort     int
	lsLog              string
	extraEnv           []string
}

func newContainerHub(t *testing.T) *containerHub {
	root := t.TempDir()
	return &containerHub{t: t, root: root, share: filepath.Join(root, "share"), local: filepath.Join(root, "local"),
		port: freePort(t), httpPort: freePort(t), lsLog: filepath.Join(root, "litestream.log")}
}

func (c *containerHub) env() []string {
	return append(append(os.Environ(),
		"LONGARM_HUB_CONFIG="+filepath.Join(c.share, "hub.toml"),
		"LONGARM_HUB_DATA_DIR="+c.share,
		"LONGARM_HUB_DB="+filepath.Join(c.local, "hub.db"),
		fmt.Sprintf("LONGARM_HUB_LISTEN=127.0.0.1:%d", c.port),
		fmt.Sprintf("LONGARM_HUB_HTTP=127.0.0.1:%d", c.httpPort),
		"LONGARM_HUB_ADVERTISE=127.0.0.1",
		"LONGARM_HUB_BOOTSTRAP=1",
		"LONGARM_HUB_LITESTREAM_CONFIG="+filepath.Join(c.root, "litestream.yml"),
		"LONGARM_HUB_LITESTREAM_BIN="+bins.helper,
		"HELPER_LITESTREAM_LOG="+c.lsLog,
	), c.extraEnv...)
}

// start runs `longarm-hub container` and returns it with its output buffer.
func (c *containerHub) start(hubBin string) (*exec.Cmd, *syncBuffer, chan struct{}) {
	cmd := exec.Command(hubBin, "container")
	cmd.Env = c.env()
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 5 * time.Second // a leftover grandchild must not block Wait on the pipes
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	c.t.Cleanup(func() { c.kill(cmd, done) })
	return cmd, out, done
}

// kill ends the container's processes: the entrypoint (now the fake
// litestream) and its -exec child, longarm-hub serve.
func (c *containerHub) kill(cmd *exec.Cmd, done chan struct{}) {
	// serve first: the fake litestream and the entrypoint then exit, and the
	// output pipes they share close
	if b, err := os.ReadFile(c.lsLog + ".child"); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
				p.Wait()
			}
		}
		os.Remove(c.lsLog + ".child")
	}
	cmd.Process.Kill()
	<-done
	// the database is free again (Windows) once serve is gone
	deadline := time.Now().Add(10 * time.Second)
	for c.healthy(buildHub(c.t)) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
}

func (c *containerHub) healthy(hubBin string) bool {
	hc := exec.Command(hubBin, "healthcheck")
	hc.Env = c.env()
	return hc.Run() == nil
}

func (c *containerHub) waitHealthy(hubBin string, out *syncBuffer, done chan struct{}) {
	deadline := time.Now().Add(60 * time.Second)
	for !c.healthy(hubBin) {
		select {
		case <-done:
			c.t.Fatalf("longarm-hub container exited:\n%s", out.String())
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("not healthy in time:\n%s", out.String())
		}
	}
}

// bootstrapLine parses the LONGARM_BOOTSTRAP line of the output.
func bootstrapLine(t *testing.T, out string) map[string]string {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "LONGARM_BOOTSTRAP "); ok {
			m := map[string]string{}
			for _, f := range strings.Fields(rest) {
				k, v, _ := strings.Cut(f, "=")
				m[k] = v
			}
			return m
		}
	}
	t.Fatalf("no LONGARM_BOOTSTRAP line in:\n%s", out)
	return nil
}

// TestHubContainerStart: the image's start sequence on a fresh "share" -
// restore (nothing), bootstrap (CA, hub.toml, admin code), serve under
// litestream replicate - then an admin joins with exactly what the deploy
// script prints, and a restart neither changes the CA nor offers a code again.
func TestHubContainerStart(t *testing.T) {
	t.Parallel()
	hubBin := buildHub(t)
	c := newContainerHub(t)
	cmd, out, done := c.start(hubBin)
	c.waitHealthy(hubBin, out, done)

	b := bootstrapLine(t, out.String())
	want := fmt.Sprintf("127.0.0.1:%d", c.port)
	if b["hub_address"] != want || len(b["ca_fingerprint"]) != 64 || b["admin_join_code"] == "" || b["admin_joined"] != "false" {
		t.Fatalf("bootstrap %v", b)
	}
	envFile, err := os.ReadFile(filepath.Join(c.share, "bootstrap.env"))
	if err != nil || !strings.Contains(string(envFile), "admin_join_code="+b["admin_join_code"]+"\n") {
		t.Fatalf("bootstrap.env: %s %v", envFile, err)
	}
	for _, f := range []string{"hub.toml", "ca/ca.crt", "ca/ca.key", "tls/hub/node.crt"} {
		if _, err := os.Stat(filepath.Join(c.share, f)); err != nil {
			t.Errorf("share: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(c.local, "hub.db")); err != nil {
		t.Errorf("local db: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.share, "hub.db")); err == nil {
		t.Error("hub.db on the share")
	}
	lsLog, _ := os.ReadFile(c.lsLog)
	db := filepath.Join(c.local, "hub.db")
	cfgLS := filepath.Join(c.root, "litestream.yml")
	if lines := strings.Split(strings.TrimSpace(string(lsLog)), "\n"); len(lines) != 2 ||
		lines[0] != "restore|-config|"+cfgLS+"|-if-db-not-exists|-if-replica-exists|"+db ||
		!strings.HasPrefix(lines[1], "replicate|-config|"+cfgLS+"|-exec|") || !strings.HasSuffix(lines[1], " serve") {
		t.Fatalf("litestream calls:\n%s", lsLog)
	}

	// the admin's laptop: the printed command
	e := &env{t: t, root: c.root}
	laptop := filepath.Join(c.root, "laptop")
	r := e.longarm(laptop, "join", b["hub_address"], b["admin_join_code"], "--ca-fingerprint", b["ca_fingerprint"], "--name", "laptop")
	if r.code != 0 || !strings.Contains(r.stdout+r.stderr, "joined") {
		t.Fatalf("join: %+v", r)
	}
	if out := e.mustLongarm(laptop, "admin", "join-code", "--role", "worker"); strings.TrimSpace(out) == "" {
		t.Fatal("no worker code")
	}
	// the code was one-time
	if r := e.longarm(filepath.Join(c.root, "eve"), "join", b["hub_address"], b["admin_join_code"], "--ca-fingerprint", b["ca_fingerprint"]); r.code == 0 {
		t.Fatal("admin code reused")
	}

	// replace the container: same share, same (restored) database
	c.kill(cmd, done)
	_, out2, done2 := c.start(hubBin)
	c.waitHealthy(hubBin, out2, done2)
	b2 := bootstrapLine(t, out2.String())
	if b2["ca_fingerprint"] != b["ca_fingerprint"] || b2["admin_joined"] != "true" || b2["admin_join_code"] != "" {
		t.Fatalf("restart bootstrap %v", b2)
	}
	if out := e.mustLongarm(laptop, "admin", "join-code", "--role", "worker"); strings.TrimSpace(out) == "" {
		t.Fatal("admin cert after restart")
	}
}

// TestHubContainerRestoreFails: a replica that can't be read stops the
// start (a hub must never come up empty next to its real data).
func TestHubContainerRestoreFails(t *testing.T) {
	t.Parallel()
	hubBin := buildHub(t)
	c := newContainerHub(t)
	c.extraEnv = []string{"HELPER_LITESTREAM_RESTORE_EXIT=3"}
	cmd := exec.Command(hubBin, "container")
	cmd.Env = c.env()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err == nil || !strings.Contains(out.String(), "litestream restore") {
		t.Fatalf("container with failing restore: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(c.share, "ca")); err == nil {
		t.Fatal("bootstrapped after a failed restore")
	}
}

// syncBuffer is a bytes.Buffer safe for a process writing while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
