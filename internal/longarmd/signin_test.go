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
// come; longarmd login finds it without input; longarmd waits for the
// enrollment instead of failing, and stops waiting when it is installed.
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
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- waitSignIn(ctx, dir, log) }()
	// the certificate alone (hub.toml not rewritten yet): still waiting
	os.MkdirAll(filepath.Join(dir, TLSDir), 0o700)
	os.WriteFile(filepath.Join(dir, TLSDir, pki.NodeCertFile), []byte("cert"), 0o600)
	select {
	case err := <-done:
		t.Fatalf("did not wait: %v", err)
	case <-time.After(4 * time.Second):
	}
	// the enrollment's hub.toml (the resolved address, no sign_in)
	if err := hubclient.SaveFile(dir, hubclient.Config{Address: "longarm.example.com:7443", Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("still waiting when enrolled")
	}

	// enrolled: SetUpSignIn leaves it alone
	if err := SetUpSignIn(dir, "other.example.com", ""); err != nil {
		t.Fatal(err)
	}
	if hf, _ := hubclient.LoadFile(dir); hf.Address != "longarm.example.com:7443" {
		t.Fatalf("enrollment overwritten: %+v", hf)
	}
	// no sign-in to come: no wait
	plain := t.TempDir()
	if err := waitSignIn(context.Background(), plain, log); err != nil {
		t.Fatal(err)
	}
	// canceled while waiting
	SetUpSignIn(plain, "longarm.example.com", "")
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := waitSignIn(ctx2, plain, log); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait: %v", err)
	}
}

// TestReloadHubFile: the hub address a sign-in writes reaches the running
// config whenever it lands - also between LoadConfig and the wait (no wait
// at all then) - unless longarmd.toml names the hub itself.
func TestReloadHubFile(t *testing.T) {
	dir := t.TempDir()
	if err := SetUpSignIn(dir, "longarm.example.com", ""); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hub.Address != "longarm.example.com" {
		t.Fatalf("loaded: %q", cfg.Hub.Address)
	}
	// the sign-in finishes before longarmd waits: waitSignIn returns at once
	os.MkdirAll(filepath.Join(dir, TLSDir), 0o700)
	os.WriteFile(filepath.Join(dir, TLSDir, pki.NodeCertFile), []byte("cert"), 0o600)
	hubclient.SaveFile(dir, hubclient.Config{Address: "203.0.113.10:7443", Owner: "alice"})
	if err := waitSignIn(context.Background(), dir, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	if err := cfg.reloadHubFile(); err != nil {
		t.Fatal(err)
	}
	if cfg.Hub.Address != "203.0.113.10:7443" || cfg.HubPollToken != "" {
		t.Fatalf("reloaded: %q %q", cfg.Hub.Address, cfg.HubPollToken)
	}

	// longarmd.toml's [hub] address wins over hub.toml
	pinned := t.TempDir()
	os.WriteFile(filepath.Join(pinned, ConfigFile), []byte("[hub]\naddress = \"hub.lan:7443\"\n"), 0o600)
	SetUpSignIn(pinned, "longarm.example.com", "")
	cfg, err = LoadConfig(pinned, nil)
	if err != nil {
		t.Fatal(err)
	}
	hubclient.SaveFile(pinned, hubclient.Config{Address: "203.0.113.10:7443"})
	if err := cfg.reloadHubFile(); err != nil || cfg.Hub.Address != "hub.lan:7443" {
		t.Fatalf("longarmd.toml's address: %q %v", cfg.Hub.Address, err)
	}
	// a Config built in code keeps what it was given
	c := Config{DataDir: pinned, Hub: HubConfig{Address: "x:1"}, HubPollToken: "tok"}
	if err := c.reloadHubFile(); err != nil || c.Hub.Address != "x:1" || c.HubPollToken != "tok" {
		t.Fatalf("code config: %+v %v", c.Hub, err)
	}
}
