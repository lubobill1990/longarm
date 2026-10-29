package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/tenancy"
)

// TestOwnedWorkers: alice registers a worker of her own (longarm worker
// add -> longarmd join): it joins approved and serves alice only - bob does
// not see it, cannot use it directly or by name, and placement never puts
// his jobs there - until an admin lets alice share and she shares it with
// bob; unsharing locks him out again.
func TestOwnedWorkers(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	alice, root := h.client, h.admin
	h.mustLongarm(root, "admin", "user", "add", "alice", "--no-code")
	h.mustLongarm(root, "admin", "user", "add", "bob", "--no-code")
	bob := filepath.Join(h.root, "cfg-bob")
	h.mustLongarm(alice, "admin", "issue", "--role", "client", "--name", "bob-laptop", "--user", "bob",
		"--out", filepath.Join(bob, "tls"), "--ca-dir", filepath.Join(alice, "ca"))
	os.WriteFile(filepath.Join(bob, "workers.toml"), nil, 0o644)
	os.WriteFile(filepath.Join(bob, "hub.toml"), []byte(fmt.Sprintf("address = %q\n", h.addr())), 0o600)

	// a fleet worker (registered by the admin) for everybody
	wf := h.newWorkerDir(t, "wf", "")
	h.joinWorker(t, wf, true)
	wf.start(t, true)

	// alice's own worker
	r := h.longarm(alice, "worker", "add", "aw", "--label", "kind=mine")
	code := strings.TrimSpace(r.stdout)
	if r.code != 0 || code == "" || !strings.Contains(r.stderr, "longarmd join "+h.addr()+" "+code+" --ca-fingerprint "+h.fp+" --name aw") {
		t.Fatalf("worker add: %+v", r)
	}
	aw := h.newWorkerDir(t, "aw", "")
	j := longarmdCmd(t, "join", h.addr(), code, "--data-dir", aw.dataDir, "--ca-fingerprint", h.fp)
	if j.code != 0 || !strings.Contains(j.stdout, "joined as worker aw") || !strings.Contains(j.stdout, "owned by alice") {
		t.Fatalf("longarmd join: %+v", j)
	}
	// The owner holds from the enrollment on: with the hub down, and even
	// without worker-access.json (hub.toml remembers the owned code), the
	// worker serves alice only - never everybody.
	if b, err := os.ReadFile(filepath.Join(aw.dataDir, tenancy.WorkerAccessFile)); err != nil || !strings.Contains(string(b), `"alice"`) {
		t.Fatalf("worker access after join: %s %v", b, err)
	}
	os.Remove(filepath.Join(aw.dataDir, tenancy.WorkerAccessFile))
	h.killHub()
	aw.start(t, true)
	direct := fmt.Sprintf("aw@127.0.0.1:%d", aw.port)
	print1 := []string{"--", bins.helper, "print", "1", "0", "0"}
	exec := func(cfg, worker string) result {
		return h.longarm(cfg, append([]string{"exec", worker}, print1...)...)
	}
	if r := exec(bob, direct); r.code == 0 || !strings.Contains(r.stderr, "worker owner/shares") {
		t.Fatalf("hub down, no access file: bob exec: %+v", r)
	}
	if r := exec(alice, direct); r.code != 0 || !strings.Contains(r.stdout, "line 1") {
		t.Fatalf("hub down, no access file: alice exec: %+v", r)
	}
	h.startHub(t)
	os.Remove(filepath.Join(bob, "hub-state.json")) // bob's longarm remembered the hub as down
	for _, w := range []string{"wf", "aw"} {
		h.waitNode(t, w, "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	}
	if n := h.waitNode(t, "aw", "owned", func(n map[string]any) bool { return n["owner"] == "alice" }); n["state"] != "approved" {
		t.Fatalf("aw: %v", n)
	}
	accessFile := filepath.Join(aw.dataDir, tenancy.WorkerAccessFile)
	waitAccess := func(what string, ok func(s string) bool) {
		t.Helper()
		waitFor(t, "worker access: "+what, 30*time.Second, func() bool {
			b, err := os.ReadFile(accessFile)
			return err == nil && ok(string(b))
		})
	}
	waitAccess("owner alice", func(s string) bool { return strings.Contains(s, `"alice"`) })

	if r := exec(alice, "aw"); r.code != 0 || !strings.Contains(r.stdout, "line 1") {
		t.Fatalf("alice exec on her worker: %+v", r)
	}
	id := strings.TrimSpace(h.mustLongarm(alice, append([]string{"run", "aw"}, print1...)...))
	h.mustLongarm(alice, "wait", "aw", id)

	// The hub session is no way around it: an admin can't read alice's job
	// through the hub - to the admin it doesn't even exist - alice can, in the panel.
	if r := h.longarm(root, "admin", "logs", id); r.code == 0 || !strings.Contains(r.stderr, "NotFound") {
		t.Fatalf("admin logs of alice's job through the hub: %+v", r)
	}
	if r := h.longarm(root, "admin", "run", "aw", "--", bins.helper, "print", "1", "0", "0"); r.code == 0 || !strings.Contains(r.stderr, "PermissionDenied") {
		t.Fatalf("admin submits to alice's worker through the hub: %+v", r)
	}
	pr := h.longarm(root, "admin", "user", "set", "alice", "--panel")
	m := regexp.MustCompile(`password (\S+)\nTOTP secret (\S+)`).FindStringSubmatch(pr.stderr)
	if pr.code != 0 || m == nil {
		t.Fatalf("panel login for alice: %+v", pr)
	}
	p := (&uiStack{hubEnv: h, user: "alice", pass: m[1], secret: m[2]}).login(t)
	waitFor(t, "alice's job in the hub index", 20*time.Second, func() bool {
		code, _ := p.do("GET", "/api/jobs/"+id, "")
		return code == 200
	})
	if logs := logText(sseEvents(t, p.c, h.httpURL()+"/api/jobs/"+id+"/logs?worker=aw", 30*time.Second)); !strings.Contains(logs, "line 1") {
		t.Fatalf("alice's panel logs: %q", logs)
	}
	if out := h.mustLongarm(alice, "ls"); !strings.Contains(out, "OWNER") || !lineStarts(out, "aw ") {
		t.Fatalf("alice's ls:\n%s", out)
	}
	if out := h.mustLongarm(alice, "worker", "ls"); !strings.Contains(out, "mine") || !strings.Contains(out, "fleet") {
		t.Fatalf("alice's worker ls:\n%s", out)
	}

	// Labels: alice sets them on her worker (longarm worker label) and
	// placement honors them; bob can't, an admin can; the hub audits it.
	if out := h.mustLongarm(alice, "worker", "label", "aw", "pool=devbox", "kind-"); !strings.Contains(out, "pool=devbox") ||
		strings.Contains(out, "kind=mine") {
		t.Fatalf("alice labels aw: %s", out)
	}
	if r := h.longarm(alice, append([]string{"run", "--require", "pool=devbox", "--no-queue"}, print1...)...); r.code != 0 ||
		!strings.Contains(r.stderr, "placed on aw") {
		t.Fatalf("alice's run --require pool=devbox: %+v", r)
	}
	if r := h.longarm(bob, "worker", "label", "aw", "pool=bob"); r.code == 0 || !strings.Contains(r.stderr, "NotFound") {
		t.Fatalf("bob labels alice's worker: %+v", r)
	}
	if r := h.longarm(alice, "worker", "label", "wf", "pool=x"); r.code == 0 || !strings.Contains(r.stderr, "PermissionDenied") {
		t.Fatalf("alice labels the fleet worker: %+v", r)
	}
	// an admin may label it by name, but learns nothing about it from the answer
	if out := h.mustLongarm(root, "worker", "label", "aw", "zone=a"); strings.Contains(out, "zone=a") || !strings.Contains(out, "labels set") {
		t.Fatalf("admin labels aw: %s", out)
	}
	h.waitNode(t, "aw", "zone=a", func(n map[string]any) bool {
		return fmt.Sprint(n["labels"]) != "" && strings.Contains(fmt.Sprint(n["labels"]), "zone:a")
	})
	if out := h.mustLongarm(root, "admin", "audit", "--node", "hub", "--limit", "200"); !strings.Contains(out, "workers/label") {
		t.Fatalf("no workers/label in the hub's audit:\n%s", out)
	}
	bobLocked := func(when string) {
		t.Helper()
		if out := h.longarm(bob, "ls").stdout; lineStarts(out, "aw ") {
			t.Fatalf("%s: bob sees aw:\n%s", when, out)
		}
		if r := exec(bob, "aw"); r.code == 0 {
			t.Fatalf("%s: bob exec aw by name: %+v", when, r)
		}
		if r := exec(bob, direct); r.code == 0 || !strings.Contains(r.stderr, "worker owner/shares") {
			t.Fatalf("%s: bob exec aw directly: %+v", when, r)
		}
		for i := 0; i < 3; i++ { // placement: always the fleet worker
			r := h.longarm(bob, append([]string{"run"}, print1...)...)
			if r.code != 0 || !strings.Contains(r.stderr, "placed on wf") {
				t.Fatalf("%s: bob's run: %+v", when, r)
			}
		}
	}
	bobLocked("not shared")
	// an admin is not exempt either (owner-only means owner-only)
	if r := exec(root, direct); r.code == 0 || !strings.Contains(r.stderr, "worker owner/shares") {
		t.Fatalf("admin exec on alice's worker: %+v", r)
	}
	adminSeesUserView(t, h, root, bob, print1)

	// sharing needs an admin's permission
	if r := h.longarm(alice, "worker", "share", "aw", "--user", "bob"); r.code == 0 || !strings.Contains(r.stderr, "can-share") {
		t.Fatalf("share without permission: %+v", r)
	}
	h.mustLongarm(root, "admin", "user", "set", "alice", "--can-share")
	if out := h.mustLongarm(alice, "whoami"); !strings.Contains(out, "can share own workers: true") {
		t.Fatalf("whoami:\n%s", out)
	}
	h.mustLongarm(alice, "worker", "share", "aw", "--user", "bob")
	waitAccess("shared with bob", func(s string) bool { return strings.Contains(s, `"bob"`) })
	if r := exec(bob, "aw"); r.code != 0 || !strings.Contains(r.stdout, "line 1") {
		t.Fatalf("bob exec on the shared worker: %+v", r)
	}
	if out := h.mustLongarm(bob, "worker", "ls"); !strings.Contains(out, "shared") {
		t.Fatalf("bob's worker ls:\n%s", out)
	}
	// Agent credentials (--agent-auth): not from bob to alice's worker with
	// shared isolation (alice's jobs run as the same OS account there)
	// unless he insists; alice's own worker gets hers, and the hub never
	// sees a value.
	ah := newAgentHome(t)
	withTok := append(ah.env, "COPILOT_GITHUB_TOKEN="+e2eCopilotValue)
	_, fakeCopilot := fakeAgents(t)
	if r := longarmWith(t, bob, withTok, "exec", "aw", "--", fakeCopilot); r.code != 2 || !strings.Contains(r.stderr, "alice's worker with shared isolation") ||
		!strings.Contains(r.stderr, "--agent-auth-shared-ok") {
		t.Fatalf("bob forwards to alice's shared-isolation worker: %+v", r)
	}
	if r := longarmWith(t, bob, withTok, "exec", "aw", "--agent-auth-shared-ok", "--", fakeCopilot); r.code != 0 || !strings.Contains(r.stderr, "WARNING") ||
		!strings.Contains(r.stdout, shaLine("COPILOT_GITHUB_TOKEN", e2eCopilotValue)) {
		t.Fatalf("bob with --agent-auth-shared-ok: %+v", r)
	}
	ar := longarmWith(t, alice, withTok, "run", "aw", "--wait", "--", fakeCopilot)
	if ar.code != 0 || !strings.Contains(ar.stderr, "forwarding agent credentials to aw") {
		t.Fatalf("alice forwards to her worker: %+v", ar)
	}
	agentJob := strings.TrimSpace(ar.stdout)
	waitFor(t, "the agent job in the hub index", 30*time.Second, func() bool {
		return strings.Contains(h.longarm(alice, "jobs", "--hub-only", "--id", agentJob, "--json").stdout, agentJob)
	})
	noSecretOnDisk(t, h.hubDir, "", e2eCopilotValue)

	h.mustLongarm(alice, "worker", "unshare", "aw", "--user", "bob")
	waitAccess("unshared", func(s string) bool { return !strings.Contains(s, `"bob"`) })
	bobLocked("unshared")

	// Shared with a team: bob leaving the team ends what he has open there
	// (the tenancy policy push re-checks open connections), not just new calls.
	h.mustLongarm(root, "admin", "team", "add", "t1", "bob")
	h.mustLongarm(alice, "worker", "share", "aw", "--team", "t1")
	waitAccess("shared with t1", func(s string) bool { return strings.Contains(s, `"t1"`) })
	waitFor(t, "bob in t1 on the worker", 30*time.Second, func() bool {
		b, _ := os.ReadFile(filepath.Join(aw.dataDir, tenancy.PolicyFile))
		return strings.Contains(string(b), `"t1"`) && exec(bob, direct).code == 0
	})
	long := h.longarmBG(t, bob, "exec", direct, "--timeout", "5m", "--", bins.helper, "sleep", "120")
	time.Sleep(3 * time.Second) // running
	start := time.Now()
	h.mustLongarm(root, "admin", "team", "rm", "t1", "bob")
	if code := long.wait(t, 90*time.Second); code == 0 || time.Since(start) > 40*time.Second {
		t.Fatalf("bob's exec after leaving the team: exit %d after %s\n%s", code, time.Since(start), long.stderr.String())
	}
	if r := exec(bob, direct); r.code == 0 || !strings.Contains(r.stderr, "worker owner/shares") {
		t.Fatalf("bob after leaving the team: %+v", r)
	}

	waitFor(t, "hub audit of the sharing", 20*time.Second, func() bool {
		out := h.longarm(root, "admin", "audit", "--node", "hub", "--limit", "100", "--json").stdout
		return strings.Contains(out, "workers/share") && strings.Contains(out, "workers/code")
	})
	h.mustLongarm(alice, "worker", "remove", "aw")
	h.waitNode(t, "aw", "revoked", func(n map[string]any) bool { return n["state"] == "revoked" })
}

// lineStarts reports whether a line of out starts with prefix.
func lineStarts(out, prefix string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// adminSeesUserView: with multi-tenancy an admin certificate gets exactly a
// regular user's view - ls, ls -o wide, worker ls and jobs show bob's
// (another regular user's) set of workers, never alice's private aw;
// placement never puts the admin's jobs on aw (hub and client-side picks);
// the fleet-wide view is the anonymous summary.
func adminSeesUserView(t *testing.T, h *hubEnv, root, bob string, print1 []string) {
	t.Helper()
	names := func(cfg string) []string {
		var m map[string]json.RawMessage
		out := h.mustLongarm(cfg, "ls", "--json")
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("ls --json: %v\n%s", err, out)
		}
		var ns []string
		for n := range m {
			if n != "w1" { // the e2e config's workers.toml entry (root's, not bob's)
				ns = append(ns, n)
			}
		}
		slices.Sort(ns)
		return ns
	}
	if r, b := names(root), names(bob); !slices.Equal(r, b) || slices.Contains(r, "aw") {
		t.Fatalf("admin's ls --json %v, bob's %v", r, b)
	}
	for _, args := range [][]string{{"ls", "-o", "wide"}, {"worker", "ls"}, {"admin", "nodes", "--all"}} {
		if out := h.mustLongarm(root, args...); lineStarts(out, "aw ") || strings.Contains(out, "aw-") {
			t.Fatalf("admin's %v shows alice's worker:\n%s", args, out)
		}
	}
	if out := h.mustLongarm(root, "jobs", "--all", "--json"); strings.Contains(out, `"aw"`) {
		t.Fatalf("admin's jobs --all show jobs on alice's worker:\n%s", out)
	}
	// placement: hub-side (run) and client-side (exec auto)
	for i := 0; i < 3; i++ {
		if r := h.longarm(root, append([]string{"run", "--on", "auto", "--no-queue"}, print1...)...); r.code != 0 || !strings.Contains(r.stderr, "placed on wf") {
			t.Fatalf("admin's run --on auto: %+v", r)
		}
		if r := h.longarm(root, append([]string{"exec", "auto"}, print1...)...); r.code != 0 || !strings.Contains(r.stdout, "line 1") ||
			!strings.Contains(r.stderr, "running on wf") {
			t.Fatalf("admin's exec auto: %+v", r)
		}
	}
	waitFor(t, "the admin's jobs in the hub index", 20*time.Second, func() bool {
		out := h.longarm(root, "jobs", "--hub-only", "--json").stdout
		return strings.Contains(out, `"wf"`)
	})
	if out := h.mustLongarm(root, "jobs", "--hub-only", "--json"); strings.Contains(out, `"aw"`) {
		t.Fatalf("an admin's job landed on alice's worker:\n%s", out)
	}
	// the fleet: counts per user, no names
	out := h.mustLongarm(root, "admin", "summary", "--rows", "--json")
	var sum struct {
		Users []struct {
			User   string         `json:"user"`
			Counts map[string]any `json:"counts"`
		} `json:"users"`
	}
	if err := json.Unmarshal([]byte(out), &sum); err != nil {
		t.Fatalf("summary json: %v\n%s", err, out)
	}
	alice := false
	for _, u := range sum.Users {
		alice = alice || (u.User == "alice" && fmt.Sprint(u.Counts["workers"]) == "1")
	}
	if !alice || strings.Contains(out, `"aw"`) || strings.Contains(out, `"wf"`) || strings.Contains(out, "kind") {
		t.Fatalf("admin summary:\n%s", out)
	}
	if r := h.longarm(bob, "admin", "summary"); r.code == 0 || !strings.Contains(r.stderr, "PermissionDenied") {
		t.Fatalf("bob's admin summary: %+v", r)
	}
}
