package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestLongarmdLoginAdminWorker: `longarmd login` on a machine whose admin
// worker (the MSI's ADMIN_WORKER=1) waits for its sign-in takes it along,
// as the tray does: one code, a prompt and an approval page that name the
// admin worker, an enrollment for the admin worker's own key marked
// longarm.io/admin=true and owned by whoever approved, handed back through
// the hand-off dir - and the hub records it as the companion of the worker
// the same approval enrolled (one machine, not two). The registry lookup
// (PendingAdminWorker) is the unit test's; here the target is given.
func TestLongarmdLoginAdminWorker(t *testing.T) {
	t.Parallel()
	a := newFakeAuthority(t)
	h := newHubEnv(t, a.hubConfig(""))
	web := entraPanel(t, h, a, "e7e70000-0000-4000-8000-000000000011", "erin@example.com")
	w := h.newWorkerDir(t, "law", "")

	// the admin worker's service: its own key, its CSR in the hand-off dir
	handoff := filepath.Join(h.root, "admin-signin")
	if err := os.MkdirAll(handoff, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPEM, csrPEM, err := pki.NewKeyAndCSR("", nil)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(longarmd.AdminRequest{Hub: h.addr(), CSR: string(csrPEM), Created: time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(handoff, longarmd.AdminRequestFile), req, 0o600); err != nil {
		t.Fatal(err)
	}
	// ... which takes the enrollment the sign-in leaves (adminHandoffStep)
	taken := make(chan *pb.JoinResponse, 1)
	go func() {
		resp := filepath.Join(handoff, longarmd.AdminResponseFile)
		for end := time.Now().Add(2 * time.Minute); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
			b, err := os.ReadFile(resp)
			if err != nil {
				continue
			}
			var ar longarmd.AdminResponse
			var jr pb.JoinResponse
			if json.Unmarshal(b, &ar) != nil || proto.Unmarshal(ar.Join, &jr) != nil {
				continue // written atomically, but be patient
			}
			os.Remove(filepath.Join(handoff, longarmd.AdminRequestFile))
			os.Remove(resp)
			taken <- &jr
			return
		}
		taken <- nil
	}()

	out := &syncBuf{}
	done := make(chan error, 1)
	go func() {
		done <- longarmd.Login(context.Background(), w.dataDir, h.addr(), longarmd.LoginOptions{Name: "law", Fingerprint: h.fp,
			Out: out, AdminWorker: &longarmd.AdminWorkerTarget{Handoff: handoff, DataDir: "admin-dir", WaitInstall: time.Minute}})
	}()
	var code string
	waitFor(t, "the code", 30*time.Second, func() bool {
		if m := linkCodeRE.FindStringSubmatch(out.String()); m != nil {
			code = m[2]
			return true
		}
		return false
	})
	if !strings.Contains(out.String(), "this worker and its admin worker as yours (the admin worker runs as SYSTEM") {
		t.Fatalf("the prompt doesn't name the admin worker:\n%s", out.String())
	}
	if page := web.decide(code, "approve", "This includes an ADMINISTRATOR worker", "runs commands as <b>SYSTEM</b>", "<code>law</code>"); !strings.Contains(page, "law") {
		t.Fatalf("approve: %s", page)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("longarmd login: %v\n%s", err, out.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatalf("longarmd login did not finish:\n%s", out.String())
	}
	if s := out.String(); strings.Count(s, "enter code") != 1 || !strings.Contains(s, "joined as worker law") ||
		!strings.Contains(s, "joined as admin worker law-admin (runs as SYSTEM, only erin can use it)") {
		t.Fatalf("longarmd login output:\n%s", s)
	}

	jr := <-taken
	if jr == nil {
		t.Fatal("no enrollment in the hand-off dir")
	}
	if jr.State != "approved" || jr.Owner != "erin" || jr.Name != "law-admin" || jr.Labels["longarm.io/admin"] != "true" {
		t.Fatalf("admin worker enrollment: %s %s %s %v", jr.State, jr.Owner, jr.Name, jr.Labels)
	}
	if _, err := tls.X509KeyPair(jr.CertPem, keyPEM); err != nil {
		t.Fatalf("the certificate isn't for the admin worker's key: %v", err)
	}

	// one machine: the hub pairs the admin worker with this login's worker
	web.waitNode("law-admin", "the admin worker, paired with law", func(n map[string]any) bool {
		return n["companion"] == "law" && (isTrue(n["admin_worker"]) || isTrue(n["adminWorker"]))
	})
	web.waitNode("law", "paired with law-admin", func(n map[string]any) bool { return n["companion"] == "law-admin" })
}
