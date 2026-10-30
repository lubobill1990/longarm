package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAgentHandoffHub: handoff between the workers of one user of a hub,
// with relayed paths only (direct paths off, the workers reachable only
// through an external relay - longarm-relay --hub):
//   - local -> aw1, then aw1 -> aw2 directly between the workers, which
//     only works through the external relay: the hub lists the pair in the
//     relay policy for the transfer (Fleet.HandoffPair) and drops it after;
//   - another user's worker is refused - for a user it is shared with and
//     for an admin certificate (of a user it is shared with, too) alike -
//     before anything moves.
func TestAgentHandoffHub(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	h := newHubEnv(t, "")
	alice, root := h.client, h.admin
	os.Remove(filepath.Join(alice, "workers.toml"))
	x := startExtRelay(t, h, "xrelay")
	waitFor(t, "the relay registered with the hub", 30*time.Second, func() bool {
		hs, _ := x.health()["hub"].(map[string]any)
		return isTrue(hs["connected"]) && isTrue(hs["ready"])
	})
	for _, u := range []string{"alice", "bob", "carol"} {
		h.mustLongarm(root, "admin", "user", "add", u, "--no-code")
	}
	h.mustLongarm(root, "admin", "user", "set", "alice", "--can-share")
	// bob's client certificate; carol's admin certificate (an admin of the
	// hub, acting as carol)
	bob, carol := filepath.Join(h.root, "cfg-bob"), filepath.Join(h.root, "cfg-carol")
	h.mustLongarm(alice, "admin", "issue", "--role", "client", "--name", "bob-laptop", "--user", "bob",
		"--out", filepath.Join(bob, "tls"), "--ca-dir", filepath.Join(alice, "ca"))
	h.mustLongarm(alice, "admin", "issue", "--role", "admin", "--name", "carol-admin", "--user", "carol",
		"--out", filepath.Join(carol, "tls"), "--ca-dir", filepath.Join(alice, "ca"))
	for _, d := range []string{bob, carol} {
		os.WriteFile(filepath.Join(d, "hub.toml"), []byte(fmt.Sprintf("address = %q\n", h.addr())), 0o600)
	}

	// the fake agents; every machine its own home
	fakebin := filepath.Join(h.root, "fakebin")
	os.MkdirAll(fakebin, 0o755)
	copyExe(t, bins.helper, filepath.Join(fakebin, exeName("claude")))
	copyExe(t, bins.helper, filepath.Join(fakebin, exeName("copilot")))
	path := fakebin + string(os.PathListSeparator) + os.Getenv("PATH")
	homes := map[string]string{}
	for _, n := range []string{"aw1", "aw2", "alice", "bob", "carol"} {
		homes[n] = filepath.Join(h.root, "home-"+n)
		os.MkdirAll(homes[n], 0o755)
	}
	netEnv := []string{"LONGARM_NET_DISABLE_DIRECT=1"}

	// alice's two workers (longarm worker add -> longarmd join), loopback
	// only: other machines reach them through the relays
	for _, name := range []string{"aw1", "aw2"} {
		r := h.longarm(alice, "worker", "add", name)
		code := strings.TrimSpace(r.stdout)
		if r.code != 0 || code == "" {
			t.Fatalf("worker add %s: %+v", name, r)
		}
		w := h.newWorkerDir(t, name, "")
		w.env = append(homeEnv(homes[name], path), netEnv...)
		if j := longarmdCmd(t, "join", h.addr(), code, "--data-dir", w.dataDir, "--ca-fingerprint", h.fp); j.code != 0 || !strings.Contains(j.stdout, "owned by alice") {
			t.Fatalf("longarmd join %s: %+v", name, j)
		}
		w.start(t, true)
	}
	for _, name := range []string{"aw1", "aw2"} {
		extEP := fmt.Sprintf("longnet://%s?name=%s", x.udp, name)
		h.waitNode(t, name, "online on the external relay, owned by alice", func(n map[string]any) bool {
			return isTrue(n["online"]) && n["owner"] == "alice" && strings.Contains(fmt.Sprint(n["endpoints"]), extEP)
		})
	}
	// every client reaches the workers over the external relay only (and
	// passes that endpoint on: the target dials the source there)
	workers := ""
	for _, name := range []string{"aw1", "aw2"} {
		workers += fmt.Sprintf("[[worker]]\nname = %q\ncert_name = %q\ntransport = \"longnet\"\naddress = %q\n", name, name, x.udp)
	}
	for _, d := range []string{alice, bob, carol} {
		if err := os.WriteFile(filepath.Join(d, "workers.toml"), []byte(workers), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// run runs longarm in dir as a client (config cfg) with its own home
	run := func(cfg, home, dir string, args ...string) result {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bins.longarm, args...)
		cmd.Dir = dir
		cmd.Env = append(append(append(os.Environ(), homeEnv(home, path)...), netEnv...),
			"LONGARM_CONFIG_DIR="+cfg, "LONGARM_WORKER=", "LONGARM_SESSION=")
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("longarm %v: %v", args, err)
		}
		return result{out.String(), errb.String(), code}
	}

	// alice's checkout and a Claude Code session of it
	repo := filepath.Join(h.root, "src", "app")
	os.MkdirAll(repo, 0o755)
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "core.autocrlf", "false")
	mustWrite(t, filepath.Join(repo, "tracked.txt"), "v1\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "c1")
	mustWrite(t, filepath.Join(repo, "tracked.txt"), "v2 uncommitted\n")
	session := func(home, id string) {
		mustWrite(t, filepath.Join(home, ".claude", "projects", claudeDir(repo), id+".jsonl"),
			fmt.Sprintf(`{"type":"user","cwd":%s,"sessionId":%q,"version":"1.0.0","message":{"role":"user","content":"go"}}`+"\n", jsonQ(repo), id))
	}
	id := "8e55a7c1-0000-4000-8000-00000000c1a0"
	session(homes["alice"], id)
	decode := func(r result) handoffOut {
		t.Helper()
		var out handoffOut
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil {
			t.Fatalf("handoff: %+v", r)
		}
		return out
	}

	t.Run("local_to_aw1", func(t *testing.T) {
		w1 := decode(run(alice, homes["alice"], repo, "handoff", "--json", "--to", "aw1", "--agent", "claude", "--session", id))
		if want := filepath.Join(homes["aw1"], "longarm-handoff", "app-8e55a7c1"); w1.Worktree != want {
			t.Fatalf("handoff: %+v (want worktree %s)", w1, want)
		}
		if got := readText(t, filepath.Join(w1.Worktree, "tracked.txt")); got != "v2 uncommitted\n" {
			t.Errorf("tracked.txt on aw1: %q", got)
		}
	})

	t.Run("aw1_to_aw2_through_the_external_relay", func(t *testing.T) {
		pairs := func() float64 {
			hs, _ := x.health()["hub"].(map[string]any)
			n, _ := hs["worker_pairs"].(float64)
			return n
		}
		if n := pairs(); n != 0 {
			t.Fatalf("worker pairs on the relay before the handoff: %v", n)
		}
		// watch the relay's policy while the handoff runs
		var mu sync.Mutex
		maxPairs := 0.0
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n := pairs()
				mu.Lock()
				maxPairs = max(maxPairs, n)
				mu.Unlock()
				select {
				case <-stop:
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}()
		r := run(alice, homes["alice"], repo, "handoff", "--json", "--from", "aw1", "--to", "aw2", "--session", id, "--idle", "1s", "--grace", "5s")
		close(stop)
		wg.Wait()
		out := decode(r)
		// no direct path between the workers, and the only endpoint of aw1
		// the client passes on is the external relay's: "direct" with
		// nothing relayed by the client went through the external relay
		if out.Transfer != "direct" || out.Relayed != 0 || strings.Contains(r.stderr, "no direct transfer") {
			t.Fatalf("transfer %q, %d bytes relayed by the client\n%s", out.Transfer, out.Relayed, r.stderr)
		}
		if got := readText(t, filepath.Join(out.Worktree, "tracked.txt")); got != "v2 uncommitted\n" {
			t.Errorf("tracked.txt on aw2: %q", got)
		}
		mu.Lock()
		seen := maxPairs
		mu.Unlock()
		if seen < 1 {
			t.Errorf("the relay never had the pair aw1 <-> aw2 in its policy")
		}
		waitFor(t, "the pair released on the relay", 30*time.Second, func() bool { return pairs() == 0 })
	})

	t.Run("another_users_worker", func(t *testing.T) {
		// aw1 is shared with bob and with carol (whose admin certificate
		// this is): they may run jobs there, never move sessions in or out
		h.mustLongarm(alice, "worker", "share", "aw1", "--user", "bob")
		h.mustLongarm(alice, "worker", "share", "aw1", "--user", "carol")
		bid := "9e55a7c1-0000-4000-8000-00000000c1a0"
		for _, who := range []struct{ name, cfg string }{{"bob", bob}, {"carol", carol}} {
			home := homes[who.name]
			waitFor(t, "aw1 shared with "+who.name, 30*time.Second, func() bool {
				return run(who.cfg, home, repo, "exec", "aw1", "--", bins.helper, "print", "1", "0", "0").code == 0
			})
			session(home, bid)
			r := run(who.cfg, home, repo, "handoff", "--to", "aw1", "--agent", "claude", "--session", bid, "--no-attach")
			if r.code != 77 || !strings.Contains(r.stderr, "aw1 is another user's worker") {
				t.Errorf("%s hands off to alice's worker: %+v", who.name, r)
			}
			if m, _ := filepath.Glob(filepath.Join(homes["aw1"], ".claude", "projects", "*", bid+"*")); len(m) != 0 {
				t.Errorf("%s's session arrived on aw1: %v", who.name, m)
			}
			if _, err := os.Stat(filepath.Join(home, ".claude", "projects", claudeDir(repo), bid+".handoff.json")); err == nil {
				t.Errorf("%s's refused session was marked handed off", who.name)
			}
			// from it: nothing there is theirs to take
			r = run(who.cfg, home, repo, "handoff", "--from", "aw1", "--to", "local", "--agent", "claude", "--session", id)
			if r.code != 77 || !strings.Contains(r.stderr, "aw1 is another user's worker") {
				t.Errorf("%s hands off from alice's worker: %+v", who.name, r)
			}
		}
	})
}
