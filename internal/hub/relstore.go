package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/azauth"
	"github.com/lubobill1990/longarm/internal/diag"
	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/update"
)

// Release file storage.
//
// The database has the release index (store.Release: names, sha256, the
// signatures); the files themselves are in <data_dir>/releases/<V>/<file>
// (+ <file>.minisig). With [releases] blob_url they are also in a blob
// container as <V>/<file> (+ <V>/<file>.minisig), and the data dir is only a
// cache: a hub whose data dir is the container's own disk (deploy/azure,
// storageAuth identity) loses it on every restart, the blobs stay. Reads
// take the local copy, else download the blob into the cache (checked
// against the sha256 in the database) and serve that.

// EnvReleasesBlobURL overrides [releases] blob_url.
const EnvReleasesBlobURL = "LONGARM_HUB_RELEASES_BLOB_URL"

func (c *Config) releasesEnvDefaults() error {
	if v := strings.TrimSpace(os.Getenv(EnvReleasesBlobURL)); v != "" {
		c.Releases.BlobURL = v
	}
	if c.Releases.BlobURL != "" {
		if _, err := diag.OpenStore(c.Releases.BlobURL, nil); err != nil {
			return fmt.Errorf("[releases] %w", err)
		}
	}
	return nil
}

// errReleaseMissing: no copy of a release file anywhere.
var errReleaseMissing = errors.New("release file missing")

// errFetchBackoff: a recent download of the file from the blob container
// failed; the hub doesn't try again before the backoff ends.
var errFetchBackoff = errors.New("release file download failed recently; retrying later")

// Limits of blob -> cache downloads (anonymous requests trigger them).
const (
	// fetchLimit is how many downloads run at once (all files).
	fetchLimit = 2
	// fetchTimeout bounds one download. It goes on when the request that
	// started it goes away (others may wait for the same file, and the
	// next request finds the file cached), but not longer than this.
	fetchTimeout = 10 * time.Minute
	// fetchBackoffMin / Max: after a failed download (missing blob, wrong
	// size or sha256, storage error) requests for the file fail fast for
	// this long, doubling per failure; a publish of the file clears it.
	fetchBackoffMin = time.Minute
	fetchBackoffMax = 30 * time.Minute
	// fetchFailMax bounds the failure cache (entries).
	fetchFailMax = 1024
)

// releaseStore keeps release files in the data dir and, with blob_url, in
// a blob container.
type releaseStore struct {
	dir  string     // <data_dir>/releases
	blob diag.Store // nil = the data dir only
	log  *slog.Logger

	mu sync.Mutex
	// inBlob: the blobs the container has (List at start, writes since);
	// nil until the first List succeeded (then nothing is called missing)
	inBlob map[string]bool
	// fetches in flight, by blob name
	fetching map[string]*relFetch
	// failed downloads, by blob name (at most fetchFailMax)
	failed map[string]*fetchFail
	// fetchSem limits concurrent downloads
	fetchSem chan struct{}
	// versionMu serializes the writes of a version (publish, release add)
	versionMu map[string]*sync.Mutex
	now       func() time.Time // time.Now (tests)
}

type relFetch struct {
	done chan struct{}
	err  error
}

type fetchFail struct {
	until   time.Time
	backoff time.Duration
	err     error
}

// openReleaseStore opens the release store of cfg (ts: tokens for the blob
// container; nil = the managed identity).
func openReleaseStore(cfg *Config, ts azauth.TokenSource, log *slog.Logger) (*releaseStore, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &releaseStore{dir: filepath.Join(cfg.DataDir, ReleasesDir), log: log, fetching: map[string]*relFetch{},
		failed: map[string]*fetchFail{}, fetchSem: make(chan struct{}, fetchLimit), versionMu: map[string]*sync.Mutex{}, now: time.Now}
	if u := cfg.Releases.BlobURL; u != "" {
		b, err := diag.OpenStore(u, ts)
		if err != nil {
			return nil, fmt.Errorf("[releases] %w", err)
		}
		if bs, ok := b.(*diag.BlobStore); ok {
			bs.ContentType = "application/octet-stream"
		}
		s.blob = b
	}
	return s, nil
}

