package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/platform"
)

func testRelease(version, file, content string) store.Release {
	return store.Release{Version: version, File: file, Channel: "stable", Kind: "package", SHA256: sha256Hex([]byte(content)), Size: int64(len(content))}
}

// TestReleaseRestoreConcurrent: identical publishes of a version running
// at once (restores) while readers serve the cached file: every read sees
// the whole file, and no temp file is left behind (each writer has its
// own, checked before the rename; publishes of a version are serialized).
func TestReleaseRestoreConcurrent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	data := t.TempDir()
	rs, _ := testReleaseStore(t, data, newMemBlob())
	big := strings.Repeat("0123456789abcdef", 1<<15) // 512 KiB
	files := map[string]string{"longarm-windows-amd64.msi": big, "longarmd-linux-amd64": "d", "longarmd-linux-amd64.minisig": "sig"}
	if _, err := addRelease(ctx, rs, st, releaseDir(t, files), "1.0.0", "", "", false); err != nil {
		t.Fatal(err)
	}
	dir := releaseDir(t, files)
	p := rs.localPath("1.0.0", "longarm-windows-amd64.msi")
	stop := make(chan struct{})
	var bad []string
	var mu sync.Mutex
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b, err := platform.ReadFileShared(p) // as the hub serves it
				if err == nil && string(b) != big {
					mu.Lock()
					bad = append(bad, fmt.Sprintf("%d bytes", len(b)))
					mu.Unlock()
				}
			}
		}()
	}
	var writers sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			_, err := addRelease(ctx, rs, st, dir, "1.0.0", "", "", true)
			errs <- err
		}()
	}
	writers.Wait()
	close(stop)
	readers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("restore: %v", err)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("readers saw partial files: %v", bad[:min(len(bad), 5)])
	}
	ents, _ := os.ReadDir(filepath.Join(data, ReleasesDir, "1.0.0"))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

// TestInstallFile: only exactly the expected bytes replace dst.
func TestInstallFile(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "v", "f")
	good := "the right content"
	if err := installFile(dst, strings.NewReader(good), int64(len(good)), sha256Hex([]byte(good)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, content string }{
		{"short", good[:5]},
		{"long", good + "and more"},
		{"other", strings.ToUpper(good)},
	} {
		if err := installFile(dst, strings.NewReader(c.content), int64(len(good)), sha256Hex([]byte(good)), 0o644); err == nil {
			t.Fatalf("%s content installed", c.name)
		}
		if b, _ := os.ReadFile(dst); string(b) != good {
			t.Fatalf("%s: dst became %q", c.name, b)
		}
	}
	ents, _ := os.ReadDir(filepath.Dir(dst))
	if len(ents) != 1 {
		t.Fatalf("leftovers: %v", ents)
	}
}

// TestReleaseFetchBackoff: a failed download (missing or corrupt blob) is
// not retried by every request: requests fail fast for a backoff that
// grows per failure, and a publish of the file clears it.
func TestReleaseFetchBackoff(t *testing.T) {
	ctx := context.Background()
	blob := newMemBlob()
	rs, _ := testReleaseStore(t, t.TempDir(), blob)
	now := time.Now()
	rs.now = func() time.Time { return now }
	r := testRelease("1.0.0", "longarm-linux-amd64.deb", "deb")

	// missing: one Get, then 404s from the cache
	for i := 0; i < 5; i++ {
		if _, err := rs.file(ctx, r, false); !errors.Is(err, errReleaseMissing) {
			t.Fatalf("missing: %v", err)
		}
	}
	if g := blob.getCount(); g != 1 {
		t.Fatalf("%d downloads of a missing blob", g)
	}
	now = now.Add(fetchBackoffMin + time.Second)
	rs.file(ctx, r, false)
	if g := blob.getCount(); g != 2 {
		t.Fatalf("after the backoff: %d downloads", g)
	}
	// the second failure doubles it
	now = now.Add(fetchBackoffMin + time.Second)
	rs.file(ctx, r, false)
	if g := blob.getCount(); g != 2 {
		t.Fatalf("second backoff not longer: %d downloads", g)
	}

	// corrupt: same size, other bytes - downloaded and hashed once
	c := testRelease("1.0.0", "longarm-linux-amd64.rpm", "rpm")
	blob.blobs["1.0.0/longarm-linux-amd64.rpm"] = []byte("RPM")
	gets := blob.getCount()
	for i := 0; i < 5; i++ {
		if _, err := rs.file(ctx, c, false); err == nil {
			t.Fatal("corrupt blob served")
		} else if i > 0 && !errors.Is(err, errFetchBackoff) {
			t.Fatalf("corrupt, again: %v", err)
		}
	}
	if g := blob.getCount() - gets; g != 1 {
		t.Fatalf("%d downloads of a corrupt blob", g)
	}

	// a publish of the file clears the backoff
	src := filepath.Join(t.TempDir(), "longarm-linux-amd64.deb")
	os.WriteFile(src, []byte("deb"), 0o644)
	unlock := rs.lockVersion("1.0.0")
	err := rs.put(ctx, r, src, nil)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(rs.localPath("1.0.0", r.File)) // served from the blob, not the cache
	if p, err := rs.file(ctx, r, false); err != nil || readAll(t, p) != "deb" {
		t.Fatalf("after a publish: %v", err)
	}

	// the cache is bounded
	for i := 0; i < fetchFailMax+10; i++ {
		rs.mu.Lock()
		rs.failLocked(fmt.Sprintf("9.9.%d/x", i), errReleaseMissing)
		rs.mu.Unlock()
	}
	if n := len(rs.failed); n > fetchFailMax {
		t.Fatalf("failure cache has %d entries", n)
	}
}

