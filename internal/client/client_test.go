package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, WorkersFile), []byte(`
[tailscale]
hostname = "longarm-client-me"

[[worker]]
name = "w1"
address = "100.64.0.11:7701"

[[worker]]
name = "w2"
address = "longarm-w2"
transport = "tailscale"
cert_name = "worker-2"
`), 0o644)
	c, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	w1, err := c.Worker("w1")
	if err != nil || w1.Transport != "tcp" || w1.CertName != "w1" {
		t.Fatalf("w1 = %+v %v", w1, err)
	}
	w2, _ := c.Worker("w2")
	if w2.Transport != "tailscale" || w2.CertName != "worker-2" || c.Tailscale.Hostname != "longarm-client-me" {
		t.Fatalf("w2 = %+v", w2)
	}
	adhoc, err := c.Worker("box@127.0.0.1:7701")
	if err != nil || adhoc.Address != "127.0.0.1:7701" || adhoc.CertName != "box" {
		t.Fatalf("adhoc = %+v %v", adhoc, err)
	}
	if _, err := c.Worker("nope"); err == nil {
		t.Fatal("unknown worker accepted")
	}
	empty, err := LoadConfig(t.TempDir())
	if err != nil || len(empty.Workers) != 0 {
		t.Fatalf("missing file: %v", err)
	}
	os.WriteFile(filepath.Join(dir, WorkersFile), []byte("[[worker]]\nname=\"a\"\naddress=\"x:1\"\n[[worker]]\nname=\"a\"\naddress=\"y:1\"\n"), 0o644)
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestRetry(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := Retry(ctx, time.Minute, nil, func(ctx context.Context, progress func()) error {
		calls++
		if calls < 3 {
			return status.Error(codes.Unavailable, "down")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	perm := status.Error(codes.NotFound, "nope")
	if err := Retry(ctx, time.Minute, nil, func(context.Context, func()) error { return perm }); !errors.Is(err, perm) {
		t.Fatalf("permanent error not returned: %v", err)
	}
	start := time.Now()
	err = Retry(ctx, 300*time.Millisecond, nil, func(context.Context, func()) error { return status.Error(codes.Unavailable, "down") })
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("give-up: %v after %v", err, time.Since(start))
	}
}