// lockVersion serializes the writes of version (two publishes of the same
// version must not interleave their files); call the result to unlock.
func (s *releaseStore) lockVersion(version string) func() {
	s.mu.Lock()
	m := s.versionMu[version]
	if m == nil {
		m = &sync.Mutex{}
		s.versionMu[version] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// location describes where the files are (logs).
func (s *releaseStore) location() string {
	if s.blob == nil {
		return s.dir
	}
	return s.blob.Location() + " (cache " + s.dir + ")"
}

// blobName is the blob of version's file name (name may end in .minisig).
func blobName(version, name string) string { return version + "/" + name }

func (s *releaseStore) localPath(version, name string) string {
	return filepath.Join(s.dir, version, name)
}

func regularFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// errTooLarge: more bytes than expected.
var errTooLarge = errors.New("larger than expected")

// installFile writes r to dst through a temp file of its own in dst's
// directory: at most size+1 bytes are read (the extra one fails the
// write), and only exactly size bytes with sha256 want are renamed into
// place, so readers of dst (which trust it) never see partial or foreign
// bytes, also with concurrent writers.
func installFile(dst string, r io.Reader, size int64, want string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".part-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, size+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		return err
	case n > size:
		return fmt.Errorf("%s: %w (want %d bytes)", filepath.Base(dst), errTooLarge, size)
	case n != size:
		return fmt.Errorf("%s: %d bytes, want %d", filepath.Base(dst), n, size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != want {
		return fmt.Errorf("%s: sha256 %s, want %s", filepath.Base(dst), sum, want)
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return platform.ReplaceFile(tmp.Name(), dst)
}

// put stores the file src (r: its index row; size and sha256 are checked
// again while copying) with its signature (nil = none): in the blob
// container first, then in the data dir. An error leaves the database
// alone (the caller records the file after put). Callers hold
// lockVersion(r.Version).
func (s *releaseStore) put(ctx context.Context, r store.Release, src string, sig []byte) error {
	version, name := r.Version, r.File
	if s.blob != nil {
		if err := s.putBlob(ctx, version, name, src); err != nil {
			return err
		}
		if sig != nil {
			if err := s.blob.Put(ctx, blobName(version, name+update.SigExt), bytes.NewReader(sig), int64(len(sig)), nil); err != nil {
				return fmt.Errorf("%s: %w", name+update.SigExt, err)
			}
			s.markBlob(blobName(version, name+update.SigExt))
		}
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := installFile(s.localPath(version, name), f, r.Size, r.SHA256, 0o755); err != nil {
		return err
	}
	if sig != nil {
		return installFile(s.localPath(version, name+update.SigExt), bytes.NewReader(sig), int64(len(sig)), sha256Hex(sig), 0o644)
	}
	return nil
}

func (s *releaseStore) putBlob(ctx context.Context, version, name, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := s.blob.Put(ctx, blobName(version, name), f, fi.Size(), nil); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	s.markBlob(blobName(version, name))
	return nil
}

// markBlob records a blob the hub wrote (and forgets failed downloads of
// it).
func (s *releaseStore) markBlob(name string) {
	s.mu.Lock()
	if s.inBlob != nil {
		s.inBlob[name] = true
	}
	delete(s.failed, name)
	s.mu.Unlock()
}

// has reports whether r's file can be served: a local copy, or a blob (as
// far as the hub knows; before the first listing it assumes so).
func (s *releaseStore) has(r store.Release) bool {
	if regularFile(s.localPath(r.Version, r.File)) {
		return true
	}
	if s.blob == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inBlob == nil || s.inBlob[blobName(r.Version, r.File)]
}

// file returns a local path with r's file (sig: its signature), downloading
// it from the blob container into the cache when the data dir lost it.
// errReleaseMissing: no copy anywhere; errFetchBackoff: a download failed
// recently.
func (s *releaseStore) file(ctx context.Context, r store.Release, sig bool) (string, error) {
	name := r.File
	if sig {
		name += update.SigExt
	}
	p := s.localPath(r.Version, name)
	if regularFile(p) {
		return p, nil
	}
	if sig {
		// the database has the signature
		if r.Sig == nil {
			return "", errReleaseMissing
		}
		return p, installFile(p, bytes.NewReader(r.Sig), int64(len(r.Sig)), sha256Hex(r.Sig), 0o644)
	}
	if s.blob == nil {
		return "", errReleaseMissing
	}
	bn := blobName(r.Version, r.File)
	s.mu.Lock()
	if fl := s.failed[bn]; fl != nil && s.now().Before(fl.until) {
		s.mu.Unlock()
		if errors.Is(fl.err, errReleaseMissing) {
			return "", errReleaseMissing
		}
		return "", errFetchBackoff
	}
	f := s.fetching[bn]
	if f == nil {
		f = &relFetch{done: make(chan struct{})}
		s.fetching[bn] = f
		go func() {
			// Not the request's context: a client that goes away mustn't
			// fail the others waiting for the same file. fetchTimeout
			// bounds it, fetchSem the downloads at once.
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
			f.err = s.fetchLimited(fctx, r, p)
			cancel()
			s.mu.Lock()
			delete(s.fetching, bn)
			if f.err != nil {
				s.failLocked(bn, f.err)
			}
			s.mu.Unlock()
			close(f.done)
		}()
	}
	s.mu.Unlock()
	select {
	case <-f.done:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if f.err != nil {
		return "", f.err
	}
	return p, nil
}

// failLocked records a failed download of bn: requests fail fast for a
// backoff that doubles per failure (fetchBackoffMin..fetchBackoffMax). The
// cache holds at most fetchFailMax entries.
func (s *releaseStore) failLocked(bn string, err error) {
	now := s.now()
	fl := s.failed[bn]
	if fl == nil {
		if len(s.failed) >= fetchFailMax {
			// drop expired entries, else the one that ends first
			first := ""
			for k, v := range s.failed {
				if !now.Before(v.until) {
					delete(s.failed, k)
				} else if first == "" || v.until.Before(s.failed[first].until) {
					first = k
				}
			}
			if len(s.failed) >= fetchFailMax {
				delete(s.failed, first)
			}
		}
		fl = &fetchFail{}
		s.failed[bn] = fl
	}
	fl.backoff = min(max(2*fl.backoff, fetchBackoffMin), fetchBackoffMax)
	fl.until, fl.err = now.Add(fl.backoff), err
}

func (s *releaseStore) fetchLimited(ctx context.Context, r store.Release, p string) error {
	select {
	case s.fetchSem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-s.fetchSem }()
	return s.fetch(ctx, r, p)
}

// fetch downloads r's blob to p: its advertised size must be the recorded
// one, at most that many bytes (+1) are read, and only content with the
// recorded sha256 reaches p.
func (s *releaseStore) fetch(ctx context.Context, r store.Release, p string) error {
	bn := blobName(r.Version, r.File)
	rc, bi, err := s.blob.Get(ctx, bn)
	if errors.Is(err, diag.ErrNotFound) {
		s.mu.Lock()
		if s.inBlob != nil {
			delete(s.inBlob, bn)
		}
		s.mu.Unlock()
		s.log.Error("release file missing: not in the data dir, not in the blob container; publish the version again",
			"version", r.Version, "file", r.File, "location", s.blob.Location())
		return errReleaseMissing
	}
	if err != nil {
		return err
	}
	defer rc.Close()
	if bi.Size >= 0 && bi.Size != r.Size {
		s.log.Error("release blob doesn't match the database; publish the version again (--replace)",
			"version", r.Version, "file", r.File, "size", bi.Size, "want_size", r.Size)
		return fmt.Errorf("%s: the blob has %d bytes, want %d", bn, bi.Size, r.Size)
	}
	if err := installFile(p, rc, r.Size, r.SHA256, 0o755); err != nil {
		s.log.Error("release blob doesn't match the database; publish the version again (--replace)",
			"version", r.Version, "file", r.File, "err", err)
		return fmt.Errorf("download %s: %w", bn, err)
	}
	s.log.Info("release file restored from the blob container", "version", r.Version, "file", r.File)
	return nil
}

// reconcile compares the database's release files with the copies: with a
// blob container it lists it (retrying until ctx ends), uploads files only
// the data dir has, and logs every file that is nowhere (install scripts
// then skip its version). Run at start.
func (s *releaseStore) reconcile(ctx context.Context, st *store.Store) {
	rels, err := st.Releases()
	if err != nil {
		s.log.Warn("releases: reading the index", "err", err)
		return
	}
	if s.blob != nil {
		for wait := 5 * time.Second; ; wait = min(2*wait, 5*time.Minute) {
			blobs, err := s.blob.List(ctx, "")
			if err == nil {
				seen := map[string]bool{}
				for _, b := range blobs {
					seen[b.Name] = true
				}
				s.mu.Lock()
				s.inBlob = seen
				s.mu.Unlock()
				break
			}
			s.log.Warn("releases: listing the blob container (retrying)", "location", s.blob.Location(), "err", err, "retry_in", wait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
	missing := map[string][]string{}
	for _, r := range rels {
		if !versionRE.MatchString(r.Version) || !servableFile(r.File) {
			continue
		}
		local := regularFile(s.localPath(r.Version, r.File))
		if s.blob != nil {
			s.mu.Lock()
			inBlob := s.inBlob[blobName(r.Version, r.File)]
			sigInBlob := s.inBlob[blobName(r.Version, r.File+update.SigExt)]
			s.mu.Unlock()
			if local && !inBlob {
				// the data dir has it, the container not (published
				// before blob_url was set, or the upload failed): copy it
				if err := s.putBlob(ctx, r.Version, r.File, s.localPath(r.Version, r.File)); err != nil {
					s.log.Warn("releases: uploading a local file to the blob container", "version", r.Version, "file", r.File, "err", err)
				} else {
					inBlob = true
					s.log.Info("releases: uploaded a local file to the blob container", "version", r.Version, "file", r.File)
				}
			}
			if inBlob && !sigInBlob && r.Sig != nil {
				bn := blobName(r.Version, r.File+update.SigExt)
				if err := s.blob.Put(ctx, bn, bytes.NewReader(r.Sig), int64(len(r.Sig)), nil); err == nil {
					s.markBlob(bn)
				}
			}
			if inBlob {
				continue
			}
		}
		if !local {
			missing[r.Version] = append(missing[r.Version], r.File)
		}
	}
	versions := make([]string, 0, len(missing))
	for v := range missing {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	for _, v := range versions {
		files := missing[v]
		sort.Strings(files)
		s.log.Error("RELEASE FILES MISSING: the database lists them, but neither the data dir nor the blob container has them; install scripts and rollouts skip this version until it is published again (longarm admin release publish with the same files, or a new version)",
			"version", v, "files", files, "location", s.location())
	}
	s.log.Info("releases: store checked", "location", s.location(), "files", len(rels), "incomplete_versions", len(versions))
}

// missingFiles returns the files of version (rels: its index rows) that
// can't be served.
func (s *releaseStore) missingFiles(rels []store.Release) []string {
	var out []string
	for _, r := range rels {
		if !s.has(r) {
			out = append(out, r.File)
		}
	}
	return out
}