// TestReleaseFetchLimits: at most fetchLimit downloads at once; a
// requester that goes away doesn't stop a download others wait for, and
// the file lands in the cache.
func TestReleaseFetchLimits(t *testing.T) {
	ctx := context.Background()
	blob := newMemBlob()
	blob.hold = make(chan struct{})
	rs, _ := testReleaseStore(t, t.TempDir(), blob)
	var rels []store.Release
	for i := 0; i < 6; i++ {
		name := fmt.Sprintf("longarm-linux-amd64.%s", []string{"deb", "rpm", "tar.gz", "zip", "pkg", "msi"}[i])
		r := testRelease("1.0.0", name, "content "+name)
		blob.blobs["1.0.0/"+name] = []byte("content " + name)
		rels = append(rels, r)
	}
	// the first requester gives up; another one waits for the same file
	gone, cancel := context.WithCancel(ctx)
	first := make(chan error, 1)
	go func() { _, err := rs.file(gone, rels[0], false); first <- err }()
	var wg sync.WaitGroup
	errs := make(chan error, len(rels))
	for _, r := range rels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := rs.file(ctx, r, false)
			errs <- err
		}()
	}
	time.Sleep(200 * time.Millisecond)
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("gone requester: %v", err)
	}
	close(blob.hold)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if blob.maxInflight > fetchLimit {
		t.Fatalf("%d downloads at once", blob.maxInflight)
	}
	if g := blob.getCount(); g != len(rels) {
		t.Fatalf("%d downloads for %d files", g, len(rels))
	}
	if !regularFile(rs.localPath("1.0.0", rels[0].File)) {
		t.Fatal("the abandoned download didn't reach the cache")
	}
}

// TestReleaseFetchSize: a blob whose advertised size isn't the recorded
// one is refused before its body is read; one without a size is read up
// to the recorded size + 1 byte, and nothing reaches the cache.
func TestReleaseFetchSize(t *testing.T) {
	ctx := context.Background()
	blob := newMemBlob()
	data := t.TempDir()
	rs, _ := testReleaseStore(t, data, blob)
	r := testRelease("1.0.0", "longarm-windows-amd64.msi", "msi")
	huge := bytes.Repeat([]byte("x"), 1<<20)
	blob.blobs["1.0.0/longarm-windows-amd64.msi"] = huge
	if _, err := rs.file(ctx, r, false); err == nil || !strings.Contains(err.Error(), "bytes, want 3") {
		t.Fatalf("advertised size: %v", err)
	}
	if blob.read != 0 {
		t.Fatalf("read %d bytes of a blob with the wrong size", blob.read)
	}
	rs.failed = map[string]*fetchFail{}
	blob.advertise = map[string]int64{"1.0.0/longarm-windows-amd64.msi": -1}
	if _, err := rs.file(ctx, r, false); !errors.Is(err, errTooLarge) {
		t.Fatalf("unknown size: %v", err)
	}
	if blob.read > r.Size+1 {
		t.Fatalf("read %d bytes, want at most %d", blob.read, r.Size+1)
	}
	if ents, _ := os.ReadDir(filepath.Join(data, ReleasesDir, "1.0.0")); len(ents) != 0 {
		t.Fatalf("cache: %v", ents)
	}
}
