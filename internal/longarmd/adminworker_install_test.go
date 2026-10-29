package longarmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/hubclient"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestAdminInstallRefusedBetweenChecks: the service refuses the enrollment
// (error written, then the response removed) right after Install looked for
// an error and before it looked for the response. Install must report the
// refusal, not "installed" because the response is gone.
func TestAdminInstallRefusedBetweenChecks(t *testing.T) {
	handoff := t.TempDir()
	target := AdminWorkerTarget{Handoff: handoff, DataDir: t.TempDir(), WaitInstall: 5 * time.Second}
	once := false
	installCheckHook = func() {
		if once {
			return
		}
		once = true
		os.WriteFile(filepath.Join(handoff, AdminErrorFile), []byte("the certificate is not for this worker's key\n"), 0o644)
		os.Remove(filepath.Join(handoff, AdminResponseFile))
	}
	t.Cleanup(func() { installCheckHook = nil })
	r := &hubclient.JoinResult{Resp: &pb.JoinResponse{State: "approved", Role: "worker", Name: "w1-admin", Owner: "alice"}}
	_, err := target.Install("longarm.example.com", r, nil)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Install: %v, want the refusal", err)
	}
}
