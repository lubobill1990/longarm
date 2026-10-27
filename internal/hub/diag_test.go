package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/diag"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

type diagHub struct {
	h                   *Hub
	blobDir             string
	logs                *bytes.Buffer
	worker, alice, root *grpc.ClientConn
}

func newDiagHub(t *testing.T, blobURL string) *diagHub {
	t.Helper()
	dir := t.TempDir()
	ca, err := InitCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{DataDir: dir, Listen: "127.0.0.1:0", RelayUDP: "off", HTTP: "off"}
	cfg.Diagnostics = DiagConfig{BlobURL: blobURL, MaxMB: 1, PerNodePerHour: 2}
	if err := cfg.applyDefaults(); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	h, err := Open(cfg, slog.New(slog.NewTextHandler(&syncWriter{w: logs}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; h.Close() })
	for h.Addr("grpc") == "" {
		time.Sleep(10 * time.Millisecond)
	}
	dial := func(o pki.IssueOptions) *grpc.ClientConn {
		certPEM, keyPEM, err := ca.Issue(o)
		if err != nil {
			t.Fatal(err)
		}
		bdir := filepath.Join(dir, "c-"+o.Name)
		if err := pki.WriteBundle(bdir, ca.CertPEM, certPEM, keyPEM); err != nil {
			t.Fatal(err)
		}
		b, err := pki.LoadBundle(bdir)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	return &diagHub{h: h, logs: logs,
		worker: dial(pki.IssueOptions{Role: pki.RoleWorker, Name: "w-secret-name"}),
		alice:  dial(pki.IssueOptions{Role: pki.RoleClient, Name: "alice-laptop", User: "alice"}),
		root:   dial(pki.IssueOptions{Role: pki.RoleAdmin, Name: "root"})}
}

var diagLogMu sync.Mutex

type syncWriter struct{ w *bytes.Buffer }

func (s *syncWriter) Write(p []byte) (int, error) {
	diagLogMu.Lock()
	defer diagLogMu.Unlock()
	return s.w.Write(p)
}

func zipFile(t *testing.T, size int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "b.zip")
	b := bytes.Repeat([]byte("0123456789abcdef"), size/16+1)[:size]
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiagUpload(t *testing.T) {
	blobDir := t.TempDir()
	d := newDiagHub(t, "file:///"+filepath.ToSlash(blobDir))
	ctx := tctx(t)
	p := zipFile(t, 300<<10)
	r, err := diag.Upload(ctx, d.worker, p, &pb.DiagMeta{Component: "longarmd", Node: "<node:forged>", Note: "worker w-secret-name breaks at 192.0.2.9"})
	if err != nil {
		t.Fatal(err)
	}
	if !diag.ReportIDRE.MatchString(r.ReportId) || r.Size != 300<<10 || !strings.HasSuffix(r.Blob, r.ReportId+".zip") || !strings.HasPrefix(r.Location, "file:///") {
		t.Fatalf("receipt %+v", r)
	}
	stored, err := os.ReadFile(filepath.Join(blobDir, filepath.FromSlash(r.Blob)))
	want, _ := os.ReadFile(p)
	if err != nil || !bytes.Equal(stored, want) {
		t.Fatalf("stored blob: %v", err)
	}
	diagLogMu.Lock()
	logs := d.logs.String()
	diagLogMu.Unlock()
	line := ""
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "diagnostic bundle uploaded") {
			line = l
		}
	}
	if !strings.Contains(line, r.ReportId) || !strings.Contains(line, "uploader=<node:") || strings.Contains(line, "w-secret-name") {
		t.Fatalf("hub log: %s", logs)
	}

	// a client may upload; the size limit holds
	if _, err := diag.Upload(ctx, d.alice, zipFile(t, 10<<10), &pb.DiagMeta{Component: "longarm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := diag.Upload(ctx, d.alice, zipFile(t, 2<<20), &pb.DiagMeta{Component: "longarm"}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("over max_mb: %v", err)
	}
	// rate: 2 per node and hour (the refused one above didn't count)
	if _, err := diag.Upload(ctx, d.alice, zipFile(t, 1<<10), &pb.DiagMeta{Component: "longarm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := diag.Upload(ctx, d.alice, zipFile(t, 1<<10), &pb.DiagMeta{Component: "longarm"}); status.Code(err) != codes.ResourceExhausted ||
		!strings.Contains(err.Error(), "per node") {
		t.Fatalf("rate: %v", err)
	}
	// a lying uploader: wrong hash -> nothing stored
	stream, err := pb.NewDiagnosticsClient(d.root).Upload(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stream.Send(&pb.DiagChunk{Meta: &pb.DiagMeta{Size: 4, Sha256: strings.Repeat("0", 64)}, Data: []byte("abcd")})
	if _, err := stream.CloseAndRecv(); status.Code(err) != codes.DataLoss {
		t.Fatalf("bad hash: %v", err)
	}

	// admins list and download; others can't
	for _, c := range []*grpc.ClientConn{d.worker, d.alice} {
		if _, err := pb.NewDiagnosticsClient(c).List(ctx, &pb.DiagListRequest{}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("non-admin list: %v", err)
		}
		var buf bytes.Buffer
		if _, err := diag.Download(ctx, c, r.ReportId, &buf); status.Code(err) != codes.PermissionDenied || buf.Len() > 0 {
			t.Fatalf("non-admin download: %v", err)
		}
	}
	l, err := pb.NewDiagnosticsClient(d.root).List(ctx, &pb.DiagListRequest{})
	if err != nil || len(l.Reports) != 3 {
		t.Fatalf("list %v %v", l, err)
	}
	var found *pb.DiagMeta
	for _, m := range l.Reports {
		if m.ReportId == r.ReportId {
			found = m
		}
	}
	if found == nil || found.Component != "longarmd" || found.UploaderRole != "worker" || !strings.HasPrefix(found.Node, "<node:") ||
		found.Node == "<node:forged>" || found.Size != 300<<10 || found.Created == nil {
		t.Fatalf("listed %+v", found)
	}
	if strings.Contains(found.Note, "w-secret-name") || strings.Contains(found.Note, "192.0.2.9") || !strings.Contains(found.Note, "breaks at <ip>") {
		t.Fatalf("note not scrubbed: %q", found.Note)
	}
	var buf bytes.Buffer
	m, err := diag.Download(ctx, d.root, strings.ToLower(r.ReportId), &buf)
	if err != nil || !bytes.Equal(buf.Bytes(), want) || m.ReportId != r.ReportId {
		t.Fatalf("download: %v", err)
	}
	sum := sha256.Sum256(want)
	if m.Sha256 != hex.EncodeToString(sum[:]) {
		t.Fatal("sha256 in meta")
	}
	if _, err := diag.Download(ctx, d.root, "LAR-20200101-AAAAAA", io.Discard); status.Code(err) != codes.NotFound {
		t.Fatalf("missing report: %v", err)
	}
	if _, err := diag.Download(ctx, d.root, "../../etc/passwd", io.Discard); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
	// old reports are outside --since
	if l, err := pb.NewDiagnosticsClient(d.root).List(ctx, &pb.DiagListRequest{Since: nil, Limit: 1}); err != nil || len(l.Reports) != 1 {
		t.Fatalf("limit: %v %v", l, err)
	}
}

func TestDiagNotConfigured(t *testing.T) {
	d := newDiagHub(t, "")
	ctx := tctx(t)
	_, err := diag.Upload(ctx, d.alice, zipFile(t, 100), &pb.DiagMeta{Component: "longarm"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "doesn't collect diagnostics") {
		t.Fatalf("upload without a store: %v", err)
	}
	if _, err := pb.NewDiagnosticsClient(d.root).List(ctx, &pb.DiagListRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("list without a store: %v", err)
	}
}

func TestDiagConfig(t *testing.T) {
	t.Setenv(EnvDiagBlobURL, "https://acct.blob.core.windows.net/diagnostics")
	c, err := LoadConfig("")
	if err != nil || c.Diagnostics.BlobURL != "https://acct.blob.core.windows.net/diagnostics" || c.Diagnostics.MaxMB != 50 || c.Diagnostics.PerNodePerHour != 10 {
		t.Fatalf("%+v %v", c.Diagnostics, err)
	}
	t.Setenv(EnvDiagBlobURL, "https://acct.blob.core.windows.net/diagnostics?sv=2022&sig=x")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("SAS URL accepted")
	}
}
