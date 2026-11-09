package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/longarmd"
)

// TestHubAdminWorkerRelay: the admin worker (the MSI's longarmd-admin
// service) listens on loopback only, like the session worker, and must get
// the hub relay legs as well. Its sign-in keeps the hub in hub.toml as
// typed for the installer ("hub.example", "https://hub.example"), not the
// resolved host:port the session worker's sign-in writes; the relay legs
// once built their listen URLs from that unnormalized address, found none,
// and the admin worker stayed loopback-only without a word in its log.
//
// Here a worker is made the admin worker (the installer's marker, the
// provenance of its credentials, the owner-only [access] its sign-in pins)
// with the hub as typed. It reports a longnet endpoint on the hub's relay,
// its owner runs exec through it, and the admin worker's properties hold
// over the relay: nobody but the owner gets in.
func TestHubAdminWorkerRelay(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	os.Remove(filepath.Join(h.client, "workers.toml")) // workers come from the hub

	w := h.newWorkerDir(t, "box-admin", "") // listen = ["tcp://127.0.0.1:<port>"] only
	h.joinWorker(t, w, true)

	// The admin worker as its install and sign-in leave it.
	handoff := filepath.Join(h.root, "admin-signin")
	if err := os.MkdirAll(handoff, 0o700); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(map[string]string{"handoff": handoff})
	if err := os.WriteFile(filepath.Join(w.dataDir, longarmd.AdminMarkerFile), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	prov := filepath.Join(h.root, "provenance.json")
	b, _ := json.Marshal(map[string]string{longarmd.ProvAdminCA: h.fp})
	if err := os.WriteFile(prov, b, 0o600); err != nil {
		t.Fatal(err)
	}
	w.env = []string{longarmd.ProvenanceFileEnv + "=" + prov}
	appendFile(t, filepath.Join(w.dataDir, "longarmd.toml"),
		"\n[access]\nusers = [\"alice\"]\nowners = [\"alice\"]\nadmins = \"none\"\n")
	hubFile := filepath.Join(w.dataDir, "hub.toml")
	hf, err := os.ReadFile(hubFile)
	if err != nil {
		t.Fatal(err)
	}
	typed := regexp.MustCompile(`(?m)^address = .*$`).ReplaceAll(hf, []byte(fmt.Sprintf("address = %q", "https://"+h.addr()+"/")))
	if err := os.WriteFile(hubFile, typed, 0o600); err != nil {
		t.Fatal(err)
	}

	w.start(t, true)
	want := fmt.Sprintf("longnet://%s?name=box-admin", h.addr()) // the test hub's relay shares its port
	h.waitNode(t, "box-admin", "online with the hub relay leg", func(n map[string]any) bool {
		return isTrue(n["online"]) && strings.Contains(fmt.Sprint(n["endpoints"]), want)
	})
	log, _ := os.ReadFile(w.logPath)
	for _, s := range []string{"admin worker: hub-initiated actions", "listening on the hub's relay", "hub: endpoints reported to the hub"} {
		if !strings.Contains(string(log), s) {
			t.Fatalf("no %q in the admin worker's log:\n%s", s, tail(string(log), 4000))
		}
	}

	// The owner, over the relay only.
	r := h.longarm(h.client, "--transport", "longnet", "exec", "box-admin", "--", bins.helper, "print", "3", "0", "0")
	if r.code != 0 || r.stdout != lines(1, 3) {
		t.Fatalf("exec through the hub relay leg: %+v", r)
	}
	if info := h.mustLongarm(h.client, "--transport", "longnet", "info", "box-admin"); !strings.Contains(info, "path: longnet/") {
		t.Fatalf("info:\n%s", info)
	}
	// A hub admin is not the owner: refused over the relay as directly.
	if r := h.longarm(h.admin, "--transport", "longnet", "exec", "box-admin", "--", bins.helper, "print", "1", "0", "0"); r.code == 0 ||
		!strings.Contains(r.stderr, "not a user of this worker") {
		t.Fatalf("a hub admin on the admin worker: %+v", r)
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}
