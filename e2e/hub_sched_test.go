package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var placedRE = regexp.MustCompile(`placed on (\S+)`)

// freeGB is a worker's free memory as the hub sees it.
func (h *hubEnv) freeGB(t *testing.T, name string) float64 {
	t.Helper()
	n := h.waitNode(t, name, "metrics", func(n map[string]any) bool { return n["metrics"] != nil })
	m := n["metrics"].(map[string]any)
	total, _ := strconv.ParseFloat(fmt.Sprint(m["mem_total_bytes"]), 64)
	used, _ := strconv.ParseFloat(fmt.Sprint(m["mem_used_bytes"]), 64)
	return (total - used) / (1 << 30)
}

func placedOn(t *testing.T, stderr string) string {
	t.Helper()
	m := placedRE.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("not placed by the hub: %s", stderr)
	}
	return m[1]
}

// TestHubScheduler: longarm run without a worker is placed by the hub; memory
// reservations steer the next job elsewhere; when nobody passes the gate
// the job queues and starts once a reservation is released; drained
// workers (admin or longarmd drain) get nothing; label constraints; and the
// client-side fallback when the hub is down.
func TestHubScheduler(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	os.Remove(filepath.Join(h.client, "workers.toml"))
	wa, wb := h.newWorkerDir(t, "wa", ""), h.newWorkerDir(t, "wb", "")
	for _, w := range []*hubWorker{wa, wb} {
		h.joinWorker(t, w, true)
		w.start(t, true)
	}
	for _, n := range []string{"wa", "wb"} {
		h.waitNode(t, n, "online", func(n map[string]any) bool { return isTrue(n["online"]) && n["metrics"] != nil })
	}
	// Both workers share this machine: the same free memory F. A job
	// reserving 0.7 F fits on an idle worker but not next to another one,
	// as long as the free memory stays within [0.7 F, 1.4 F) - it moves on
	// a busy machine, and a Hyper-V VM with dynamic memory grows it.
	big := fmt.Sprintf("%.1f", 0.7*min(h.freeGB(t, "wa"), h.freeGB(t, "wb")))
	// The small jobs reserve little explicitly: the default (2 GB) may not
	// fit next to a big one on a CI VM with a few GB free.
	const small = "0.1"

	run := func(args ...string) result {
		t.Helper()
		return h.longarm(h.client, append([]string{"run"}, args...)...)
	}
	var idA, idB, onA, onB string
	t.Run("reservation_spreads", func(t *testing.T) {
		r := run("--mem-gb", big, "--name", "A", "--", bins.helper, "sleep", "120")
		if r.code != 0 {
			t.Fatalf("%+v", r)
		}
		idA, onA = strings.TrimSpace(r.stdout), placedOn(t, r.stderr)
		r = run("--mem-gb", big, "--name", "B", "--", bins.helper, "sleep", "120")
		if r.code != 0 {
			t.Fatalf("%+v", r)
		}
		idB, onB = strings.TrimSpace(r.stdout), placedOn(t, r.stderr)
		if onA == onB {
			t.Fatalf("both big jobs on %s: the reservation did not count", onA)
		}
		n := h.waitNode(t, onA, "reservation", func(n map[string]any) bool { return n["reserved_gb"] != nil })
		if fmt.Sprint(n["jobs_active"]) != "1" {
			t.Fatalf("node %v", n)
		}
	})

	t.Run("queue_until_released", func(t *testing.T) {
		bg := h.longarmBG(t, h.client, "run", "--mem-gb", big, "--name", "C", "--wait", "--", bins.helper, "print", "1", "0", "0")
		waitOutput(t, bg.stderr.String, "queued at position 1", 20*time.Second)
		select {
		case <-bg.done:
			t.Fatalf("queued job did not wait: %s", bg.stderr.String())
		case <-time.After(time.Second):
		}
		h.mustLongarm(h.client, "kill", idA) // releases A's reservation
		if code := bg.wait(t, 60*time.Second); code != 0 {
			t.Fatalf("queued job exit %d: %s", code, bg.stderr.String())
		}
		if got := placedOn(t, bg.stderr.String()); got != onA {
			t.Fatalf("queued job went to %s, want the freed %s", got, onA)
		}
	})

	t.Run("drain_and_labels", func(t *testing.T) {
		// B's reservation is done with (the queue test): release it, or a
		// small job may not fit next to it once this shared machine's free
		// memory shrinks. Drain onA: small jobs go to onB.
		h.mustLongarm(h.client, "kill", idB)
		h.mustLongarm(h.admin, "admin", "drain", onA)
		r := run("--mem-gb", small, "--no-queue", "--wait", "--", bins.helper, "print", "1", "0", "0")
		if r.code != 0 || placedOn(t, r.stderr) != onB {
			t.Fatalf("drained %s still got work: %+v", onA, r)
		}
		h.mustLongarm(h.admin, "admin", "undrain", onA)
		// longarmd drain (local control API) is seen by the hub too.
		dir := map[string]string{"wa": wa.dataDir, "wb": wb.dataDir}[onB]
		if r := longarmdCmd(t, "drain", "--data-dir", dir, "--reason", "maintenance"); r.code != 0 || !strings.Contains(r.stdout, "draining: maintenance") {
			t.Fatalf("longarmd drain: %+v", r)
		}
		h.waitNode(t, onB, "draining", func(n map[string]any) bool { return isTrue(n["draining"]) && n["drain_reason"] == "maintenance" })
		r = run("--mem-gb", small, "--no-queue", "--wait", "--", bins.helper, "print", "1", "0", "0")
		if r.code != 0 || placedOn(t, r.stderr) != onA {
			t.Fatalf("locally drained %s still got work: %+v", onB, r)
		}
		if r := longarmdCmd(t, "undrain", "--data-dir", dir); r.code != 0 {
			t.Fatalf("longarmd undrain: %+v", r)
		}
		h.waitNode(t, onB, "undrained", func(n map[string]any) bool { return !isTrue(n["draining"]) })
		// Label constraints: nobody has pool=gpu -> --no-queue fails, else queue.
		r = run("--mem-gb", small, "--require", "pool=gpu", "--no-queue", "--", bins.helper, "print", "1", "0", "0")
		if r.code != 75 || !strings.Contains(r.stderr, "pool") || !strings.Contains(r.stderr, "ResourceExhausted") {
			t.Fatalf("unsatisfiable constraint: %+v (want exit 75: refused for capacity)", r)
		}
		bg := h.longarmBG(t, h.client, "run", "--mem-gb", small, "--require", "pool=gpu", "--wait", "--", bins.helper, "print", "1", "0", "0")
		waitOutput(t, bg.stderr.String, "queued", 20*time.Second)
		h.mustLongarm(h.admin, "admin", "label", "wb", "pool=gpu,region=eu")
		if code := bg.wait(t, 60*time.Second); code != 0 || placedOn(t, bg.stderr.String()) != "wb" {
			t.Fatalf("labelled placement: %d %s", code, bg.stderr.String())
		}
		// Comma-separated label lists: the join code's "pool=e2e,tier=test"
		// became two labels, longarm admin label set two, --require takes a list.
		h.waitNode(t, "wb", "labels", func(n map[string]any) bool {
			l, _ := n["labels"].(map[string]any)
			return l["pool"] == "gpu" && l["region"] == "eu" && l["tier"] == "test"
		})
		if r := run("--mem-gb", small, "--require", "pool=gpu,region=eu", "--wait", "--", bins.helper, "print", "1", "0", "0"); r.code != 0 || placedOn(t, r.stderr) != "wb" {
			t.Fatalf("--require list: %+v", r)
		}
		for _, bad := range [][]string{{"admin", "join-code", "--labels", "region=eu,os"}, {"admin", "label", "wb", "region=eu,,x=1"},
			{"admin", "join-code", "--labels", "region=eu", "--labels", "region=us"}} {
			if r := h.longarm(h.admin, bad...); r.code == 0 {
				t.Fatalf("malformed labels accepted: %v %+v", bad, r)
			}
		}
	})

	t.Run("client_side_fallback", func(t *testing.T) {
		h.killHub()
		r := run("--wait", "--", bins.helper, "print", "1", "0", "5")
		if r.code != 5 || !strings.Contains(r.stderr, "client-side") {
			t.Fatalf("fallback placement: %+v", r)
		}
	})
}
