package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/trust"
)

// TestEndToEndTrustWorker: a worker whose owner pinned alice's user key
// accepts only alice's endorsed devices - not the hub's admin, not a
// certificate the CA mints for alice on its own - and the hub session
// can't read jobs. The worker's own endorsement reaches its certificate
// through a renewal (the hub copies the extension).
func TestEndToEndTrustWorker(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	wa := h.newWorkerDir(t, "wa", "")
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "online", func(n map[string]any) bool { return isTrue(n["online"]) })
	ca, err := pki.LoadCA(filepath.Join(h.client, "ca")) // what a thief of the hub's CA key holds
	if err != nil {
		t.Fatal(err)
	}
	alicePub, aliceKey, _ := ed25519.GenerateKey(rand.Reader)
	execOn := func(cfgDir string) result {
		return h.longarm(cfgDir, "exec", "wa", "--", bins.helper, "print", "1", "0", "0")
	}
	workerCert := func() *pki.TrustExt {
		b, _ := os.ReadFile(filepath.Join(wa.dataDir, "tls", "node.crt"))
		c, err := pki.ParseCertPEM(b)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := pki.TrustOf(c)
		if err != nil {
			t.Fatal(err)
		}
		return tx
	}

	// joined with a binding to its node key, not endorsed yet
	if tx := workerCert(); tx == nil || tx.Endorsement != nil {
		t.Fatalf("worker certificate after join: %+v", tx)
	}
	if r := execOn(h.admin); r.code != 0 {
		t.Fatalf("no trust policy: the admin's exec failed: %+v", r)
	}

	// the owner pins alice: end-to-end trust is on
	r := longarmdCmd(t, "trust", "add-user", pki.UserKeyLine{User: "alice", Key: alicePub}.String(), "--data-dir", wa.dataDir)
	if r.code != 0 || !strings.Contains(r.stdout, "end-to-end trust is on") {
		t.Fatalf("trust add-user: %+v", r)
	}
	waitFor(t, "the policy reload", 20*time.Second, func() bool {
		b, _ := os.ReadFile(wa.logPath)
		return strings.Contains(string(b), "trust policy reloaded")
	})
	for _, dir := range []string{h.admin, h.client} {
		if r := execOn(dir); r.code == 0 {
			t.Fatalf("%s: exec without an endorsement succeeded", filepath.Base(dir))
		}
	}

	// alice's device, endorsed by her key (what longarm trust init does)
	nk, err := trust.Store{Dir: filepath.Join(h.client, "trust")}.NodeKey()
	if err != nil {
		t.Fatal(err)
	}
	e, err := pki.Endorse(aliceKey, "alice", pki.KindDevice, "alice", nk.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	mint := func(dir string, o pki.IssueOptions, tr *pki.CSRTrust) {
		keyPEM, csrPEM, err := pki.NewKeyAndCSR(o.Name, tr)
		if err != nil {
			t.Fatal(err)
		}
		certPEM, err := ca.SignCSR(csrPEM, o)
		if err != nil {
			t.Fatal(err)
		}
		if err := pki.WriteBundleAtomic(filepath.Join(dir, "tls"), ca.CertPEM, certPEM, keyPEM); err != nil {
			t.Fatal(err)
		}
	}
	mint(h.client, pki.IssueOptions{Role: pki.RoleClient, Name: "alice"}, &pki.CSRTrust{NodeKey: nk, Endorsement: e})
	if r := execOn(h.client); r.code != 0 || !strings.Contains(r.stdout, "line 1") {
		t.Fatalf("endorsed alice: %+v", r)
	}

	// the CA mints "alice" an admin certificate of its own (bound to a
	// node key, but nobody endorsed it): refused
	thief := filepath.Join(h.root, "thief")
	os.MkdirAll(thief, 0o700)
	for _, f := range []string{"hub.toml", "workers.toml"} {
		if b, err := os.ReadFile(filepath.Join(h.client, f)); err == nil {
			os.WriteFile(filepath.Join(thief, f), b, 0o600)
		}
	}
	_, tk, _ := ed25519.GenerateKey(rand.Reader)
	mint(thief, pki.IssueOptions{Role: pki.RoleAdmin, Name: "alice"}, &pki.CSRTrust{NodeKey: tk})
	if r := execOn(thief); r.code == 0 {
		t.Fatalf("a CA-minted alice certificate was accepted: %+v", r)
	}
	waitFor(t, "trust/denied in the audit log", 10*time.Second, func() bool {
		b, _ := os.ReadFile(filepath.Join(wa.dataDir, "audit.log"))
		return strings.Contains(string(b), "trust/denied") && strings.Contains(string(b), "not endorsed")
	})

	// the hub session can't reach jobs (remote_jobs defaults to none)
	id := strings.TrimSpace(h.mustLongarm(h.client, "run", "wa", "--", bins.helper, "print", "2", "0", "0"))
	h.mustLongarm(h.client, "wait", "wa", id)
	if r := h.longarm(h.admin, "admin", "logs", id); r.code == 0 || !strings.Contains(r.stderr, "remote_jobs") {
		t.Fatalf("admin logs through the hub: %+v", r)
	}

	// the worker's endorsement: request -> endorse (alice) -> accept ->
	// renewed certificate that carries it
	r = longarmdCmd(t, "trust", "request", "--data-dir", wa.dataDir)
	req, err := pki.ParseEndorseRequest(strings.SplitN(r.stdout, "\n", 2)[0])
	if err != nil || req.Kind != pki.KindWorker || req.Name != "wa" {
		t.Fatalf("trust request: %+v %v", r, err)
	}
	we, err := pki.Endorse(aliceKey, "alice", pki.KindWorker, req.Name, req.NodeKey)
	if err != nil {
		t.Fatal(err)
	}
	if r := longarmdCmd(t, "trust", "accept", we.String(), "--data-dir", wa.dataDir); r.code != 0 {
		t.Fatalf("trust accept: %+v", r)
	}
	waitFor(t, "a renewed certificate carrying the endorsement", 30*time.Second, func() bool {
		tx := workerCert()
		return tx != nil && tx.Endorsement != nil && tx.Endorsement.User == "alice"
	})
	if r := longarmdCmd(t, "trust", "status", "--data-dir", wa.dataDir); !strings.Contains(r.stdout, "certificate  endorsed by alice") {
		t.Fatalf("trust status: %+v", r)
	}
	if r := execOn(h.client); r.code != 0 {
		t.Fatalf("alice after the worker's renewal: %+v", r)
	}

	// an endorsement for another worker is refused
	other, _ := pki.Endorse(aliceKey, "alice", pki.KindWorker, "wb", req.NodeKey)
	if r := longarmdCmd(t, "trust", "accept", other.String(), "--data-dir", wa.dataDir); r.code == 0 {
		t.Fatalf("accepted an endorsement for another name: %+v", r)
	}

	// removing the last user turns it off again
	if r := longarmdCmd(t, "trust", "remove-user", "alice", "--data-dir", wa.dataDir); r.code != 0 || !strings.Contains(r.stdout, "end-to-end trust is off") {
		t.Fatalf("remove-user: %+v", r)
	}
	waitFor(t, "the admin back in", 20*time.Second, func() bool { return execOn(h.admin).code == 0 })
}
