package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"longarm/internal/hubclient"
	"longarm/internal/tenancy"
)

func TestClaimWorker(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	wa := h.newWorkerDir(t, "worker-1", "")
	wb := h.newWorkerDir(t, "worker-2", "")
	for _, w := range []*hubWorker{wa, wb} {
		h.joinWorker(t, w, true)
		w.start(t, true)
		h.waitNode(t, w.name, "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	}
	before := h.waitNode(t, wa.name, "ID", func(n map[string]any) bool { return n["id"] != nil })
	h.mustLongarm(h.admin, "admin", "user", "add", "alice", "--no-code")
	h.mustLongarm(h.admin, "admin", "user", "add", "bob", "--no-code")
	bob := linkUser(t, h, "bob-laptop", "bob")
	direct := fmt.Sprintf("%s@127.0.0.1:%d", wa.name, wa.port)
	port, _ := jobPort(t, h, bob, wb.name, bins.helper, "http-serve", "600")
	link := linkCreate(t, h, bob, wb.name+":"+port, "--on", wa.name, "--port", strconv.Itoa(freePort(t)))
	pidFile := filepath.Join(wa.dataDir, "claim-exec.pid")
	long := h.longarmBG(t, bob, "exec", direct, "--timeout", "5m", "--", bins.helper, "tree-leaf", pidFile)
	waitFor(t, "bob's connection running before claim", 20*time.Second, func() bool {
		_, err := os.Stat(pidFile)
		return err == nil
	})
	out := h.mustLongarm(h.client, "worker", "claim", wa.name, "--yes")
	if !strings.Contains(out, "owned by alice") || !strings.Contains(out, fmt.Sprint(before["id"])) {
		t.Fatalf("claim changed node ID: %s, before %v", out, before)
	}
	if code := long.wait(t, 40*time.Second); code == 0 {
		t.Fatal("bob's open connection survived the claim")
	}
	waitFor(t, "bob's port link removed", 20*time.Second, func() bool {
		return !strings.Contains(h.mustLongarm(bob, "link", "ls"), link)
	})
	waitFor(t, "owner persisted in hub.toml", 20*time.Second, func() bool {
		cfg, err := hubclient.LoadFile(wa.dataDir)
		return err == nil && cfg.Owner == "alice"
	})
	if r := h.longarm(bob, "exec", direct, "--", bins.helper, "print", "1", "0", "0"); r.code == 0 {
		t.Fatal("bob may still use the claimed worker")
	}
	wa.kill()
	h.killHub()
	if err := os.Remove(filepath.Join(wa.dataDir, tenancy.WorkerAccessFile)); err != nil {
		t.Fatal(err)
	}
	wa.start(t, true)
	if r := h.longarm(bob, "exec", direct, "--", bins.helper, "print", "1", "0", "0"); r.code == 0 || !strings.Contains(r.stderr, "worker owner/shares") {
		t.Fatalf("lost access file reopened worker: %+v", r)
	}
	if r := h.longarm(h.client, "exec", direct, "--", bins.helper, "print", "1", "0", "0"); r.code != 0 {
		t.Fatalf("owner cannot use worker offline: %+v", r)
	}
	h.startHub(t)
	wb.kill()
	h.mustLongarm(h.client, "worker", "claim", wb.name, "--yes")
	wb.start(t, true)
	waitFor(t, "offline worker learns its owner on reconnect", 20*time.Second, func() bool {
		cfg, err := hubclient.LoadFile(wb.dataDir)
		return err == nil && cfg.Owner == "alice"
	})
}

func TestAdoptHub(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	h.mustLongarm(h.client, "whoami")
	h.mustLongarm(h.admin, "whoami")
	w := h.newWorkerDir(t, "worker-1", "")
	h.joinWorker(t, w, true)
	w.start(t, true)
	h.waitNode(t, w.name, "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	out := h.hubCmd(t, "adopt", "--user", "alice", "--map", "root=bob", "--yes")
	if !strings.Contains(out, "2 devices pinned, 1 workers claimed") {
		t.Fatalf("adoption summary: %s", out)
	}
	waitFor(t, "running hub applies adoption", 20*time.Second, func() bool {
		cfg, err := hubclient.LoadFile(w.dataDir)
		return err == nil && cfg.Owner == "alice"
	})
	if out := h.mustLongarm(h.client, "worker", "ls"); !strings.Contains(out, "worker-1") || !strings.Contains(out, "alice") {
		t.Fatalf("adopted worker: %s", out)
	}
	var me struct {
		User string `json:"user"`
	}
	out = h.mustLongarm(h.admin, "whoami", "--json")
	if err := json.Unmarshal([]byte(out), &me); err != nil || me.User != "bob" {
		t.Fatalf("pinned record did not beat old certificate: %s (%v)", out, err)
	}
	if out := h.hubCmd(t, "audit", "--node", "hub", "--json"); !strings.Contains(out, "workers/adopt") {
		t.Fatalf("adoption not audited: %s", out)
	}
}

func TestIssuedWorkerOwnership(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	h.mustLongarm(h.admin, "admin", "user", "add", "root", "--no-code")
	h.mustLongarm(h.admin, "admin", "user", "add", "alice", "--no-code")
	if r := h.longarm(h.admin, "admin", "join-code", "--role", "worker", "--owner", "alice"); r.code == 0 || !strings.Contains(r.stderr, "PermissionDenied") {
		t.Fatalf("assigning another user's worker code: %+v", r)
	}
	outDir := filepath.Join(h.root, "worker-tls")
	if r := h.longarm(h.admin, "admin", "issue", "--role", "worker", "--name", "worker-1", "--owner", "alice", "--out", outDir,
		"--ca-dir", filepath.Join(h.client, "ca")); r.code == 0 || !strings.Contains(r.stderr, "PermissionDenied") {
		t.Fatalf("assigning another user's worker bundle: %+v", r)
	}
	h.mustLongarm(h.admin, "admin", "issue", "--role", "worker", "--name", "worker-1", "--out", outDir, "--ca-dir", filepath.Join(h.client, "ca"))
	h.waitNode(t, "worker-1", "owned before its first connect", func(n map[string]any) bool { return n["owner"] == "root" })
}
