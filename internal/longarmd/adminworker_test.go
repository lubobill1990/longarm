package longarmd

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestAdminWorkerHandoff: the admin worker (its service waiting for its
// sign-in) leaves its CSR in the hand-off dir; the user's sign-in (the
// tray: AdminWorkerTarget) hands the approved certificate back; the
// service installs it only when it is for its key, from the hub's CA and
// marked as the admin worker, and pins its owner in longarmd.toml.
func TestAdminWorkerHandoff(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ca, err := pki.InitCA(t.TempDir(), "test hub", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	setup := func() (dataDir, handoff string, target AdminWorkerTarget) {
		dataDir, handoff = t.TempDir(), filepath.Join(t.TempDir(), "admin-signin")
		if err := SetUpSignIn(dataDir, "longarm.example.com", ""); err != nil {
			t.Fatal(err)
		}
		if err := WriteAdminMarker(dataDir, handoff); err != nil {
			t.Fatal(err)
		}
		return dataDir, handoff, AdminWorkerTarget{Handoff: handoff, DataDir: dataDir, WaitRequest: time.Second, WaitInstall: 15 * time.Second}
	}
	respFor := func(csr []byte, labels map[string]string) *hubclient.JoinResult {
		cert, err := ca.SignCSR(csr, pki.IssueOptions{Role: pki.RoleWorker, Name: "w1-admin"})
		if err != nil {
			t.Fatal(err)
		}
		return &hubclient.JoinResult{Resp: &pb.JoinResponse{State: "approved", Role: "worker", Name: "w1-admin", Owner: "alice",
			CaPem: ca.CertPEM, CertPem: cert, Labels: labels}, CAFingerprint: pki.Fingerprint(ca.Cert)}
	}

	dataDir, handoff, target := setup()
	if _, err := target.CSR("longarm.example.com"); err == nil {
		t.Fatal("a CSR before the service asked")
	}
	adminHandoffStep(dataDir, log)
	csr, err := target.CSR("longarm.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.Prepare(""); err != nil {
		t.Fatal(err)
	}
	// the request is stable while it waits
	adminHandoffStep(dataDir, log)
	if again, _ := target.CSR(""); string(again) != string(csr) {
		t.Fatal("the request changed while waiting")
	}

	// the service takes the enrollment while Install waits
	go func() {
		for i := 0; i < 100; i++ {
			if fileExists(filepath.Join(handoff, AdminResponseFile)) {
				adminHandoffStep(dataDir, log)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	var notes []string
	dir, err := target.Install("longarm.example.com", respFor(csr, map[string]string{AdminLabel: "true"}), func(s string) { notes = append(notes, s) })
	if err != nil || dir != dataDir {
		t.Fatalf("install: %q %v", dir, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, TLSDir, pki.NodeCertFile)); err != nil {
		t.Fatal("no certificate installed")
	}
	if hf, _ := hubclient.LoadFile(dataDir); hf.SignIn || hf.Owner != "alice" {
		t.Fatalf("hub.toml %+v", hf)
	}
	toml, _ := os.ReadFile(filepath.Join(dataDir, ConfigFile))
	for _, w := range []string{"[access]", `users = ["alice"]`, `owners = ["alice"]`, `admins = "none"`} {
		if !strings.Contains(string(toml), w) {
			t.Errorf("longarmd.toml without %s:\n%s", w, toml)
		}
	}
	if fileExists(filepath.Join(handoff, AdminRequestFile)) || fileExists(filepath.Join(dataDir, TLSDir, adminKeyFile)) {
		t.Error("request or pending key left behind")
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "SYSTEM") {
		t.Errorf("notes %v", notes)
	}

	// refused: not marked as the admin worker (an old hub), or for another key
	for name, mk := range map[string]func(csr []byte) *hubclient.JoinResult{
		"no admin label": func(csr []byte) *hubclient.JoinResult { return respFor(csr, nil) },
		"another key": func([]byte) *hubclient.JoinResult {
			_, other, _ := pki.NewKeyAndCSR("", nil)
			return respFor(other, map[string]string{AdminLabel: "true"})
		},
	} {
		dataDir, handoff, target := setup()
		adminHandoffStep(dataDir, log)
		csr, err := target.CSR("")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for i := 0; i < 100; i++ {
				if fileExists(filepath.Join(handoff, AdminResponseFile)) {
					adminHandoffStep(dataDir, log)
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
		if _, err := target.Install("", mk(csr), nil); err == nil {
			t.Errorf("%s: installed", name)
		}
		if fileExists(filepath.Join(dataDir, TLSDir, pki.NodeCertFile)) {
			t.Errorf("%s: certificate written", name)
		}
		st := AdminWorkerStatus(AdminInstall{Enabled: true, Service: AdminServiceName, Handoff: handoff}, nil)
		if st.Error == "" || st.SignedIn {
			t.Errorf("%s: status %+v", name, st)
		}
	}
}
