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

type fakeTools struct {
	Tools map[string]string `json:"tools"`
	Fail  []string          `json:"fail,omitempty"`
	Log   []string          `json:"log,omitempty"`
}

func readFake(t *testing.T, path string) fakeTools {
	t.Helper()
	var f fakeTools
	b, _ := os.ReadFile(path)
	json.Unmarshal(b, &f)
	return f
}

// TestHubProfiles: toolchain profiles reconciled by longarmd with a fake
// installer backend. A profile change goes to a canary first; the canary
// waits for its running job (draining meanwhile) and the other worker keeps
// the old revision until the canary reports ok. A failing canary halts the
// rollout.
func TestHubProfiles(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	fake := map[string]string{}
	var ws []*hubWorker
	for _, name := range []string{"wa", "wb"} {
		fake[name] = filepath.Join(h.root, "fake-"+name+".json")
		os.WriteFile(fake[name], []byte(`{"tools":{"git":"2.40.1"}}`), 0o600)
		w := h.newWorkerDir(t, name, fmt.Sprintf("[toolchain]\nbackend = \"fake\"\nfake_state = %q\n", filepath.ToSlash(fake[name])))
		h.joinWorker(t, w, true)
		w.start(t, true)
		ws = append(ws, w)
	}
	for _, n := range []string{"wa", "wb"} {
		h.waitNode(t, n, "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	}
	profileLs := func() string { return h.mustLongarm(h.admin, "admin", "profile", "ls") }
	toolchainState := func(n map[string]any) (state string, rev string) {
		tc, _ := n["toolchain"].(map[string]any)
		return fmt.Sprint(tc["state"]), fmt.Sprint(tc["revision"])
	}

	// r1: nobody assigned yet; then both workers get it.
	h.mustLongarm(h.admin, "admin", "profile", "set", "dev", "git=2.45", "node=22")
	for _, n := range []string{"wa", "wb"} {
		h.mustLongarm(h.admin, "admin", "profile", "assign", n, "dev")
	}
	for _, n := range []string{"wa", "wb"} {
		h.waitNode(t, n, "profile r1 ok", func(n map[string]any) bool { s, r := toolchainState(n); return s == "ok" && r == "1" })
	}
	if f := readFake(t, fake["wa"]); f.Tools["git"] != "2.45.0" || f.Tools["node"] != "22.0.0" {
		t.Fatalf("wa tools %v", f.Tools)
	}

	// r2 with a job running on the canary (wa: first by name).
	id := strings.TrimSpace(h.mustLongarm(h.client, "run", "wa", "--", bins.helper, "sleep", "120"))
	h.mustLongarm(h.admin, "admin", "profile", "set", "dev", "git=2.45", "node=22", "pnpm=latest")
	h.waitNode(t, "wa", "canary waiting for jobs", func(n map[string]any) bool {
		s, r := toolchainState(n)
		return s == "pending" && r == "2" && isTrue(n["draining"])
	})
	if out := profileLs(); !strings.Contains(out, "canary") || !strings.Contains(out, "wa") {
		t.Fatalf("rollout: %s", out)
	}
	time.Sleep(time.Second)
	if f := readFake(t, fake["wb"]); f.Tools["pnpm"] != "" {
		t.Fatal("wb got revision 2 before the canary passed")
	}
	if f := readFake(t, fake["wa"]); f.Tools["pnpm"] != "" {
		t.Fatal("canary installed while a job was running")
	}
	h.mustLongarm(h.client, "kill", "wa", id)
	for _, n := range []string{"wa", "wb"} {
		h.waitNode(t, n, "profile r2 ok", func(n map[string]any) bool { s, r := toolchainState(n); return s == "ok" && r == "2" })
	}
	waitFor(t, "rollout done", 20*time.Second, func() bool { return strings.Contains(profileLs(), "done") })
	if n := h.waitNode(t, "wa", "undrained", func(n map[string]any) bool { return !isTrue(n["draining"]) }); n == nil {
		t.Fatal()
	}
	fa, fb := readFake(t, fake["wa"]), readFake(t, fake["wb"])
	if fa.Tools["pnpm"] != "99.0.0" || fb.Tools["pnpm"] != "99.0.0" || strings.Count(strings.Join(fa.Log, ","), "git@") != 1 {
		t.Fatalf("installs wa=%v wb=%v", fa, fb)
	}

	// r3: the canary fails -> halted; wb stays on r2.
	fa.Fail = []string{"uv"}
	b, _ := json.Marshal(fa)
	os.WriteFile(fake["wa"], b, 0o600)
	h.mustLongarm(h.admin, "admin", "profile", "set", "dev", "git=2.45", "node=22", "pnpm=latest", "uv=latest")
	waitFor(t, "halted rollout", 30*time.Second, func() bool { return strings.Contains(profileLs(), "halted") })
	time.Sleep(time.Second)
	if f := readFake(t, fake["wb"]); f.Tools["uv"] != "" {
		t.Fatal("wb got the failed revision")
	}
	if n := h.waitNode(t, "wb", "still r2", func(map[string]any) bool { return true }); fmt.Sprint(n["toolchain"].(map[string]any)["revision"]) != "2" {
		t.Fatalf("wb toolchain %v", n["toolchain"])
	}
	// longarmd's local view agrees.
	st := longarmdCmd(t, "toolchain", "status", "--data-dir", ws[0].dataDir)
	if st.code != 0 || !strings.Contains(st.stdout, `"failed"`) {
		t.Fatalf("longarmd toolchain status: %+v", st)
	}
}
