package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDynamicMemory: a worker that is a Hyper-V Dynamic Memory guest
// (simulated: LONGARM_TEST_MEM_MAX_GB, honoured by the "testhooks" longarmd
// TestMain builds) reports its ceiling - in longarm ls
// (table and --json) - and the hub places a job by it that fits in no
// machine's physical memory.
func TestDynamicMemory(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	os.Remove(filepath.Join(h.client, "workers.toml"))
	ws, wd := h.newWorkerDir(t, "ws", ""), h.newWorkerDir(t, "wd", "")
	wd.env = []string{"LONGARM_TEST_MEM_MAX_GB=65536"} // 64 TB: above any test machine
	for _, w := range []*hubWorker{ws, wd} {
		h.joinWorker(t, w, true)
		w.start(t, true)
	}
	h.waitNode(t, "ws", "online", func(n map[string]any) bool { return isTrue(n["online"]) && n["metrics"] != nil })
	h.waitNode(t, "wd", "ceiling", func(n map[string]any) bool {
		m, _ := n["metrics"].(map[string]any)
		return isTrue(n["online"]) && m != nil && fmt.Sprint(m["mem_max_bytes"]) == fmt.Sprint(uint64(65536)<<30)
	})

	var js map[string]struct {
		Metrics map[string]any `json:"metrics"`
	}
	if out := h.mustLongarm(h.client, "ls", "--json"); json.Unmarshal([]byte(out), &js) != nil ||
		fmt.Sprint(js["wd"].Metrics["memMaxBytes"]) != fmt.Sprint(uint64(65536)<<30) {
		t.Fatalf("ls --json:\n%s", out)
	}
	if out := h.mustLongarm(h.client, "ls"); !lineHas(out, "wd ", "(max 65536G)") || lineHas(out, "ws ", "(max 65536G)") {
		t.Fatalf("ls:\n%s", out)
	}

	r := h.longarm(h.client, "run", "--mem-gb", "30000", "--no-queue", "--wait", "--", bins.helper, "print", "1", "0", "0")
	if r.code != 0 || placedOn(t, r.stderr) != "wd" {
		t.Fatalf("30 TB job: %+v", r)
	}
}

// lineHas reports whether a line of out starts with prefix and contains s.
func lineHas(out, prefix, s string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) && strings.Contains(l, s) {
			return true
		}
	}
	return false
}
