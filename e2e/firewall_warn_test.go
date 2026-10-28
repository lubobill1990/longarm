package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
)

// TestFirewallWarning: a Windows Firewall Block rule for longarm.exe / longarmd.exe
// (what dismissing the firewall prompt adds) is surfaced where the user
// looks - longarm daemon status, longarm pool and longarm ls (when legs are relayed),
// longarm firewall status, longarmd status - with the fix command. The rules come
// from a fake rule source (LONGARM_TEST_FIREWALL_RULES), so this runs on every
// OS and touches no real firewall.
func TestFirewallWarning(t *testing.T) {
	if testing.Short() {
		t.Skip("slow; skipped with -short")
	}
	t.Parallel()
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	rules := filepath.Join(t.TempDir(), "rules.txt")
	os.WriteFile(rules, []byte(strings.Join([]string{
		"v2.33|Action=Allow|Active=TRUE|Dir=In|Protocol=17|App=" + abs(bins.longarm) + "|Name=Longarm longarm (UDP)|",
		"v2.33|Action=Block|Active=TRUE|Dir=In|Protocol=17|Profile=Public|App=" + abs(bins.longarm) + "|Name=longarm.exe|",
		"v2.33|Action=Block|Active=TRUE|Dir=In|Protocol=17|Profile=Public|App=" + abs(bins.longarmd) + "|Name=longarmd.exe|",
	}, "\n")), 0o600)
	fwEnv := platform.TestFirewallRulesEnv + "=" + rules

	h := newHubEnv(t, "")
	os.Remove(filepath.Join(h.client, "workers.toml")) // workers come from the hub
	wa := h.newWorkerDir(t, "wa", "")
	wa.env = []string{"LONGARM_NET_DISABLE_DIRECT=1", fwEnv} // longnet stays on the relay
	os.WriteFile(filepath.Join(wa.dataDir, "longarmd.toml"), []byte(fmt.Sprintf(`listen = ["longnet://%s?name=wa"]
[hub]
advertise = ["longnet://%s?name=wa"]
`, h.addr(), h.addr())), 0o600)
	h.joinWorker(t, wa, true)
	wa.start(t, false)
	h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })

	env := []string{fwEnv, "LONGARM_POOL_INTERVAL=200ms"}
	fix := "firewall allow"

	// longarm firewall status
	r := h.longarmWith(env, h.client, "firewall", "status")
	if r.code == 0 || !strings.Contains(r.stdout, "blocked:") || !strings.Contains(r.stdout, `"longarm.exe (public)"`) || !strings.Contains(r.stdout, fix) {
		t.Fatalf("longarm firewall status: %+v", r)
	}

	// longarm daemon status / longarm pool
	if r := h.longarmWith(env, h.client, "daemon", "start"); r.code != 0 {
		t.Fatalf("longarm daemon start: %+v", r)
	}
	t.Cleanup(func() { h.longarmWith(nil, h.client, "daemon", "stop") })
	st := h.longarmWith(env, h.client, "daemon", "status").stdout
	if !strings.Contains(st, "firewall PROBLEM") || !strings.Contains(st, "Block rule") || !strings.Contains(st, fix) {
		t.Fatalf("longarm daemon status:\n%s", st)
	}
	var pool string
	waitFor(t, "longarm pool warns about the relayed leg", 60*time.Second, func() bool {
		pool = h.longarm(h.client, "pool").stdout
		return strings.Contains(pool, "warning: relayed to wa (longnet relay-")
	})
	if !strings.Contains(pool, fix) || !strings.Contains(pool, "Block rule") {
		t.Fatalf("longarm pool:\n%s", pool)
	}
	if js := h.mustLongarm(h.client, "pool", "--json"); !strings.Contains(js, `"firewall":`) || !strings.Contains(js, `"blocking":["longarm.exe (public)"]`) {
		t.Fatalf("longarm pool --json: %s", js)
	}

	// longarm ls (direct probes): the relayed path gets the note on stderr.
	r = h.longarmWith(env, h.client, "ls", "--direct", "-o", "wide")
	if r.code != 0 || !strings.Contains(r.stdout, "longnet/relay") || !strings.Contains(r.stderr, "warning: relayed to wa") || !strings.Contains(r.stderr, fix) {
		t.Fatalf("longarm ls --direct: %+v", r)
	}
	// Without a firewall to check (not Windows, no fake rules): no note.
	r = h.longarm(h.client, "ls", "--direct", "-o", "wide")
	if strings.Contains(r.stderr, "warning: relayed") && runtime.GOOS != "windows" {
		t.Fatalf("longarm ls without a firewall problem: %+v", r)
	}

	// longarmd status (the worker's own longarmd.exe)
	out := longarmdCmd(t, "status", "--data-dir", wa.dataDir).stdout
	if !strings.Contains(out, "firewall    PROBLEM") || !strings.Contains(out, "longarmd.exe (public)") || !strings.Contains(out, "--program") {
		t.Fatalf("longarmd status:\n%s", out)
	}
}
