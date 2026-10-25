package pki

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReloader(t *testing.T) {
	root := t.TempDir()
	ca := mustCA(t, filepath.Join(root, "ca"))
	wdir := filepath.Join(root, "w")
	issue(t, ca, wdir, RoleWorker, "w1")
	client := issue(t, ca, filepath.Join(root, "c"), RoleClient, "alice")
	r, err := NewReloader(wdir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	before := r.Bundle().NotAfter
	if _, err := handshake(t, r.TLSConfig(), client.ClientConfig("w1")); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.Check(); changed || err != nil {
		t.Fatalf("no change expected: %v %v", changed, err)
	}
	// Re-issue with a different validity.
	certPEM, keyPEM, err := ca.Issue(IssueOptions{Role: RoleWorker, Name: "w1", Validity: 48 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	caPEM, _ := os.ReadFile(filepath.Join(wdir, CACertFile))
	time.Sleep(20 * time.Millisecond) // distinct mtime
	if err := WriteBundle(wdir, caPEM, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.Check(); !changed || err != nil {
		t.Fatalf("reload: %v %v", changed, err)
	}
	if r.Bundle().NotAfter.Equal(before) {
		t.Fatal("bundle not replaced")
	}
	if _, err := handshake(t, r.TLSConfig(), client.ClientConfig("w1")); err != nil {
		t.Fatal(err)
	}
	// A broken bundle is ignored; the previous one keeps serving.
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(filepath.Join(wdir, NodeKeyFile), []byte("garbage"), 0o600)
	if changed, err := r.Check(); changed || err == nil {
		t.Fatalf("broken bundle: %v %v", changed, err)
	}
	if _, err := handshake(t, r.TLSConfig(), client.ClientConfig("w1")); err != nil {
		t.Fatal(err)
	}
	// A client certificate is not accepted as the worker's.
	cdir := filepath.Join(root, "c2")
	issue(t, ca, cdir, RoleClient, "bob")
	if _, err := NewReloader(cdir, nil); err == nil {
		t.Fatal("client bundle accepted for serving")
	}
}
