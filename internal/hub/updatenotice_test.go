package hub

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestUpdateNotice: Whoami, CheckUpdate and /releases/latest.json tell
// about the newest stable release whose files the hub has.
func TestUpdateNotice(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := InitCA(dir); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: dir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "off", PublicURL: "https://hub.example.com"}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	h, err := Open(cfg, slog.New(slog.NewTextHandler(&syncWriter{w: &bytes.Buffer{}}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	fs := &fleetServer{h: h}
	who := func(goos, goarch string) *pb.WhoamiResponse {
		h.latest.mu.Lock()
		h.latest.at = h.latest.at.AddDate(-1, 0, 0) // expire the cache
		h.latest.mu.Unlock()
		cctx := context.WithValue(ctx, callerKey{}, caller{id: pki.Identity{Role: pki.RoleClient, Name: "c1"}, cert: &x509.Certificate{}})
		r, err := fs.Whoami(cctx, &pb.WhoamiRequest{Os: goos, Arch: goarch})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := who("windows", "amd64"); r.LatestVersion != "" || r.LatestPath != "" || r.PublicUrl != "https://hub.example.com" {
		t.Fatalf("no release: %v", r)
	}
	files := map[string]string{"longarmd-windows-amd64.exe": "d", "longarm-windows-amd64.msi": "msi", "longarm-windows-amd64.msi.minisig": "sig"}
	if _, err := addRelease(ctx, h.rel, h.store, releaseDir(t, files), "1.2.0", "", "", false); err != nil {
		t.Fatal(err)
	}
	if r := who("windows", "amd64"); r.LatestVersion != "1.2.0" || r.LatestPath != "/releases/1.2.0/longarm-windows-amd64.msi" {
		t.Fatalf("windows: %v", r)
	}
	if r := who("windows", "arm64"); r.LatestPath != "/install.ps1" {
		t.Fatalf("windows/arm64 (no MSI): %v", r)
	}
	if r := who("linux", "amd64"); r.LatestPath != "/install.sh" {
		t.Fatalf("linux: %v", r)
	}
	// a beta doesn't make a notice
	if _, err := addRelease(ctx, h.rel, h.store, releaseDir(t, files), "1.3.0-rc1", "beta", "", false); err != nil {
		t.Fatal(err)
	}
	if r := who("windows", "amd64"); r.LatestVersion != "1.2.0" {
		t.Fatalf("after a beta: %v", r)
	}
	// a stable version that lost a file: still the complete one
	if _, err := addRelease(ctx, h.rel, h.store, releaseDir(t, files), "1.4.0", "", "", false); err != nil {
		t.Fatal(err)
	}
	if r := who("windows", "amd64"); r.LatestVersion != "1.4.0" {
		t.Fatalf("1.4.0: %v", r)
	}
	os.Remove(h.rel.localPath("1.4.0", "longarm-windows-amd64.msi"))
	h.rel.reconcile(ctx, h.store)
	if r := who("windows", "amd64"); r.LatestVersion != "1.2.0" {
		t.Fatalf("1.4.0 incomplete: %v", r)
	}

	// /releases/latest.json: sha256 and signature presence of every file
	w := httptest.NewRecorder()
	h.latestJSON(w, httptest.NewRequest(http.MethodGet, "/releases/latest.json", nil))
	var lj struct {
		Version string
		Files   []latestFile
	}
	if err := json.Unmarshal(w.Body.Bytes(), &lj); err != nil || lj.Version != "1.2.0" {
		t.Fatalf("latest.json: %v %s", err, w.Body)
	}
	found := false
	for _, f := range lj.Files {
		if f.Name == "longarm-windows-amd64.msi" {
			found = f.SHA256 == sha256Hex([]byte("msi")) && f.Signed && f.Size == 3
		}
	}
	if !found {
		t.Fatalf("latest.json without the MSI's record: %s", w.Body)
	}

	// CheckUpdate: a worker of the stable channel hears of 1.2.0 without a rollout
	h.mu.Lock()
	h.putTestNode(&node{rec: &store.Node{Name: "w1", Role: "worker", State: stateApproved, Version: "1.0.0"}})
	h.mu.Unlock()
	wctx := context.WithValue(ctx, callerKey{}, caller{id: pki.Identity{Role: pki.RoleWorker, Name: "w1"}, cert: &x509.Certificate{}})
	resp, err := fs.CheckUpdate(wctx, &pb.UpdateCheckRequest{Os: "windows", Arch: "amd64", Version: "1.0.0", Channel: "stable"})
	if err != nil || resp.Version != "" || resp.LatestVersion != "1.2.0" {
		t.Fatalf("CheckUpdate: %v %v", resp, err)
	}
	resp, err = fs.CheckUpdate(wctx, &pb.UpdateCheckRequest{Os: "windows", Arch: "amd64", Version: "1.0.0", Channel: "beta"})
	if err != nil || resp.LatestVersion != "" {
		t.Fatalf("CheckUpdate on beta: %v %v", resp, err)
	}
}

// TestPushReleaseAllowNone: no worker can self-update - with allow_none
// the push answers state "none" listing them (nothing started), without
// it FailedPrecondition as before.
func TestPushReleaseAllowNone(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := InitCA(dir); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: dir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "off"}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	h, err := Open(cfg, slog.New(slog.NewTextHandler(&syncWriter{w: &bytes.Buffer{}}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	files := map[string]string{"longarmd-windows-amd64.exe": "d", "longarmd-windows-amd64.exe.minisig": "sig"}
	if _, err := addRelease(ctx, h.rel, h.store, releaseDir(t, files), "1.2.0", "", "", false); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	for _, name := range []string{"msi-b", "msi-a"} {
		h.putTestNode(&node{rec: &store.Node{Name: name, Role: "worker", State: stateApproved, Version: "1.1.0"}})
	}
	h.mu.Unlock()
	if _, err := h.pushRelease(&pb.PushReleaseRequest{Version: "1.2.0"}, "test"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("without allow_none: %v", err)
	}
	r, err := h.pushRelease(&pb.PushReleaseRequest{Version: "1.2.0", AllowNone: true}, "test")
	if err != nil || r.State != rolloutNone || strings.Join(r.NotCapable, ",") != "msi-a,msi-b" {
		t.Fatalf("allow_none: %v %v", r, err)
	}
	h.mu.Lock()
	started := h.rollout != nil
	h.mu.Unlock()
	if started {
		t.Fatal("a rollout was started")
	}
}
