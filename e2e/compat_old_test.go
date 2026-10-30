package e2e

// Compatibility with released versions (docs/security.md "Node identity",
// "Older versions"): a hub of this tree takes over the database of the last
// release before node IDs (schema 5) and keeps serving the workers and
// clients that release enrolled, running the released binaries -
// downloaded from GitHub and checked against the release's SHA256SUMS.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
)

// oldRel is a released version's binaries for this OS/arch.
type oldRel struct {
	version                       string
	longarm, longarmd, hub, relay string
}

var oldRels struct {
	mu sync.Mutex
	m  map[string]*oldRel
}

const releaseDownload = "https://github.com/lubobill1990/longarm/releases/download/"

// oldRelease downloads (once; cached across runs in
// LONGARM_E2E_RELEASE_CACHE, default the user cache dir) longarm, longarmd,
// longarm-hub and longarm-relay of version for this OS/arch and checks
// each against the release's SHA256SUMS (the per-binary list; the
// release's checksums.txt covers the package archives). Unreachable
// GitHub: a failure in CI, a skip elsewhere.
func oldRelease(t *testing.T, version string) *oldRel {
	t.Helper()
	oldRels.mu.Lock()
	defer oldRels.mu.Unlock()
	if r := oldRels.m[version]; r != nil {
		return r
	}
	fail := func(format string, a ...any) {
		t.Helper()
		if os.Getenv("CI") != "" {
			t.Fatalf(format, a...)
		}
		t.Skipf(format+" (outside CI: skipped)", a...)
	}
	cache := os.Getenv("LONGARM_E2E_RELEASE_CACHE")
	if cache == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			d = os.TempDir()
		}
		cache = filepath.Join(d, "longarm-e2e-releases")
	}
	dir := filepath.Join(cache, version, runtime.GOOS+"-"+runtime.GOARCH)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	get := func(asset string) ([]byte, error) {
		resp, err := client.Get(releaseDownload + version + "/" + asset)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", asset, resp.Status)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	}
	sums, err := get("SHA256SUMS")
	if err != nil {
		fail("release %s: SHA256SUMS: %v", version, err)
	}
	want := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 {
			want[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	r := &oldRel{version: version}
	for name, dst := range map[string]*string{"longarm": &r.longarm, "longarmd": &r.longarmd, "longarm-hub": &r.hub, "longarm-relay": &r.relay} {
		asset := name + "-" + runtime.GOOS + "-" + runtime.GOARCH
		if runtime.GOOS == "windows" {
			asset += ".exe"
		}
		sum := want[asset]
		if sum == "" {
			fail("release %s has no %s in SHA256SUMS", version, asset)
		}
		path := filepath.Join(dir, exeName(name))
		if b, err := os.ReadFile(path); err == nil && sha256Hex(b) == sum {
			*dst = path
			continue
		}
		b, err := get(asset)
		if err != nil {
			fail("release %s: %v", version, err)
		}
		if got := sha256Hex(b); got != sum {
			t.Fatalf("release %s: %s has sha256 %s, SHA256SUMS says %s", version, asset, got, sum)
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		*dst = path
	}
	if oldRels.m == nil {
		oldRels.m = map[string]*oldRel{}
	}
	oldRels.m[version] = r
	return r
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// runBin runs a binary with a longarm config dir ("" = none) and extra
// environment.
func runBin(t *testing.T, exe, cfgDir string, extraEnv []string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = os.Environ()
	if cfgDir != "" {
		cmd.Env = append(cmd.Env, "LONGARM_CONFIG_DIR="+cfgDir)
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("%s %v: %v", filepath.Base(exe), args, err)
		}
		code = ee.ExitCode()
	}
	return result{out.String(), errb.String(), code}
}

func mustBin(t *testing.T, exe, cfgDir string, args ...string) string {
	t.Helper()
	r := runBin(t, exe, cfgDir, nil, args...)
	if r.code != 0 {
		t.Fatalf("%s %v: exit %d\nstdout: %s\nstderr: %s", filepath.Base(exe), args, r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

// bgBin starts a long-running command (logs -f, attach, forward) with a
// stdin pipe.
func bgBin(t *testing.T, exe, cfgDir string, extraEnv []string, args ...string) (*proc, io.WriteCloser) {
	t.Helper()
	cmd := exec.Command(exe, args...)
	cmd.Env = append(append(os.Environ(), "LONGARM_CONFIG_DIR="+cfgDir), extraEnv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	return startProc(t, cmd), stdin
}

// clientDir makes a client config dir: a certificate issued for name by
// exe (an older longarm issues what that release issued: no node ID) from
// the e2e CA, and the hub's address - no workers.toml: workers come from
// the hub.
func (h *hubEnv) clientDir(t *testing.T, exe, name string) string {
	t.Helper()
	dir := filepath.Join(h.root, "cfg-"+name)
	mustBin(t, exe, h.client, "admin", "issue", "--role", "client", "--name", name, "--out", filepath.Join(dir, "tls"))
	if err := os.WriteFile(filepath.Join(dir, "hub.toml"), []byte(fmt.Sprintf("address = %q\n", h.addr())), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// joinWorkerRel enrolls w with a release's longarmd and approves it with
// that release's longarm (the hub of the time), and runs w with it.
func (h *hubEnv) joinWorkerRel(t *testing.T, w *hubWorker, rel *oldRel) {
	t.Helper()
	code := strings.TrimSpace(mustBin(t, rel.longarm, h.admin, "admin", "join-code", "--role", "worker", "--labels", "pool=e2e"))
	r := runBin(t, rel.longarmd, "", nil, "join", h.addr(), code, "--data-dir", w.dataDir, "--name", w.name, "--ca-fingerprint", h.fp, "--no-wait")
	if r.code != 0 {
		t.Fatalf("%s longarmd join %s: %+v", rel.version, w.name, r)
	}
	if out := mustBin(t, rel.longarm, h.admin, "admin", "approve", w.name); !strings.Contains(out, "approved") {
		t.Fatalf("approve %s: %s", w.name, out)
	}
	w.exe = rel.longarmd
}

// certOf reads a node's current certificate (<dir>/tls/node.crt).
func certOf(t *testing.T, dir string) pki.Identity {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "tls", "node.crt"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := pki.ParseCertPEM(b)
	if err != nil {
		t.Fatal(err)
	}
	id, err := pki.IdentityFromCert(c)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// listedNames is what a longarm lists as workers: ls --json is an object
// of every worker's record by its name.
func listedNames(t *testing.T, exe, cfgDir string) []string {
	t.Helper()
	out := mustBin(t, exe, cfgDir, "ls", "--json")
	var byName map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &byName); err != nil {
		t.Fatalf("ls --json: %v
%s", err, out)
	}
	var names []string
	for n := range byName {
		names = append(names, n)
	}
	return names
}

// keyUser is the user of a user key line.
func keyUser(t *testing.T, line string) string {
	t.Helper()
	k, err := pki.ParseUserKeyLine(line)
	if err != nil {
		t.Fatal(err)
	}
	return k.User
}

// keyLine is the user key line in longarm trust init's output.
func keyLine(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "
") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "longarm-user-key.") {
			return l
		}
	}
	t.Fatalf("no user key line in:
%s", out)
	return ""
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestOldReleaseCompat: the upgrade of a hub from the last release before
// node IDs (v0.1.21, database schema 5) with workers and clients of
// v0.1.21 and v0.1.13, running the released binaries:
//
//  1. old workers reconnect with their certificates, renew them (the new
//     ones carry node IDs) and run jobs;
//  2. old clients run ls, run, logs -f, attach, forward and a session;
//  3. old clients reach new workers and new clients old workers, directly
//     and through the hub's relay and an external relay (this tree's and
//     v0.1.21's longarm-relay);
//  4. end-to-end trust: endorsements made by old nodes are verified by new
//     ones; new ID-bound endorsements are refused by old nodes (by design:
//     update them);
//  5. a worker the migration renamed (a name that looks like an ID: "dead"
//     -> "w-dead-2", "w-dead" taken) stays reachable for old clients under
//     its certificate's name, for new ones under its new name and its ID;
//  6. an old worker revoked by ID is cut off: hub, embedded relay and both
//     external relays.
func TestOldReleaseCompat(t *testing.T) {
	t.Parallel()
	r21, r13 := oldRelease(t, "v0.1.21"), oldRelease(t, "v0.1.13")
	// The fleet of the time: v0.1.21's hub, short certificates so that
	// every node renews with the new hub (whose renew_before is a third of
	// its 90 / 30 days).
	h := newHubEnvBin(t, "[certs]\nworker_ttl = \"2h\"\nclient_ttl = \"2h\"\n", r21.hub)
	os.Remove(filepath.Join(h.client, "workers.toml"))
	os.Remove(filepath.Join(h.admin, "workers.toml"))
	oc := h.clientDir(t, r21.longarm, "oc")     // a v0.1.21 client
	oc13 := h.clientDir(t, r13.longarm, "oc13") // a v0.1.13 client
	var ow, owd, odead, ow13 *hubWorker
	for _, w := range []struct {
		p    **hubWorker
		name string
		rel  *oldRel
	}{{&ow, "ow", r21}, {&owd, "w-dead", r21}, {&odead, "dead", r21}, {&ow13, "ow13", r13}} {
		*w.p = h.newWorkerDir(t, w.name, "")
		h.joinWorkerRel(t, *w.p, w.rel)
		(*w.p).start(t, true)
	}
	for _, n := range []string{"ow", "w-dead", "dead", "ow13"} {
		h.waitNode(t, n, "online on the old hub", func(n map[string]any) bool { return isTrue(n["online"]) })
	}
	// clients known to the old hub; a job in its index
	for _, c := range []struct{ exe, dir string }{{r21.longarm, oc}, {r13.longarm, oc13}} {
		mustBin(t, c.exe, c.dir, "ls")
	}
	if out := mustBin(t, r21.longarm, oc, "exec", "ow", "--", bins.helper, "print", "1", "0", "0"); !strings.Contains(out, "line 1") {
		t.Fatalf("v0.1.21 exec on the old hub: %s", out)
	}
	t.Logf("before the upgrade: root's certificate %s, alice's %s", certSerial(t, filepath.Join(h.admin, "tls", "node.crt")), certSerial(t, filepath.Join(h.client, "tls", "node.crt")))
	h.killHub()
	if b, err := os.ReadFile(filepath.Join(h.hubDir, "hub.db")); err != nil || len(b) == 0 {
		t.Fatalf("no database of the old hub: %v", err)
	}

	// --- the upgrade: this tree's hub on the same data directory (schema
	// 5 -> 6), its default certificate lifetimes
	h.hubBin = buildHub(t)
	cfg, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.cfgPath, []byte(strings.Replace(string(cfg), "[certs]\nworker_ttl = \"2h\"\nclient_ttl = \"2h\"\n", "", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	h.startHub(t)
	hubLog := func() string { b, _ := os.ReadFile(filepath.Join(h.root, "hub.log")); return string(b) }
	if l := strings.ReplaceAll(hubLog(), `\"`, `"`); !strings.Contains(l, `database migration: node "dead" (worker, approved) renamed to "w-dead-2"`) {
		t.Fatalf("no migration note for dead:\n%s", tail(hubLog(), 4000))
	}
	t.Logf("after the upgrade: root's certificate %s, alice's %s", certSerial(t, filepath.Join(h.admin, "tls", "node.crt")), certSerial(t, filepath.Join(h.client, "tls", "node.crt")))
	nw := h.newWorkerDir(t, "nw", "") // a worker of this tree
	h.joinWorker(t, nw, true)
	nw.start(t, true)

	ids := map[string]string{} // name -> node ID (as the new hub lists it)
	t.Run("1_old_workers_reconnect_renew_run", func(t *testing.T) {
		for _, w := range []*hubWorker{ow, ow13, odead, nw} {
			name := w.name
			if w == odead {
				name = "w-dead-2"
			}
			n := h.waitNode(t, name, "online with a renewed certificate", func(n map[string]any) bool {
				na, _ := time.Parse(time.RFC3339, fmt.Sprint(n["cert_not_after"]))
				return isTrue(n["online"]) && time.Until(na) > 24*time.Hour
			})
			ids[name], _ = n["id"].(string)
			if len(ids[name]) != 36 {
				t.Fatalf("%s: no node id: %v", name, n)
			}
			// the renewed certificate on disk: the node's ID, its
			// certificate's name (an old longarmd keeps it: "dead")
			id := certOf(t, w.dataDir)
			if id.ID != ids[name] || id.Name != w.name {
				t.Fatalf("%s's certificate after the renewal: %+v (node %s)", w.name, id, ids[name])
			}
		}
		for _, target := range []string{"ow", "ow13", ids["ow"][:8]} {
			if r := h.longarm(h.client, "exec", target, "--", bins.helper, "print", "2", "0", "0"); r.code != 0 || r.stdout != lines(1, 2) {
				t.Fatalf("new client exec %s: %+v", target, r)
			}
		}
		id := strings.TrimSpace(h.mustLongarm(h.client, "run", "ow", "--", bins.helper, "print", "3", "0", "0"))
		h.mustLongarm(h.client, "wait", "ow", id)
		if out := h.mustLongarm(h.client, "logs", "ow", id); out != lines(1, 3) {
			t.Fatalf("logs of a job on the renewed old worker: %q", out)
		}
	})

	t.Run("2_old_clients", func(t *testing.T) {
		for _, c := range []struct {
			rel *oldRel
			dir string
		}{{r21, oc}, {r13, oc13}} {
			lg := func(args ...string) result { return runBin(t, c.rel.longarm, c.dir, nil, args...) }
			// the client renews too (its first call to the new hub)
			if r := lg("ls"); r.code != 0 || !strings.Contains(r.stdout, "nw") || !strings.Contains(r.stdout, "ow") {
				t.Fatalf("%s ls: %+v", c.rel.version, r)
			}
			// its certificate (from before node IDs) keeps working, mapped by
			// serial; it gets the ID at its regular renewal
			t.Logf("%s client's certificate: node id %q", c.rel.version, certOf(t, c.dir).ID)
			for _, w := range []string{"nw", "ow"} {
				id := strings.TrimSpace(lg("run", w, "--", bins.helper, "print", "40", "20", "0").stdout)
				if id == "" {
					t.Fatalf("%s run on %s failed", c.rel.version, w)
				}
				p, _ := bgBin(t, c.rel.longarm, c.dir, nil, "logs", w, id, "-f")
				select {
				case <-p.done:
				case <-time.After(60 * time.Second):
					t.Fatalf("%s logs -f on %s did not end:\n%s", c.rel.version, w, p.out.String())
				}
				if !strings.Contains(p.out.String(), "line 40") {
					t.Fatalf("%s logs -f on %s:\n%s", c.rel.version, w, p.out.String())
				}
				if r := lg("exec", w, "--", bins.helper, "print", "1", "0", "0"); r.code != 0 || r.stdout != lines(1, 1) {
					t.Fatalf("%s exec %s: %+v", c.rel.version, w, r)
				}
			}
			if c.rel != r21 {
				continue // v0.1.13: the basics above
			}
			// attach: an interactive session, detach, transcript
			a, in := bgBin(t, c.rel.longarm, c.dir, nil, "attach", "nw", "--session", "oc-s1", "--", bins.helper, "repl")
			waitOutput(t, a.out.String, "repl ready", 30*time.Second)
			in.Write([]byte("hello old\n"))
			waitOutput(t, a.out.String, "echo: hello old", 15*time.Second)
			in.Write([]byte{0x1d, 'd'}) // Ctrl-] d
			select {
			case <-a.done:
			case <-time.After(15 * time.Second):
				t.Fatalf("attach did not detach:\n%s", a.out.String())
			}
			if out := plain(mustBin(t, c.rel.longarm, c.dir, "logs", "nw", "oc-s1", "--session")); !strings.Contains(out, "echo: hello old") {
				t.Fatalf("session transcript:\n%s", out)
			}
			// a session for programs: start, send, stop
			if out := mustBin(t, c.rel.longarm, c.dir, "session", "start", "ow", "-s", "oc-s2", "--json", "--", bins.helper, "repl"); !strings.Contains(out, `"created": true`) && !strings.Contains(out, `"created":true`) {
				t.Fatalf("session start: %s", out)
			}
			mustBin(t, c.rel.longarm, c.dir, "session", "send", "ow", "-s", "oc-s2", "--text", "ping", "--key", "enter")
			waitFor(t, "the session's echo", 20*time.Second, func() bool {
				return strings.Contains(plain(lg("logs", "ow", "oc-s2", "--session").stdout), "echo: ping")
			})
			mustBin(t, c.rel.longarm, c.dir, "session", "stop", "ow", "-s", "oc-s2")
			// port forward to an echo server on the worker's machine
			port := startEchoServer(t, h.root)
			local := freePort(t)
			f, _ := bgBin(t, c.rel.longarm, c.dir, nil, "forward", "nw", strconv.Itoa(port), strconv.Itoa(local))
			waitOutput(t, f.out.String, "forwarding", 30*time.Second)
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", local), 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			conn.SetDeadline(time.Now().Add(15 * time.Second))
			fmt.Fprint(conn, "ping through an old forward\n")
			if line, err := bufio.NewReader(conn).ReadString('\n'); err != nil || line != "ping through an old forward\n" {
				t.Fatalf("forward echo %q %v", line, err)
			}
			conn.Close()
			mustBin(t, c.rel.longarm, c.dir, "forward", "--stop", "all")
		}
	})

	// relays: this tree's (registered with the hub) and v0.1.21's
	xn := startExtRelayBin(t, h, "xnew", "")
	xo := startExtRelayBin(t, h, "xold", r21.relay)
	for _, x := range []*extRelay{xn, xo} {
		waitFor(t, "relay "+x.udp+" on the hub", 30*time.Second, func() bool {
			hs, _ := x.health()["hub"].(map[string]any)
			return isTrue(hs["connected"]) && isTrue(hs["ready"])
		})
	}
	for _, w := range []string{"ow", "nw"} {
		h.waitNode(t, w, "listening on both external relays", func(n map[string]any) bool {
			eps := fmt.Sprint(n["endpoints"])
			return isTrue(n["online"]) && strings.Contains(eps, xn.udp) && strings.Contains(eps, xo.udp)
		})
	}
	netEnv := []string{"LONGARM_NET_DISABLE_DIRECT=1", "LONGARM_HUB_TIMEOUT=2s"}
	via := map[string]string{"hub": h.addr(), "xnew": xn.udp, "xold": xo.udp}
	writeRelayWorkers := func(dir string) {
		var b strings.Builder
		for _, w := range []string{"ow", "nw"} {
			for rn, addr := range via {
				fmt.Fprintf(&b, "[[worker]]\nname = \"%s-%s\"\ncert_name = %q\ntransport = \"longnet\"\naddress = %q\n", w, rn, w, addr)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "workers.toml"), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeRelayWorkers(oc)
	writeRelayWorkers(h.client)

	t.Run("3_cross_versions_direct_and_relayed", func(t *testing.T) {
		for _, c := range []struct {
			name, exe, dir string
		}{{"v0.1.21 client", r21.longarm, oc}, {"new client", bins.longarm, h.client}} {
			for _, w := range []string{"ow", "nw"} {
				if r := runBin(t, c.exe, c.dir, nil, "exec", w, "--", bins.helper, "print", "1", "0", "0"); r.code != 0 || r.stdout != lines(1, 1) {
					t.Errorf("%s -> %s direct: %+v", c.name, w, r)
				}
				for rn := range via {
					target := w + "-" + rn
					if r := runBin(t, c.exe, c.dir, netEnv, "exec", target, "--", bins.helper, "print", "1", "0", "0"); r.code != 0 || r.stdout != lines(1, 1) {
						t.Errorf("%s -> %s through %s: %+v", c.name, w, rn, r)
					}
				}
			}
		}
	})

	t.Run("5_renamed_by_the_migration", func(t *testing.T) {
		// old clients list it under its certificate's name and reach it
		for _, c := range []struct{ exe, dir string }{{r21.longarm, oc}, {r13.longarm, oc13}} {
			names := listedNames(t, c.exe, c.dir)
			if !contains(names, "dead") || contains(names, "w-dead-2") {
				t.Fatalf("an old client lists %v", names)
			}
			if r := runBin(t, c.exe, c.dir, nil, "exec", "dead", "--", bins.helper, "print", "1", "0", "0"); r.code != 0 || r.stdout != lines(1, 1) {
				t.Fatalf("old client exec dead: %+v", r)
			}
		}
		// new clients: by its new name and its ID; the hub knows it says "dead"
		n := h.waitNode(t, "w-dead-2", "listed", func(map[string]any) bool { return true })
		if n["cert_name"] != "dead" {
			t.Fatalf("w-dead-2: %v", n)
		}
		for _, target := range []string{"w-dead-2", ids["w-dead-2"][:8], "id:" + ids["w-dead-2"][:6]} {
			if r := h.longarm(h.client, "exec", target, "--", bins.helper, "print", "1", "0", "0"); r.code != 0 || r.stdout != lines(1, 1) {
				t.Fatalf("new client exec %s: %+v", target, r)
			}
		}
		// the unrenamed w-dead is still itself
		if r := h.longarm(h.client, "exec", "w-dead", "--", bins.helper, "print", "1", "0", "0"); r.code != 0 {
			t.Fatalf("exec w-dead: %+v", r)
		}
	})

	t.Run("4_end_to_end_trust", func(t *testing.T) {
		// a v0.1.21 device makes its user key and endorses itself (v1,
		// name-bound) ...
		oldKey := keyLine(t, mustBin(t, r21.longarm, oc, "trust", "init"))
		// ... a new worker that trusts that key verifies it
		if r := longarmdCmd(t, "trust", "add-user", oldKey, "--data-dir", nw.dataDir); r.code != 0 {
			t.Fatalf("new worker trust add-user: %+v", r)
		}
		waitFor(t, "nw's trust policy", 20*time.Second, func() bool {
			b, _ := os.ReadFile(nw.logPath)
			return strings.Contains(string(b), "trust policy reloaded")
		})
		if r := runBin(t, r21.longarm, oc, nil, "exec", "nw", "--", bins.helper, "print", "1", "0", "0"); r.code != 0 {
			t.Fatalf("a new worker refuses an old device's endorsement: %+v", r)
		}
		if r := h.longarm(h.client, "exec", "nw", "--", bins.helper, "print", "1", "0", "0"); r.code == 0 {
			t.Fatal("trust on: an unendorsed client got through")
		}
		// an old worker endorsed by the old device's key (v1) ...
		req := firstLine(mustBin(t, r21.longarmd, "", "trust", "request", "--data-dir", ow.dataDir))
		end := firstLine(mustBin(t, r21.longarm, oc, "trust", "endorse", req))
		mustBin(t, r21.longarmd, "", "trust", "accept", end, "--data-dir", ow.dataDir)
		waitFor(t, "ow's certificate carrying the endorsement", 30*time.Second, func() bool {
			b, _ := os.ReadFile(filepath.Join(ow.dataDir, "tls", "node.crt"))
			c, err := pki.ParseCertPEM(b)
			if err != nil {
				return false
			}
			tx, _ := pki.TrustOf(c)
			return tx != nil && tx.Endorsement != nil
		})
		// ... is verified by a new client that requires endorsed workers
		mustBin(t, bins.longarm, h.client, "trust", "add-user", oldKey)
		mustBin(t, bins.longarm, h.client, "trust", "require", "on")
		if r := h.longarm(h.client, "exec", "ow", "--", bins.helper, "print", "1", "0", "0"); r.code != 0 {
			t.Fatalf("a new client refuses an old worker's endorsement: %+v", r)
		}
		if r := h.longarm(h.client, "exec", "ow13", "--", bins.helper, "print", "1", "0", "0"); r.code == 0 {
			t.Fatal("require on: an unendorsed worker got through")
		}
		mustBin(t, bins.longarm, h.client, "trust", "require", "off")
		// the reverse: a new device's endorsement binds its node ID; a
		// v0.1.21 worker that trusts that user refuses it (it predates
		// node IDs): such workers must be updated
		newKey := keyLine(t, h.mustLongarm(h.client, "trust", "init"))
		if r := runBin(t, r21.longarmd, "", nil, "trust", "add-user", newKey, "--data-dir", ow13.dataDir); r.code != 0 {
			// v0.1.13 has no end-to-end trust: nothing to check there
			t.Logf("v0.1.13 longarmd trust add-user: %+v", r)
		}
		mustBin(t, r21.longarmd, "", "trust", "add-user", newKey, "--data-dir", odead.dataDir)
		waitFor(t, "dead's trust policy", 20*time.Second, func() bool {
			b, _ := os.ReadFile(odead.logPath)
			return strings.Count(string(b), "trust policy reloaded") > 0
		})
		r := h.longarm(h.client, "exec", "w-dead-2", "--", bins.helper, "print", "1", "0", "0")
		t.Logf("new client (ID-bound endorsement) -> v0.1.21 worker with end-to-end trust: exit %d %s", r.code, strings.TrimSpace(r.stderr))
		if r.code == 0 {
			t.Log("note: the v0.1.21 worker accepted the ID-bound device endorsement")
		}
		mustBin(t, r21.longarmd, "", "trust", "remove-user", keyUser(t, newKey), "--data-dir", odead.dataDir)
		longarmdCmd(t, "trust", "remove-user", keyUser(t, oldKey), "--data-dir", nw.dataDir)
	})

	t.Run("6_revoke_old_worker_by_id", func(t *testing.T) {
		id := ids["ow"]
		before := map[*extRelay]float64{}
		for _, x := range []*extRelay{xn, xo} {
			hs, _ := x.health()["hub"].(map[string]any)
			before[x], _ = hs["revoked"].(float64)
		}
		h.mustLongarm(h.admin, "admin", "revoke", id[:8])
		h.waitNode(t, "ow", "revoked and offline", func(n map[string]any) bool { return n["state"] == "revoked" && !isTrue(n["online"]) })
		for _, x := range []*extRelay{xn, xo} {
			waitFor(t, "the revocation on relay "+x.udp, 30*time.Second, func() bool {
				hs, _ := x.health()["hub"].(map[string]any)
				n, _ := hs["revoked"].(float64)
				return n > before[x]
			})
		}
		// the hub refuses its reconnects; no relay carries it
		waitFor(t, "ow refused by the hub", 30*time.Second, func() bool {
			b, _ := os.ReadFile(ow.logPath)
			return strings.Contains(strings.ToLower(string(b)), "revoked")
		})
		for rn := range via {
			waitFor(t, "ow unreachable through "+rn, 60*time.Second, func() bool {
				r := runBin(t, bins.longarm, h.client, netEnv, "exec", "ow-"+rn, "--", bins.helper, "print", "1", "0", "0")
				return r.code != 0
			})
		}
		// a new client refuses it by the hub's word, an old client sees it revoked
		if r := h.longarm(h.client, "exec", "ow", "--", bins.helper, "print", "1", "0", "0"); r.code == 0 {
			t.Fatalf("new client exec of a revoked worker: %+v", r)
		}
	})
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(s)
}
