package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestReleaseRecordsAndUpdateAvailable: the public release records the
// LongarmUpdate service reads and CheckUpdate
// with want (an owner's longarm worker update).
func TestReleaseRecordsAndUpdateAvailable(t *testing.T) {
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
	for _, v := range []string{"1.0.0", "1.1.0"} {
		files := map[string]string{"longarmd-linux-amd64": "d" + v, "longarmd-linux-amd64.minisig": "s" + v,
			"longarm-windows-amd64.msi": "msi" + v, "longarm-windows-amd64.msi.minisig": "m" + v}
		if _, err := addRelease(ctx, h.rel, h.store, releaseDir(t, files), v, "", "", false); err != nil {
			t.Fatal(err)
		}
	}
	mux := h.httpHandler()
	get := func(path string) (int, releaseJSON) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		var r releaseJSON
		json.Unmarshal(w.Body.Bytes(), &r)
		return w.Code, r
	}
	code, latest := get("/releases/latest.json")
	if code != 200 || latest.Version != "1.1.0" || len(latest.Files) != 2 {
		t.Fatalf("latest.json: %d %+v", code, latest)
	}
	var msi *releaseFileJSON
	for i, f := range latest.Files {
		if f.Name == "longarm-windows-amd64.msi" {
			msi = &latest.Files[i]
		}
	}
	if msi == nil || msi.Size != int64(len("msi1.1.0")) || len(msi.SHA256) != 64 || !msi.Signed || msi.OS != "windows" || msi.Arch != "amd64" {
		t.Fatalf("msi record %+v", msi)
	}
	if code, r := get("/releases/1.0.0/release.json"); code != 200 || r.Version != "1.0.0" {
		t.Fatalf("release.json: %d %+v", code, r)
	}
	if code, _ := get("/releases/9.9.9/release.json"); code != 404 {
		t.Fatalf("unknown version: %d", code)
	}
	// the files themselves still come from their own route
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/releases/1.1.0/longarm-windows-amd64.msi", nil))
	if w.Code != 200 || w.Body.String() != "msi1.1.0" {
		t.Fatalf("file: %d %q", w.Code, w.Body.String())
	}

	for _, tc := range []struct {
		want, version string
		code          codes.Code
	}{
		{"latest", "1.1.0", codes.OK},
		{"1.0.0", "1.0.0", codes.OK},
		{"9.9.9", "", codes.NotFound},
		{"../1.0.0", "", codes.InvalidArgument},
	} {
		resp, err := h.wantedRelease(&pb.UpdateCheckRequest{Os: "linux", Arch: "amd64", Version: "1.0.0", Want: tc.want}, "stable")
		if status.Code(err) != tc.code {
			t.Fatalf("want %s: %v", tc.want, err)
		}
		if err == nil && (resp.Version != tc.version || len(resp.Files) != 1 || resp.Files[0].Name != "longarmd-linux-amd64") {
			t.Fatalf("want %s: %+v", tc.want, resp)
		}
	}
	if _, err := h.wantedRelease(&pb.UpdateCheckRequest{Os: "darwin", Arch: "arm64", Want: "latest"}, "stable"); status.Code(err) != codes.NotFound {
		t.Fatalf("no files for the platform: %v", err)
	}
}
