package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nodeByID returns the node with ID id from longarm admin nodes --all --json.
func (h *hubEnv) nodeByID(t *testing.T, id string) map[string]any {
	t.Helper()
	for _, n := range h.nodes(t) {
		if n["id"] == id {
			return n
		}
	}
	return nil
}

// TestNodeIDs (docs/security.md "Node identity"): a worker is renamed and
// reached by its ID, an ID prefix, id:<prefix> and its new name (its old
// name no longer); a revoked worker's name is free at once for a new node
// with a new ID, which the old revocation doesn't touch; listings and JSON
// carry the IDs.
func TestNodeIDs(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	os.Remove(filepath.Join(h.client, "workers.toml")) // everything through the hub

	w := h.newWorkerDir(t, "idbox", "")
	h.joinWorker(t, w, true)
	w.start(t, true)
	n := h.waitNode(t, "idbox", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	id, _ := n["id"].(string)
	if len(id) != 36 {
		t.Fatalf("node id %q (%v)", id, n)
	}
	exec := func(target string) result {
		t.Helper()
		return h.longarm(h.client, "exec", target, "--", bins.helper, "print", "1", "0", "0")
	}
	if r := exec("idbox"); r.code != 0 || !strings.Contains(r.stdout, "line 1") {
		t.Fatalf("exec by name: %+v", r)
	}
	// IDs next to names
	if out := h.mustLongarm(h.client, "ls"); !strings.Contains(out, id[:8]) {
		t.Fatalf("ls without the short id:\n%s", out)
	}

	t.Run("rename_and_dispatch", func(t *testing.T) {
		out := h.mustLongarm(h.admin, "admin", "rename", id[:8], "idbox2")
		if !strings.Contains(out, "idbox2") || !strings.Contains(out, id) {
			t.Fatalf("rename: %s", out)
		}
		// the worker follows: it renews with the new name (no restart: no
		// longnet listener); the hub's record keeps the id
		waitFor(t, "the worker's certificate to carry the new name", 30*time.Second, func() bool {
			n := h.nodeByID(t, id)
			return n != nil && n["name"] == "idbox2" && (n["cert_name"] == nil || n["cert_name"] == "idbox2") && isTrue(n["online"])
		})
		for _, target := range []string{id, id[:8], "id:" + id[:6], "idbox2", "name:idbox2"} {
			if r := exec(target); r.code != 0 || !strings.Contains(r.stdout, "line 1") {
				t.Fatalf("exec %s: %+v", target, r)
			}
		}
		if r := exec("idbox"); r.code == 0 {
			t.Fatalf("the old name still reaches the worker: %+v", r)
		}
		// jobs carry the worker's id (a job run by an id prefix)
		if r := h.longarm(h.client, "run", id[:8], "-f", "--", bins.helper, "print", "1", "0", "0"); r.code != 0 || !strings.Contains(r.stdout, "line 1") {
			t.Fatalf("run by id prefix: %+v", r)
		}
		var js struct {
			Jobs []map[string]any `json:"jobs"`
		}
		if err := json.Unmarshal([]byte(h.mustLongarm(h.client, "jobs", id[:8], "--json")), &js); err != nil || len(js.Jobs) == 0 {
			t.Fatalf("jobs --json: %v %v", js, err)
		}
		for _, j := range js.Jobs {
			if j["workerId"] != id || j["worker"] != "idbox2" {
				t.Fatalf("job %v", j)
			}
		}
		// the audit has the rename
		if out := h.mustLongarm(h.admin, "admin", "audit", "--node", "hub"); !strings.Contains(out, "nodes/rename") {
			t.Fatalf("no rename in the audit:\n%s", out)
		}
	})

	t.Run("revoke_then_reuse_name", func(t *testing.T) {
		h.mustLongarm(h.admin, "admin", "revoke", "idbox2")
		waitFor(t, "revoked", 10*time.Second, func() bool {
			n := h.nodeByID(t, id)
			return n != nil && n["state"] == "revoked"
		})
		w.kill()
		// the name is free at once: a new node joins as idbox2
		w2 := h.newWorkerDir(t, "idbox2", "")
		h.joinWorker(t, w2, true)
		w2.start(t, true)
		var id2 string
		waitFor(t, "the new idbox2 online", 30*time.Second, func() bool {
			for _, n := range h.nodes(t) {
				if n["name"] == "idbox2" && n["state"] == "approved" && isTrue(n["online"]) {
					id2, _ = n["id"].(string)
					return true
				}
			}
			return false
		})
		if id2 == "" || id2 == id {
			t.Fatalf("new node id %q (old %s)", id2, id)
		}
		// the name reaches the new node; the old id is refused as revoked
		info := h.mustLongarm(h.client, "info", "idbox2")
		if !strings.Contains(info, filepath.Base(w2.dataDir)) {
			t.Fatalf("info idbox2 is not the new worker:\n%s", info)
		}
		if r := exec(id2[:8]); r.code != 0 {
			t.Fatalf("exec the new node by id: %+v", r)
		}
		if r := exec(id); r.code == 0 || !strings.Contains(r.stderr, "revoked") {
			t.Fatalf("exec the revoked node by id: %+v", r)
		}
	})
}
