package longarmd

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"longarm/internal/conntrack"
	"longarm/internal/hubclient"
	"longarm/internal/server"
	"longarm/internal/tenancy"
	pb "longarm/proto/longarm/v1"
)

func TestWorkerAccessPersistsOwner(t *testing.T) {
	dir := t.TempDir()
	if err := hubclient.SaveFile(dir, hubclient.Config{Address: "hub.example.com:8443"}); err != nil {
		t.Fatal(err)
	}
	a := &hubAgent{dataDir: dir, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		audit: server.NewAudit(filepath.Join(dir, "audit.log")),
		srv:   &server.Server{Conns: &conntrack.Tracker{}, WorkerAccess: tenancy.PendingWorkerAccess(filepath.Join(dir, tenancy.WorkerAccessFile), "")}}
	a.applyWorkerAccess(&pb.WorkerAccess{Version: 2, Owner: "alice"})
	cfg, err := hubclient.LoadFile(dir)
	if err != nil || cfg.Owner != "alice" || cfg.Address != "hub.example.com:8443" {
		t.Fatalf("persisted config: %+v %v", cfg, err)
	}
	a.applyWorkerAccess(&pb.WorkerAccess{Version: 1})
	cfg, err = hubclient.LoadFile(dir)
	if err != nil || cfg.Owner != "alice" {
		t.Fatalf("stale push cleared owner: %+v %v", cfg, err)
	}
}
