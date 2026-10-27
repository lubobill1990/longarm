package longarmd

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
)

// TestSetUpSignIn: the MSI's LOGIN=1 records the hub for a sign-in to
// come; longarm login finds it without input; longarmd waits for the
// certificate instead of failing, and stops waiting when it is there.
func TestSetUpSignIn(t *testing.T) {
	dir := t.TempDir()
	// no hub.toml: the MSI's hub if this machine has one, else an error
	if hub, _, err := PendingHub(dir, ""); err == nil && hub == "" {
		t.Fatal("PendingHub without a hub: no error")
	}
	if err := SetUpSignIn(dir, " longarm.example.com ", "AB:CD"); err != nil {
		t.Fatal(err)
	}
	hf, err := hubclient.LoadFile(dir)
	if err != nil || hf.Address != "longarm.example.com" || !hf.SignIn || hf.CAFingerprint != "abcd" {
		t.Fatalf("hub.toml: %+v %v", hf, err)
	}
	hub, fp, err := PendingHub(dir, "")
	if err != nil || hub != "longarm.example.com" || fp != "abcd" {
		t.Fatalf("pending hub: %q %q %v", hub, fp, err)
	}
	if _, fp, _ := PendingHub(dir, "ee"); fp != "ee" {
		t.Fatalf("explicit fingerprint lost: %q", fp)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	type res struct {
		from, to string
		err      error
	}
	done := make(chan res, 1)
	go func() { f, to, err := waitSignIn(ctx, dir, log); done <- res{f, to, err} }()
	// the certificate alone (hub.toml not written yet): still waiting
	os.MkdirAll(filepath.Join(dir, TLSDir), 0o700)
	os.WriteFile(filepath.Join(dir, TLSDir, pki.NodeCertFile), []byte("cert"), 0o600)
	select {
	case r := <-done:
		t.Fatalf("did not wait: %+v", r)
	case <-time.After(4 * time.Second):
	}
	// the enrollment's hub.toml (the resolved address, no sign_in)
	if err := hubclient.SaveFile(dir, hubclient.Config{Address: "longarm.example.com:7443", Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.from != "longarm.example.com" || r.to != "longarm.example.com:7443" {
			t.Fatalf("wait: %+v", r)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("still waiting when enrolled")
	}
	cancel()

	// enrolled: SetUpSignIn leaves it alone
	if err := SetUpSignIn(dir, "other.example.com", ""); err != nil {
		t.Fatal(err)
	}
	if hf, _ := hubclient.LoadFile(dir); hf.Address != "longarm.example.com:7443" {
		t.Fatalf("enrollment overwritten: %+v", hf)
	}
	// no sign-in to come: no wait
	plain := t.TempDir()
	if f, _, err := waitSignIn(context.Background(), plain, log); err != nil || f != "" {
		t.Fatal(f, err)
	}
	// canceled while waiting
	SetUpSignIn(plain, "longarm.example.com", "")
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if _, _, err := waitSignIn(ctx, plain, log); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait: %v", err)
	}
}
