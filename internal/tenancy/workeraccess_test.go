package tenancy

import (
	"os"
	"path/filepath"
	"testing"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func TestMayUseWorker(t *testing.T) {
	for _, c := range []struct {
		user   string
		teams  []string
		owner  string
		shares *pb.WorkerShares
		want   bool
	}{
		{"bob", nil, "", nil, true},
		{"", nil, "", nil, true},
		{"", nil, "alice", &pb.WorkerShares{Everyone: true}, false},
		{"alice", nil, "alice", nil, true},
		{"bob", nil, "alice", nil, false},
		{"bob", nil, "alice", &pb.WorkerShares{Users: []string{"bob"}}, true},
		{"bob", []string{"t1"}, "alice", &pb.WorkerShares{Teams: []string{"t1"}}, true},
		{"bob", []string{"t2"}, "alice", &pb.WorkerShares{Teams: []string{"t1"}}, false},
		{"bob", nil, "alice", &pb.WorkerShares{Everyone: true}, true},
	} {
		if got := MayUseWorker(c.user, c.teams, c.owner, c.shares); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
}

// TestWorkerAccessFile: the hub's word survives restarts; a missing file
// means no owner, a broken one is an error (longarmd then fails closed).
func TestWorkerAccessFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), WorkerAccessFile)
	w, err := LoadWorkerAccess(path)
	if err != nil || w.Owner() != "" || !w.Allows("anybody", nil) {
		t.Fatalf("missing file: %v %v", w.Get(), err)
	}
	if err := w.Set(&pb.WorkerAccess{Version: 3, Owner: "alice", Shares: &pb.WorkerShares{Users: []string{"bob"}}}); err != nil {
		t.Fatal(err)
	}
	w2, err := LoadWorkerAccess(path)
	if err != nil || w2.Owner() != "alice" || w2.Get().Version != 3 || !w2.Allows("bob", nil) || w2.Allows("carol", nil) {
		t.Fatalf("reloaded: %v %v", w2.Get(), err)
	}
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := LoadWorkerAccess(path); err == nil {
		t.Fatal("a broken file loads")
	}
	var nilW *WorkerAccess
	if nilW.Owner() != "" || !nilW.Allows("x", nil) {
		t.Fatal("nil worker access restricts")
	}
}
