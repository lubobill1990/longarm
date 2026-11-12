package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	pb "longarm/proto/longarm/v1"
)

func TestWorkerToolchain(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	h.mustLongarm(h.admin, "admin", "user", "add", "alice", "--no-code")
	fake := filepath.Join(h.root, "toolchain.json")
	if err := os.WriteFile(fake, []byte(`{"tools":{"git":"2.45.0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	w := h.newWorkerDir(t, "tools", fmt.Sprintf("[toolchain]\nbackend = \"fake\"\nfake_state = %q\n", filepath.ToSlash(fake)))
	code := strings.TrimSpace(h.mustLongarm(h.client, "worker", "add", "tools"))
	if r := longarmdCmd(t, "join", h.addr(), code, "--data-dir", w.dataDir, "--ca-fingerprint", h.fp); r.code != 0 {
		t.Fatalf("join: %+v", r)
	}
	w.start(t, true)
	read := func() *pb.WorkerToolchainResponse {
		t.Helper()
		out := h.mustLongarm(h.client, "worker", "toolchain", "tools", "--json")
		r := &pb.WorkerToolchainResponse{}
		if err := protojson.Unmarshal([]byte(out), r); err != nil {
			t.Fatalf("toolchain JSON: %v\n%s", err, out)
		}
		return r
	}
	waitFor(t, "inventory without a profile", 30*time.Second, func() bool {
		r := read()
		return r.Toolchain.GetState() == "none" && len(r.Toolchain.GetVersions()) == 9
	})
	out := h.mustLongarm(h.client, "worker", "ls", "--json")
	nodes := &pb.ListNodesResponse{}
	if err := protojson.Unmarshal([]byte(out), nodes); err != nil {
		t.Fatal(err)
	}
	var inventory *pb.ToolchainReport
	for _, n := range nodes.Nodes {
		if n.Name == "tools" {
			inventory = n.Toolchain
		}
	}
	if inventory.GetVersions()["git"] != "2.45.0" || len(inventory.GetVersions()) != 9 || inventory.GetVersions()["claude"] != "" {
		t.Fatalf("worker ls inventory: %v", inventory)
	}
	id := strings.TrimSpace(h.mustLongarm(h.client, "run", "tools", "--", bins.helper, "sleep", "120"))
	out = h.mustLongarm(h.client, "worker", "toolchain", "tools", "--tools", "claude,copilot,uv,node@22", "--apply", "--json")
	if err := protojson.Unmarshal([]byte(out), &pb.WorkerToolchainResponse{}); err != nil {
		t.Fatalf("set JSON: %v\n%s", err, out)
	}
	waitFor(t, "owner tools pending while busy", 30*time.Second, func() bool { return read().Toolchain.GetState() == "pending" })
	if f := readFake(t, fake); f.Tools["node"] != "" || f.Tools["claude"] != "" {
		t.Fatalf("installed while busy: %v", f)
	}
	h.mustLongarm(h.client, "kill", "tools", id)
	waitFor(t, "owner tools installed", 30*time.Second, func() bool { return read().Toolchain.GetState() == "ok" })
	r := read()
	if r.Desired.Profile != "" || r.Desired.OwnerRevision != 1 || r.Toolchain.OwnerRevision != 1 ||
		r.Toolchain.Versions["node"] != "22.0.0" || r.Toolchain.Versions["copilot"] != "99.0.0" {
		t.Fatalf("owner report: %v", r)
	}
	// Desired tools survive the hub restart; the worker re-reports its inventory.
	h.killHub()
	h.startHub(t)
	waitFor(t, "desired tools after hub restart", 30*time.Second, func() bool {
		r := read()
		return len(r.OwnerTools) == 4 && r.Toolchain.GetState() == "ok"
	})
	h.mustLongarm(h.client, "worker", "toolchain", "tools", "--clear", "--json")
	waitFor(t, "cleared owner desired tools", 30*time.Second, func() bool {
		r := read()
		return len(r.OwnerTools) == 0 && r.Toolchain.GetState() == "none" && r.Toolchain.Versions["node"] == "22.0.0"
	})
}
