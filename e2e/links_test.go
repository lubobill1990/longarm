package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/client"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Port links end to end (docs/usage.md "Port links"): a hub, two workers
// (wa = "from", wb = "to") and clients, all real processes over real
// sockets.

var (
	linkIDRE   = regexp.MustCompile(`lk-[0-9a-f]{12}`)
	linkPortRE = regexp.MustCompile(`port (\d+)`)
)

// linkUser issues a client certificate of user (device name) in a fresh
// config dir that knows only the hub.
func linkUser(t *testing.T, h *hubEnv, name, user string) string {
	t.Helper()
	dir := filepath.Join(h.root, "cfg-"+name)
	args := []string{"admin", "issue", "--role", "client", "--name", name, "--out", filepath.Join(dir, "tls"), "--ca-dir", filepath.Join(h.client, "ca")}
	if user != "" {
		args = append(args, "--user", user)
	}
	h.mustLongarm(h.client, args...)
	if err := os.WriteFile(filepath.Join(dir, "hub.toml"), []byte(fmt.Sprintf("address = %q\n", h.addr())), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// jobPort runs `helper <mode> 600` on worker as cfg and returns the port it
// prints and the job id.
func jobPort(t *testing.T, h *hubEnv, cfg, worker string, args ...string) (string, string) {
	t.Helper()
	id := strings.TrimSpace(h.mustLongarm(cfg, append([]string{"run", worker, "--"}, args...)...))
	var port string
	waitFor(t, "the server's port on "+worker, 60*time.Second, func() bool {
		r := h.longarm(cfg, "logs", id)
		if m := linkPortRE.FindStringSubmatch(r.stdout); m != nil {
			port = m[1]
			return true
		}
		return false
	})
	return port, id
}

// linkCreate creates a link and returns its id.
func linkCreate(t *testing.T, h *hubEnv, cfg string, args ...string) string {
	t.Helper()
	out := h.mustLongarm(cfg, append([]string{"link", "create"}, args...)...)
	id := linkIDRE.FindString(out)
	if id == "" {
		t.Fatalf("link create: %s", out)
	}
	return id
}

// linkGet runs helper http-get on worker as cfg (addr may be "$VAR").
func linkGet(h *hubEnv, cfg, worker, helper, addr, path string) result {
	return h.longarm(cfg, "exec", worker, "--", helper, "http-get", addr, path)
}

// linkStatus returns the link's line of `longarm link ls`.
func linkStatus(t *testing.T, h *hubEnv, cfg, id string) string {
	t.Helper()
	for _, l := range strings.Split(h.longarm(cfg, "link", "ls").stdout, "\n") {
		if strings.HasPrefix(l, id) {
			return l
		}
	}
	return ""
}

// TestPortLinks: alice's HTTP server on wb, reached from her job on wa
// through a link (and its environment variables); the status; bob can't
// see, remove or squat it and gets no variables; the target refuses
// anything but a granted worker; links survive a hub restart and work
// while the hub is down; a link bound to a job ends with it; removal cuts
// open connections and refuses new ones; audit on the hub and the target.
func TestPortLinks(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	alice := h.client
	os.Remove(filepath.Join(alice, "workers.toml")) // workers come from the hub
	h.mustLongarm(h.admin, "admin", "user", "add", "alice", "--no-code")
	h.mustLongarm(h.admin, "admin", "user", "add", "bob", "--no-code")
	bob := linkUser(t, h, "bob-laptop", "bob")
	wa := h.newWorkerDir(t, "wa", "")
	wb := h.newWorkerDir(t, "wb", "")
	for _, w := range []*hubWorker{wa, wb} {
		h.joinWorker(t, w, true)
		w.start(t, true)
	}
	for _, n := range []string{"wa", "wb"} {
		h.waitNode(t, n, "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	}

	port, srvJob := jobPort(t, h, alice, "wb", bins.helper, "http-serve", "600")
	lport := strconv.Itoa(freePort(t))
	id := linkCreate(t, h, alice, "wb:"+port, "--on", "wa", "--port", lport)
	envVar := "LONGARM_LINK_WB_" + port

	t.Run("reach_through_link", func(t *testing.T) {
		var r result
		waitFor(t, "alice's server through the link", 30*time.Second, func() bool {
			r = linkGet(h, alice, "wa", bins.helper, "$"+envVar, "/x")
			return r.code == 0 && strings.Contains(r.stdout, "status 200: hello /x from")
		})
		r = h.longarm(alice, "exec", "wa", "--", bins.helper, "env", "LONGARM_LINKS")
		if want := fmt.Sprintf("LONGARM_LINKS=wb:%s=127.0.0.1:%s", port, lport); !strings.Contains(r.stdout, want) {
			t.Fatalf("env: %q, want %q", r.stdout, want)
		}
		// a job (not only exec) gets the variables too
		jid := strings.TrimSpace(h.mustLongarm(alice, "run", "wa", "--", bins.helper, "http-get", "$"+envVar, "/job"))
		h.longarm(alice, "wait", jid)
		if out := h.longarm(alice, "logs", jid).stdout; !strings.Contains(out, "hello /job") {
			t.Fatalf("job through the link: %q", out)
		}
		waitFor(t, "link ls: listening with traffic", 20*time.Second, func() bool {
			l := linkStatus(t, h, alice, id)
			return strings.Contains(l, "listening") && !strings.Contains(l, " 0 (")
		})
		if l := linkStatus(t, h, alice, id); !strings.Contains(l, "tcp/direct") || !strings.Contains(l, "wb:"+port) {
			t.Fatalf("link ls: %s", l)
		}
	})

	t.Run("other_users", func(t *testing.T) {
		if out := h.mustLongarm(bob, "link", "ls"); strings.Contains(out, id) {
			t.Fatalf("bob sees alice's link: %s", out)
		}
		if r := h.longarm(bob, "link", "rm", id); r.code == 0 || !strings.Contains(r.stderr, "NotFound") && !strings.Contains(r.stderr, "no link") {
			t.Fatalf("bob removes alice's link: %+v", r)
		}
		if r := h.longarm(bob, "link", "create", "wb:"+port, "--on", "wa", "--port", lport); r.code == 0 || strings.Contains(r.stderr, id) {
			t.Fatalf("bob squats alice's port: %+v", r)
		}
		if r := h.longarm(bob, "exec", "wa", "--", bins.helper, "env", "LONGARM_LINKS"); strings.TrimSpace(r.stdout) != "LONGARM_LINKS=" {
			t.Fatalf("bob's environment: %+v", r)
		}
		// the admin sees everybody's links
		if out := h.mustLongarm(h.admin, "link", "ls", "--all"); !strings.Contains(out, id) {
			t.Fatalf("admin ls --all: %s", out)
		}
		// the target accepts LinkTunnel.Dial from workers with a grant only:
		// bob's client certificate can't call it at all
		cfg, err := client.LoadConfig(bob)
		if err != nil {
			t.Fatal(err)
		}
		d, err := client.NewDialer(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		conn, err := d.Dial(client.Worker{Name: "wb", CertName: "wb", Transport: "tcp", Address: fmt.Sprintf("127.0.0.1:%d", wb.port)})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		st, err := pb.NewLinkTunnelClient(conn.ClientConn).Dial(ctx)
		if err == nil {
			st.Send(&pb.ForwardFrame{})
			_, err = st.Recv()
		}
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("bob calls LinkTunnel.Dial: %v", err)
		}
	})

	t.Run("hub_restart_and_down", func(t *testing.T) {
		h.killHub()
		// the data plane needs no hub
		if r := linkGet(h, alice, "wa", bins.helper, "127.0.0.1:"+lport, "/hubdown"); !strings.Contains(r.stdout, "hello /hubdown") {
			r2 := h.longarm(alice, "exec", "wa", "--", bins.helper, "env", envVar)
			t.Fatalf("with the hub down: %+v (%s)", r, r2.stdout)
		}
		h.startHub(t)
		h.waitNode(t, "wa", "online again", func(n map[string]any) bool { return isTrue(n["online"]) })
		if l := linkStatus(t, h, alice, id); l == "" {
			t.Fatal("link lost by the hub restart")
		}
		waitFor(t, "the link after the hub restart", 30*time.Second, func() bool {
			return strings.Contains(linkGet(h, alice, "wa", bins.helper, "$"+envVar, "/again").stdout, "hello /again")
		})
	})

	t.Run("job_bound", func(t *testing.T) {
		job := strings.TrimSpace(h.mustLongarm(alice, "run", "wa", "--", bins.helper, "sleep", "300"))
		waitFor(t, "the job in the hub index", 20*time.Second, func() bool {
			return strings.Contains(h.longarm(alice, "jobs", "--state", "running").stdout, job)
		})
		jl := linkCreate(t, h, alice, "wb:"+port, "--on", "wa", "--port", strconv.Itoa(freePort(t)), "--job", job)
		h.mustLongarm(alice, "kill", job)
		waitFor(t, "the job's link to go", 30*time.Second, func() bool { return linkStatus(t, h, alice, jl) == "" })
	})

	t.Run("removal_cuts", func(t *testing.T) {
		portFile := filepath.Join(h.root, "echo.port")
		echoJob := strings.TrimSpace(h.mustLongarm(alice, "run", "wb", "--", bins.helper, "echo-server", portFile))
		var echoPort string
		waitFor(t, "echo server", 30*time.Second, func() bool { b, _ := os.ReadFile(portFile); echoPort = string(b); return echoPort != "" })
		lp := strconv.Itoa(freePort(t))
		el := linkCreate(t, h, alice, "wb:"+echoPort, "--on", "wa", "--port", lp)
		var holdJob string
		waitFor(t, "a long connection through the link", 60*time.Second, func() bool {
			if holdJob != "" {
				out := h.longarm(alice, "logs", holdJob).stdout
				if strings.Contains(out, "up") {
					return true
				}
				if !strings.Contains(out, "cut") {
					return false
				}
			}
			holdJob = strings.TrimSpace(h.mustLongarm(alice, "run", "wa", "--", bins.helper, "hold", "127.0.0.1:"+lp))
			return false
		})
		h.mustLongarm(alice, "link", "rm", el)
		waitFor(t, "the open connection to be cut", 30*time.Second, func() bool {
			return strings.Contains(h.longarm(alice, "logs", holdJob).stdout, "cut")
		})
		waitFor(t, "new connections refused", 30*time.Second, func() bool {
			r := h.longarm(alice, "exec", "wa", "--", bins.helper, "connect", lp)
			return r.code != 0
		})
		h.longarm(alice, "kill", echoJob)
	})

	t.Run("audit", func(t *testing.T) {
		out := h.mustLongarm(h.admin, "admin", "audit", "--node", "hub", "--json", "--limit", "200")
		for _, want := range []string{`"links/create"`, `"links/remove"`, id} {
			if !strings.Contains(out, want) {
				t.Errorf("hub audit lacks %s", want)
			}
		}
		waitFor(t, "the target's audit on the hub", 30*time.Second, func() bool {
			out := h.longarm(h.admin, "admin", "audit", "--node", "wb", "--json", "--limit", "500").stdout
			return strings.Contains(out, "LinkTunnel/Dial") && strings.Contains(out, id) && strings.Contains(out, `"worker/wa"`)
		})
	})

	h.mustLongarm(alice, "link", "rm", id)
	if r := linkGet(h, alice, "wa", bins.helper, "127.0.0.1:"+lport, "/gone"); r.code == 0 {
		t.Fatalf("link still answers after rm: %+v", r)
	}
	h.longarm(alice, "kill", srvJob)
}

// TestPortLinksRelay: workers that reach each other only through the
// hub's relay (longnet legs, hole punching off) - the relay carries
// worker<->worker traffic for linked pairs.
func TestPortLinksRelay(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	alice := h.client
	os.Remove(filepath.Join(alice, "workers.toml"))
	var ws []*hubWorker
	for _, n := range []string{"ra", "rb"} {
		w := h.newWorkerDir(t, n, "")
		toml := fmt.Sprintf("listen = [\"longnet://%s?name=%s\"]\n", h.addr(), n)
		if err := os.WriteFile(filepath.Join(w.dataDir, "longarmd.toml"), []byte(toml), 0o600); err != nil {
			t.Fatal(err)
		}
		w.env = []string{"LONGARM_NET_DISABLE_DIRECT=1"}
		h.joinWorker(t, w, true)
		w.start(t, true)
		ws = append(ws, w)
	}
	for _, n := range []string{"ra", "rb"} {
		h.waitNode(t, n, "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	}
	port, srvJob := jobPort(t, h, alice, "rb", bins.helper, "http-serve", "600")
	lport := strconv.Itoa(freePort(t))
	id := linkCreate(t, h, alice, "rb:"+port, "--on", "ra", "--port", lport)
	waitFor(t, "alice's server through the relayed link", 60*time.Second, func() bool {
		r := linkGet(h, alice, "ra", bins.helper, "$LONGARM_LINK_RB_"+port, "/relay")
		return strings.Contains(r.stdout, "hello /relay")
	})
	waitFor(t, "link ls: longnet relay path", 20*time.Second, func() bool {
		return strings.Contains(linkStatus(t, h, alice, id), "longnet/relay")
	})
	h.mustLongarm(alice, "link", "rm", id)
	h.longarm(alice, "kill", srvJob)
}

var refusedRE = regexp.MustCompile(`(\d+) refused`)

// TestPerUserLinks: port links between two workers with per-user
// isolation (opt-in like TestPerUserIsolation: LONGARM_PERUSER_TEST=1,
// elevated / root). On "from" only the owner's processes may use the
// listener; on "to" the target port must be the owner's (like
// Forward.Open).
func TestPerUserLinks(t *testing.T) {
	requirePerUser(t)
	h := newHubEnv(t, "")
	a := newPerUserWorkerNamed(t, h, "w1", "")
	b := newPerUserWorkerNamed(t, h, "w2", "")
	alice := h.client
	os.Remove(filepath.Join(alice, "workers.toml")) // both workers come from the hub
	bob := linkUser(t, h, "bob-laptop", "bob")
	for _, n := range []string{"w1", "w2"} {
		h.waitNode(t, n, "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	}
	h.mustLongarm(h.admin, "admin", "user", "add", "alice", "--no-code")
	h.mustLongarm(h.admin, "admin", "user", "add", "bob", "--no-code")
	for _, w := range []*puWorker{a, b} {
		waitFor(t, "the tenancy policy on the worker", 30*time.Second, func() bool {
			b, err := os.ReadFile(filepath.Join(w.dataDir, "tenancy.json"))
			return err == nil && strings.Contains(string(b), "bob")
		})
	}

	portA, jobA := jobPort(t, h, alice, "w2", b.helper, "http-serve", "600") // alice's server (as her account on w2)
	portB, jobB := jobPort(t, h, bob, "w2", b.helper, "http-serve", "600")   // bob's
	lport := strconv.Itoa(freePort(t))
	id := linkCreate(t, h, alice, "w2:"+portA, "--on", "w1", "--port", lport)

	// alice's process on w1 reaches her server on w2 - which runs as her account there
	var r result
	waitFor(t, "alice's server through the link", 60*time.Second, func() bool {
		r = linkGet(h, alice, "w1", a.helper, "$LONGARM_LINK_W2_"+portA, "/pu")
		return strings.Contains(r.stdout, "status 200: hello /pu from")
	})
	if !strings.Contains(strings.ToLower(r.stdout), b.account("alice")) {
		t.Fatalf("the server does not run as alice's account on w2: %q", r.stdout)
	}

	// bob's process on w1 can't use alice's listener
	r = linkGet(h, bob, "w1", a.helper, "127.0.0.1:"+lport, "/bob")
	if r.code == 0 || strings.Contains(r.stdout, "hello") {
		t.Fatalf("bob used alice's link: %+v", r)
	}
	waitFor(t, "the refusal in link ls", 20*time.Second, func() bool {
		m := refusedRE.FindStringSubmatch(linkStatus(t, h, alice, id))
		return m != nil && m[1] != "0"
	})

	// alice can't link to bob's server: w2 refuses (not her port)
	lport2 := strconv.Itoa(freePort(t))
	id2 := linkCreate(t, h, alice, "w2:"+portB, "--on", "w1", "--port", lport2)
	waitFor(t, "w2 refusing alice's link to bob's port", 60*time.Second, func() bool {
		r := linkGet(h, alice, "w1", a.helper, "127.0.0.1:"+lport2, "/steal")
		if strings.Contains(r.stdout, "hello") {
			t.Fatalf("alice reached bob's server: %+v", r)
		}
		return strings.Contains(linkStatus(t, h, alice, id2), "not listened on by a process of yours")
	})
	// and bob can't take alice's listen port
	if r := h.longarm(bob, "link", "create", "w2:"+portB, "--on", "w1", "--port", lport); r.code == 0 {
		t.Fatalf("bob squats alice's port: %+v", r)
	}
	// audit on w2: the refusal
	waitFor(t, "w2's audit of the refused link", 30*time.Second, func() bool {
		out := h.longarm(h.admin, "admin", "audit", "--node", "w2", "--json", "--limit", "500").stdout
		return strings.Contains(out, id2) && strings.Contains(out, "denied")
	})
	h.mustLongarm(alice, "link", "rm", id, id2)
	h.longarm(alice, "kill", jobA)
	h.longarm(bob, "kill", jobB)
}
