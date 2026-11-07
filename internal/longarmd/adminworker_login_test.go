package longarmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPendingAdminWorker: `longarmd login` takes the admin worker along
// only when the MSI recorded it, its service waits for a sign-in to the
// same hub and this account can read its request (the hand-off dir's ACL
// decides who may sign it in); otherwise its own sign-in goes on without
// it, saying why when there is something to say.
func TestPendingAdminWorker(t *testing.T) {
	const csr = "-----BEGIN CERTIFICATE REQUEST-----\nMIIB\n-----END CERTIFICATE REQUEST-----\n"
	setup := func(request string) AdminInstall {
		handoff := t.TempDir()
		if request != "" {
			if err := os.WriteFile(filepath.Join(handoff, AdminRequestFile), []byte(request), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return AdminInstall{Enabled: true, Service: AdminServiceName, DataDir: "admin-dir", Handoff: handoff}
	}
	waiting := `{"hub":"longarm.example.com:7443","csr_pem":` + quoteJSON(csr) + `}`

	for _, tc := range []struct {
		name     string
		adm      AdminInstall
		hub      string
		target   bool
		noteHas  string // "" = no note
		mkReqDir bool   // the request unreadable: a directory in its place
	}{
		{name: "no admin worker", adm: AdminInstall{}, hub: "longarm.example.com"},
		{name: "disabled", adm: func() AdminInstall { a := setup(waiting); a.Enabled = false; return a }(), hub: "longarm.example.com"},
		{name: "not waiting", adm: setup(""), hub: "longarm.example.com"},
		{name: "waiting, bare host", adm: setup(waiting), hub: "longarm.example.com", target: true},
		{name: "waiting, https URL", adm: setup(waiting), hub: "https://longarm.example.com/", target: true},
		{name: "waiting, host:port", adm: setup(waiting), hub: "longarm.example.com:7443", target: true},
		{name: "another hub", adm: setup(waiting), hub: "other.example.com", noteHas: "waits for a sign-in to longarm.example.com:7443"},
		{name: "another port", adm: setup(waiting), hub: "longarm.example.com:8443", noteHas: "stays out"},
		{name: "no hub in the request", adm: setup(`{"csr_pem":` + quoteJSON(csr) + `}`), hub: "longarm.example.com", noteHas: "unusable"},
		{name: "no CSR", adm: setup(`{"hub":"longarm.example.com:7443"}`), hub: "longarm.example.com", noteHas: "unusable"},
		{name: "not JSON", adm: setup("{"), hub: "longarm.example.com", noteHas: "unusable"},
		{name: "unreadable", adm: setup(""), mkReqDir: true, hub: "longarm.example.com", noteHas: "can't hand it over"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mkReqDir {
				if err := os.Mkdir(filepath.Join(tc.adm.Handoff, AdminRequestFile), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			target, note := pendingAdminWorker(tc.adm, tc.hub)
			if (target != nil) != tc.target {
				t.Fatalf("target %+v, note %q", target, note)
			}
			if target != nil && (target.Handoff != tc.adm.Handoff || target.DataDir != "admin-dir") {
				t.Fatalf("target %+v", target)
			}
			if (note != "") != (tc.noteHas != "") || !strings.Contains(note, tc.noteHas) {
				t.Fatalf("note %q, want one with %q", note, tc.noteHas)
			}
		})
	}
}

func quoteJSON(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}
