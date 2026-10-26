package tenancy

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// User-owned workers (docs/security.md "Your own workers"): a worker a user
// registered (longarm worker add) is usable by that user and whoever it is
// shared with; a worker without owner by everybody the hub admits. The hub
// applies the rule when listing and placing; the worker enforces it on
// every call, as an extra restriction after its own [access] policy and
// end-to-end trust.

// MayUseWorker reports whether user (member of teams) may use a worker
// owned by owner ("" = no owner) with shares.
func MayUseWorker(user string, teams []string, owner string, shares *pb.WorkerShares) bool {
	if owner == "" {
		return true
	}
	if user == "" {
		return false
	}
	if user == owner || shares.GetEveryone() || slices.Contains(shares.GetUsers(), user) {
		return true
	}
	for _, t := range shares.GetTeams() {
		if slices.Contains(teams, t) {
			return true
		}
	}
	return false
}

// WorkerAccessFile is the worker's copy of its WorkerAccess (longarmd's
// data dir), so the rule survives restarts and hub outages.
const WorkerAccessFile = "worker-access.json"

// WorkerAccess holds the owner and shares the hub pushed to this worker.
type WorkerAccess struct {
	mu   sync.RWMutex
	path string
	a    *pb.WorkerAccess
}

// LoadWorkerAccess reads path ("" = memory only); a missing file means no
// owner.
func LoadWorkerAccess(path string) (*WorkerAccess, error) {
	w := &WorkerAccess{path: path, a: &pb.WorkerAccess{}}
	if path == "" {
		return w, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	a := &pb.WorkerAccess{}
	if err := protojson.Unmarshal(b, a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	w.a = a
	return w, nil
}

// Set replaces the owner and shares and saves them.
func (w *WorkerAccess) Set(a *pb.WorkerAccess) error {
	a = proto.Clone(a).(*pb.WorkerAccess)
	w.mu.Lock()
	w.a = a
	path := w.path
	w.mu.Unlock()
	if path == "" {
		return nil
	}
	b, err := protojson.MarshalOptions{Indent: "  "}.Marshal(a)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Clean(path))
}

// Get returns a copy.
func (w *WorkerAccess) Get() *pb.WorkerAccess {
	if w == nil {
		return &pb.WorkerAccess{}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return proto.Clone(w.a).(*pb.WorkerAccess)
}

// Owner of the worker ("" = none; nil-safe).
func (w *WorkerAccess) Owner() string {
	if w == nil {
		return ""
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.a.Owner
}

// Allows reports whether user (member of teams) may use the worker.
func (w *WorkerAccess) Allows(user string, teams []string) bool {
	if w == nil {
		return true
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return MayUseWorker(user, teams, w.a.Owner, w.a.Shares)
}

// SetPath sets the file later Sets save to.
func (w *WorkerAccess) SetPath(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.path = path
}
