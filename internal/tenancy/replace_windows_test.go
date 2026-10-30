package tenancy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestWriteWorkerAccessWhileRead: a reader holding worker-access.json open
// (os.Open shares no delete access on Windows) must not make the save fail
// with "Access is denied" - the worker would keep the stale file.
func TestWriteWorkerAccessWhileRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), WorkerAccessFile)
	if err := WriteWorkerAccess(path, &pb.WorkerAccess{Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(200 * time.Millisecond); f.Close() }()
	if err := WriteWorkerAccess(path, &pb.WorkerAccess{Owner: "alice", Version: 2, Shares: &pb.WorkerShares{Teams: []string{"t1"}}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"t1"`) {
		t.Fatalf("file: %s", b)
	}
}
