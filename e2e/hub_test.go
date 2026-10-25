package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nodes returns the hub's nodes (longarm admin nodes --all --json) as maps.
func (h *hubEnv) nodes(t *testing.T) []map[string]any {
	t.Helper()
	r := h.longarm(h.admin, "admin", "nodes", "--all", "--json")
	if r.code != 0 {
		return nil
	}
	var resp struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &resp); err != nil {
		t.Fatalf("nodes json: %v\n%s", err, r.stdout)
	}
	return resp.Nodes
}

func isTrue(v any) bool { b, _ := v.(bool); return b }

// TestHub: enrollment (join code -> pending -> approve), a worker with a
// hand-issued certificate registering itself, discovery through the hub
// (no workers.toml entries), longarm ls / jobs via the hub, offline detection,
// longarm wait surviving a hub crash, and the job index rebuilt from workers.
func TestHub(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	// The client has no workers.toml entries at all: everything is discovered.
	os.Remove(filepath.Join(h.client, "workers.toml"))

	wa := h.newWorkerDir(t, "wa", "")
	t.Run("enroll_pending_approve", func(t *testing.T) {
		h.joinWorker(t, wa, false)
		n := h.waitNode(t, "wa", "pending", func(n map[string]any) bool { return n["state"] == "pending" })
		if n["role"] != "worker" {
			t.Fatalf("node %v", n)
		}
		// Not approved yet: longarmd waits instead of serving.
		wa.start(t, false)
		time.Sleep(1500 * time.Millisecond)
		if _, err := os.Stat(filepath.Join(wa.dataDir, "longarmd.endpoints")); err == nil {
			t.Fatal("longarmd serves before approval")
		}
		h.mustLongarm(h.admin, "admin", "approve", "wa")
		waitFor(t, "longarmd to finish enrolling and listen", 30*time.Second, func() bool {
			_, err := os.Stat(filepath.Join(wa.dataDir, "longarmd.endpoints"))
			return err == nil
		})
		h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) && n["state"] == "approved" })
		hubToml, _ := os.ReadFile(filepath.Join(wa.dataDir, "hub.toml"))
		if strings.Contains(string(hubToml), "poll_token") {
			t.Fatalf("poll token kept after enrollment: %s", hubToml)
		}
	})

	// w1 holds a certificate issued by hand (longarm admin issue) before the hub
	// existed: it registers by certificate.
	w1 := &hubWorker{name: "w1", dataDir: h.dataDir, port: h.env.port, logPath: filepath.Join(h.root, "longarmd-w1.log")}
	h.hmu.Lock()
	h.workers = append(h.workers, w1)
	h.hmu.Unlock()
	os.WriteFile(filepath.Join(h.dataDir, "longarmd.toml"), []byte(fmt.Sprintf("listen = [\"tcp://127.0.0.1:%d\"]\n[hub]\naddress = %q\nlabels = { region = \"test\" }\n", h.env.port, h.addr())), 0o600)
	w1.start(t, true)

	t.Run("discovery_ls_exec", func(t *testing.T) {
		n := h.waitNode(t, "w1", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
		if labels, _ := n["labels"].(map[string]any); labels["region"] != "test" || labels["os"] == nil {
			t.Fatalf("labels %v", n["labels"])
		}
		if n2 := h.waitNode(t, "wa", "labels", func(map[string]any) bool { return true }); n2["labels"].(map[string]any)["pool"] != "e2e" {
			t.Fatalf("join code labels missing: %v", n2["labels"])
		}
		waitFor(t, "metrics in longarm ls", 20*time.Second, func() bool {
			out := h.longarm(h.client, "ls").stdout
			return strings.Contains(out, "wa ") && strings.Contains(out, "w1 ") && strings.Count(out, "online") == 2 && !strings.Contains(out, "online  -")
		})
		r := h.longarm(h.client, "exec", "wa", "--", bins.helper, "print", "2", "0", "4")
		if r.code != 4 || !strings.Contains(r.stdout, "line 2") {
			t.Fatalf("exec on a discovered worker: %+v", r)
		}
		// Automatic choice for exec.
		r = h.longarm(h.client, "exec", "--", bins.helper, "print", "1", "0", "0")
		if r.code != 0 || !strings.Contains(r.stderr, "longarm: running on ") {
			t.Fatalf("exec --on auto: %+v", r)
		}
		// The cache lets longarm find workers when the hub is gone.
		if _, err := os.Stat(filepath.Join(h.client, "hub-cache.json")); err != nil {
			t.Fatal("no hub cache")
		}
	})

	t.Run("jobs_wait_hub_crash_rebuild", func(t *testing.T) {
		id := strings.TrimSpace(h.mustLongarm(h.client, "run", "wa", "--", bins.helper, "print", "30", "400", "3"))
		// The index first learns the job as preparing; under load the
		// running state follows a heartbeat later.
		var out string
		waitFor(t, "job running in the hub index", 20*time.Second, func() bool {
			out = h.longarm(h.client, "jobs", "--state", "running").stdout
			return strings.Contains(out, id)
		})
		if !strings.Contains(out, "wa") {
			t.Fatalf("longarm jobs via hub:\n%s", out)
		}
		// longarm wait <id> (no worker): through the hub; the hub dies meanwhile.
		bgw := h.longarmBG(t, h.client, "wait", id)
		time.Sleep(1500 * time.Millisecond)
		h.killHub()
		if code := bgw.wait(t, 60*time.Second); code != 3 {
			t.Fatalf("wait exit %d, stderr: %s", code, bgw.stderr.String())
		}
		if !strings.Contains(bgw.stderr.String(), "unreachable") {
			t.Fatalf("no fallback note: %s", bgw.stderr.String())
		}
		// Without the hub: ls/jobs/wait still work from the cache.
		if r := h.longarm(h.client, "wait", id); r.code != 3 {
			t.Fatalf("wait without hub: %+v", r)
		}
		if r := h.longarm(h.client, "jobs"); r.code != 0 || !strings.Contains(r.stdout, id) {
			t.Fatalf("jobs without hub: %+v", r)
		}
		if r := h.longarm(h.client, "ls"); r.code != 0 || !strings.Contains(r.stdout, "wa ") {
			t.Fatalf("ls without hub: %+v", r)
		}
		// The job ended while the hub was down; a hub started with an empty
		// job cache rebuilds it from the workers' reports.
		h.startHub(t, "--rebuild-jobs")
		waitFor(t, "rebuilt job index", 30*time.Second, func() bool {
			r := h.longarm(h.client, "jobs", "--json")
			return r.code == 0 && strings.Contains(r.stdout, id) && strings.Contains(r.stdout, "JOB_STATE_FAILED")
		})
		if r := h.longarm(h.client, "wait", id); r.code != 3 || strings.Contains(r.stderr, "unreachable") {
			t.Fatalf("wait via the restarted hub: %+v", r)
		}
	})

	t.Run("offline_detection", func(t *testing.T) {
		h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
		wa.kill()
		h.waitNode(t, "wa", "offline", func(n map[string]any) bool { return !isTrue(n["online"]) })
		if out := h.longarm(h.client, "ls").stdout; !strings.Contains(out, "offline") {
			t.Fatalf("ls: %s", out)
		}
		wa.start(t, true)
		h.waitNode(t, "wa", "online again", func(n map[string]any) bool { return isTrue(n["online"]) })
	})
}
