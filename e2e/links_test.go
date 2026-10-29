package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/trust"
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
	// expires while the hub is down (hub_restart_and_down; a minute is the
	// shortest TTL, so it is created this early)
	ttlExpires := time.Now().Add(time.Minute)
	ttlPort := strconv.Itoa(freePort(t))
	linkCreate(t, h, alice, "wb:"+strconv.Itoa(freePort(t)), "--on", "wa", "--port", ttlPort, "--ttl", "1m") // another target: no clash with envVar

	t.Run("reach_through_link", func(t *testing.T) {
		var r result
		waitFor(t, "alice's server through the link", 30*time.Second, func() bool {
			r = linkGet(h, alice, "wa", bins.helper, "$"+envVar, "/x")
			return r.code == 0 && strings.Contains(r.stdout, "status 200: hello /x from")
		})
		r = h.longarm(alice, "exec", "wa", "--", bins.helper, "env", "LONGARM_LINKS")
		// (with the 1m link's entry: LONGARM_LINKS lists all, by target)
		if want := fmt.Sprintf("wb:%s=127.0.0.1:%s", port, lport); !strings.HasPrefix(r.stdout, "LONGARM_LINKS=") || !strings.Contains(r.stdout, want) {
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
		// direct: wa reaches wb over its tcp leg or longnet's direct path,
		// whichever wins the race (on macOS runners longnet often does)
		if l := linkStatus(t, h, alice, id); !regexp.MustCompile(`(tcp|longnet)/direct`).MatchString(l) || !strings.Contains(l, "wb:"+port) {
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
		// an admin sees its own user's links only, like any user
		if out := h.mustLongarm(h.admin, "link", "ls", "--all"); strings.Contains(out, id) {
			t.Fatalf("admin ls --all shows alice's link: %s", out)
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
		// a link bound to a job that ends while the hub is down
		job := strings.TrimSpace(h.mustLongarm(alice, "run", "wa", "--", bins.helper, "sleep", "300"))
		waitFor(t, "the job in the hub index", 20*time.Second, func() bool {
			return strings.Contains(h.longarm(alice, "jobs", "--state", "running").stdout, job)
		})
		jobPort := strconv.Itoa(freePort(t))
		linkCreate(t, h, alice, "wb:"+port, "--on", "wa", "--port", jobPort, "--job", job)
		waitFor(t, "the job's link", 30*time.Second, func() bool {
			return strings.Contains(linkGet(h, alice, "wa", bins.helper, "127.0.0.1:"+jobPort, "/job").stdout, "hello /job")
		})
		// closed: wa's helper ran and nothing listens on the port any more
		closed := func(lp string) bool {
			r := linkGet(h, alice, "wa", bins.helper, "127.0.0.1:"+lp, "/gone")
			return strings.Contains(r.stdout, "error:") && strings.Contains(strings.ToLower(r.stdout), "refused")
		}
		h.killHub()
		// the data plane needs no hub
		if r := linkGet(h, alice, "wa", bins.helper, "127.0.0.1:"+lport, "/hubdown"); !strings.Contains(r.stdout, "hello /hubdown") {
			r2 := h.longarm(alice, "exec", "wa", "--", bins.helper, "env", envVar)
			t.Fatalf("with the hub down: %+v (%s)", r, r2.stdout)
		}
		// without the hub, wa ends the job's link when the job ends ...
		h.mustLongarm(alice, "kill", job)
		waitFor(t, "the job's link to close with the hub down", 30*time.Second, func() bool { return closed(jobPort) })
		// ... and both workers end a link at its TTL
		waitFor(t, "the 1m link to expire with the hub down", time.Until(ttlExpires)+30*time.Second, func() bool { return closed(ttlPort) })
		if time.Now().Before(ttlExpires) {
			t.Fatal("the 1m link closed before its TTL")
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
		// the complete trail: the hub's operators (longarm-hub audit)
		out := h.hubCmd(t, "audit", "--node", "hub", "--json", "--limit", "200")
		for _, want := range []string{`"links/create"`, `"links/remove"`, id} {
			if !strings.Contains(out, want) {
				t.Errorf("hub audit lacks %s", want)
			}
		}
		waitFor(t, "the target's audit on the hub", 30*time.Second, func() bool {
			out := h.hubCmd(t, "audit", "--node", "wb", "--json", "--limit", "500")
			return strings.Contains(out, "LinkTunnel/Dial") && strings.Contains(out, id) && strings.Contains(out, `"worker/wa"`)
		})
		// an admin reads none of alice's records, only their count
		r := h.longarm(h.admin, "admin", "audit", "--node", "hub", "--json", "--limit", "200")
		if r.code != 0 || strings.Contains(r.stdout, id) || !strings.Contains(r.stderr, "not shown") {
			t.Fatalf("admin's audit: %+v", r)
		}
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

	// Create the accounts first, with time to spare: the first useradd on a
	// fresh CI runner can take more than the CLI's call deadline.
	for _, w := range []*puWorker{a, b} {
		for _, cfg := range []string{alice, bob} {
			warmAccount(t, cfg, w)
		}
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
		out := h.hubCmd(t, "audit", "--node", "w2", "--json", "--limit", "500")
		return strings.Contains(out, id2) && strings.Contains(out, "denied")
	})
	h.mustLongarm(alice, "link", "rm", id, id2)
	h.longarm(alice, "kill", jobA)
	h.longarm(bob, "kill", jobB)
}

// warmAccount runs a command on per-user worker w as the user of cfg (which
// creates the user's account there) with a generous deadline.
func warmAccount(t *testing.T, cfg string, w *puWorker) {
	t.Helper()
	c, err := client.LoadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := client.NewDialer(c)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	name := filepath.Base(w.dataDir)
	conn, err := d.Dial(client.Worker{Name: name, CertName: name, Transport: "tcp", Address: fmt.Sprintf("127.0.0.1:%d", w.port)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r, err := conn.Exec().Run(ctx, &pb.ExecRequest{Argv: []string{w.helper, "whoami"}, Output: pb.OutputMode_OUTPUT_MODE_COMBINED,
		Timeout: durationpb.New(time.Minute)})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("first command as %s on %s: %v %v", cfg, name, r, err)
	}
}

// TestPortLinksWorkerPolicy: the workers' own [access] policies win over
// the hub's links. wb serves bob only: alice's link to wb is refused there
// (audited) while bob's identical link works, and alice's link listening
// on wb gets no listener ("refused by this worker's policy").
func TestPortLinksWorkerPolicy(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	alice := h.client
	os.Remove(filepath.Join(alice, "workers.toml")) // workers come from the hub
	h.mustLongarm(h.admin, "admin", "user", "add", "alice", "--no-code")
	h.mustLongarm(h.admin, "admin", "user", "add", "bob", "--no-code")
	bob := linkUser(t, h, "bob-laptop", "bob")
	wa := h.newWorkerDir(t, "wa", "")
	wb := h.newWorkerDir(t, "wb", "[access]\nusers = [\"bob\"]\n")
	for _, w := range []*hubWorker{wa, wb} {
		h.joinWorker(t, w, true)
		w.start(t, true)
	}
	for _, n := range []string{"wa", "wb"} {
		h.waitNode(t, n, "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	}

	// target side
	bport, _ := jobPort(t, h, bob, "wb", bins.helper, "http-serve", "600")
	al, bl := strconv.Itoa(freePort(t)), strconv.Itoa(freePort(t))
	aid := linkCreate(t, h, alice, "wb:"+bport, "--on", "wa", "--port", al)
	linkCreate(t, h, bob, "wb:"+bport, "--on", "wa", "--port", bl)
	waitFor(t, "bob's link to wb", 30*time.Second, func() bool {
		r := linkGet(h, bob, "wa", bins.helper, "127.0.0.1:"+bl, "/b")
		return r.code == 0 && strings.Contains(r.stdout, "status 200: hello /b from")
	})
	if r := linkGet(h, alice, "wa", bins.helper, "127.0.0.1:"+al, "/a"); strings.Contains(r.stdout, "status 200") {
		t.Fatalf("alice reached wb, which doesn't serve her: %+v", r)
	}
	waitFor(t, "wb's audit of alice's refused link", 30*time.Second, func() bool {
		out := h.hubCmd(t, "audit", "--node", "wb", "--json", "--limit", "500")
		return strings.Contains(out, aid) && strings.Contains(out, "not a user of this worker")
	})

	// source side
	aport, _ := jobPort(t, h, alice, "wa", bins.helper, "http-serve", "600")
	sid := linkCreate(t, h, alice, "wa:"+aport, "--on", "wb", "--port", strconv.Itoa(freePort(t)))
	waitFor(t, "wb refusing to listen for alice's link", 30*time.Second, func() bool {
		return strings.Contains(linkStatus(t, h, alice, sid), "refused by this worker's policy")
	})
}

// TestPortLinksTrust: two workers with end-to-end trust (alice's key pinned,
// both endorsed by her). A link created on alice's endorsed device carries
// her signed consent and works; a link for alice the hub accepts from a
// device she never endorsed (as an insider hub could make up) is refused by
// the workers.
func TestPortLinksTrust(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	alice := h.client
	os.Remove(filepath.Join(alice, "workers.toml")) // workers come from the hub
	ca, err := pki.LoadCA(filepath.Join(h.client, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	alicePub, aliceKey, _ := ed25519.GenerateKey(rand.Reader)
	ws := map[string]*hubWorker{"wa": h.newWorkerDir(t, "wa", ""), "wb": h.newWorkerDir(t, "wb", "")}
	for name, w := range ws {
		h.joinWorker(t, w, true)
		w.start(t, true)
		h.waitNode(t, name, "online", func(n map[string]any) bool { return isTrue(n["online"]) })
		if r := longarmdCmd(t, "trust", "add-user", pki.UserKeyLine{User: "alice", Key: alicePub}.String(), "--data-dir", w.dataDir); r.code != 0 {
			t.Fatalf("trust add-user: %+v", r)
		}
		r := longarmdCmd(t, "trust", "request", "--data-dir", w.dataDir)
		req, err := pki.ParseEndorseRequest(strings.SplitN(r.stdout, "\n", 2)[0])
		if err != nil {
			t.Fatalf("trust request: %+v %v", r, err)
		}
		we, err := pki.Endorse(aliceKey, "alice", pki.KindWorker, req.Name, req.NodeKey)
		if err != nil {
			t.Fatal(err)
		}
		if r := longarmdCmd(t, "trust", "accept", we.String(), "--data-dir", w.dataDir); r.code != 0 {
			t.Fatalf("trust accept: %+v", r)
		}
	}
	for name, w := range ws {
		waitFor(t, name+"'s trust policy reload", 20*time.Second, func() bool {
			b, _ := os.ReadFile(w.logPath)
			return strings.Contains(string(b), "trust policy reloaded")
		})
		waitFor(t, name+"'s renewed, endorsed certificate", 60*time.Second, func() bool {
			b, _ := os.ReadFile(filepath.Join(w.dataDir, "tls", "node.crt"))
			c, err := pki.ParseCertPEM(b)
			if err != nil {
				return false
			}
			tx, _ := pki.TrustOf(c)
			return tx != nil && tx.Endorsement != nil
		})
	}
	// alice's device: endorsed by her key (certificate and trust store, as
	// longarm trust init leaves them)
	st := trust.Store{Dir: filepath.Join(alice, "trust")}
	nk, err := st.NodeKey()
	if err != nil {
		t.Fatal(err)
	}
	e, err := pki.Endorse(aliceKey, "alice", pki.KindDevice, "alice", nk.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetEndorsement(e, pki.KindDevice, "alice"); err != nil {
		t.Fatal(err)
	}
	keyPEM, csrPEM, err := pki.NewKeyAndCSR("alice", &pki.CSRTrust{NodeKey: nk, Endorsement: e})
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignCSR(csrPEM, pki.IssueOptions{Role: pki.RoleClient, Name: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteBundleAtomic(filepath.Join(alice, "tls"), ca.CertPEM, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}

	port, _ := jobPort(t, h, alice, "wb", bins.helper, "http-serve", "600")
	signed := strconv.Itoa(freePort(t))
	sid := linkCreate(t, h, alice, "wb:"+port, "--on", "wa", "--port", signed)
	waitFor(t, "alice's signed link", 30*time.Second, func() bool {
		r := linkGet(h, alice, "wa", bins.helper, "127.0.0.1:"+signed, "/s")
		return r.code == 0 && strings.Contains(r.stdout, "status 200: hello /s from")
	})
	waitFor(t, "the signed link listening on the hub's list", 20*time.Second, func() bool {
		return strings.Contains(linkStatus(t, h, alice, sid), "listening")
	})

	// a device of "alice" the hub enrolled on its own (not endorsed): the
	// hub accepts its link, the workers don't
	other := linkUser(t, h, "alice-2", "alice")
	fid := linkCreate(t, h, other, "wb:"+port, "--on", "wa", "--port", strconv.Itoa(freePort(t)))
	waitFor(t, "wa refusing the unsigned link", 30*time.Second, func() bool {
		return strings.Contains(linkStatus(t, h, alice, fid), "no owner's consent")
	})
}
