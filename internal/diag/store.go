package diag

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/azauth"
)

// ---------------------------------------------------------------- report ids

// reportAlphabet: digits and upper case letters without I L O U (like join
// codes: nothing to confuse when read out or typed).
const reportAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ReportIDRE matches a report id: LAR-<yyyymmdd>-<6 characters>.
var ReportIDRE = regexp.MustCompile(`^LAR-(\d{8})-([0-9A-HJKMNP-TV-Z]{6})$`)

// NewReportID returns a fresh report id for a bundle received at t
// ("LAR-20261027-7F3K9Q"; the date is UTC).
func NewReportID(t time.Time) string {
	var b [6]byte
	rand.Read(b[:])
	for i := range b {
		b[i] = reportAlphabet[int(b[i])%len(reportAlphabet)]
	}
	return "LAR-" + t.UTC().Format("20060102") + "-" + string(b[:])
}

// BlobPath is where a report is stored in the container:
// <yyyy>/<mm>/<dd>/<report-id>.zip.
func BlobPath(id string) (string, error) {
	id = strings.ToUpper(strings.TrimSpace(id))
	m := ReportIDRE.FindStringSubmatch(id)
	if m == nil {
		return "", fmt.Errorf("%q is not a report id (LAR-yyyymmdd-XXXXXX)", id)
	}
	d := m[1]
	return d[:4] + "/" + d[4:6] + "/" + d[6:] + "/" + id + ".zip", nil
}

// DayPrefix is the blob prefix of a day ("2026/10/27/").
func DayPrefix(t time.Time) string { return t.UTC().Format("2006/01/02") + "/" }

// ---------------------------------------------------------------- stores

// ErrNotFound: no such blob.
var ErrNotFound = errors.New("not found")

// BlobInfo describes a stored bundle.
type BlobInfo struct {
	Name     string
	Size     int64
	Modified time.Time
	Meta     map[string]string
}

// Store keeps uploaded bundles (the hub's [diagnostics] blob_url).
type Store interface {
	// Put stores size bytes from r as name, with metadata (lower-case
	// ASCII keys and values). It fails without leaving a partial blob.
	Put(ctx context.Context, name string, r io.Reader, size int64, meta map[string]string) error
	// Get opens a blob.
	Get(ctx context.Context, name string) (io.ReadCloser, BlobInfo, error)
	// List returns the blobs whose names start with prefix.
	List(ctx context.Context, prefix string) ([]BlobInfo, error)
	// Location is the container's URL (shown to uploaders; no secrets).
	Location() string
}

// OpenStore opens the store at rawURL: https://<account>.blob.core.windows.net/<container>
// (Azure Blob Storage with tokens from ts; http:// only for emulators on
// the loopback) or file:///<dir> (a local directory: tests, self-hosted
// hubs).
func OpenStore(rawURL string, ts azauth.TokenSource) (Store, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("diagnostics blob_url: %w", err)
	}
	switch u.Scheme {
	case "file":
		p := u.Path
		if u.Host != "" && u.Host != "localhost" {
			p = "//" + u.Host + p // UNC
		}
		if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
			p = p[1:] // file:///C:/x
		}
		if p == "" {
			return nil, errors.New("diagnostics blob_url: file:// needs a directory")
		}
		return &DirStore{Dir: filepath.FromSlash(p)}, nil
	case "https", "http":
		if u.RawQuery != "" || u.User != nil {
			return nil, errors.New("diagnostics blob_url: no SAS token or credentials in the URL; the hub authenticates with its managed identity")
		}
		if u.Scheme == "http" && !loopbackHost(u.Hostname()) {
			return nil, errors.New("diagnostics blob_url: http:// only to the loopback (a storage emulator); bearer tokens need https")
		}
		// emulators (http, loopback) have the account in the path
		c := strings.Trim(u.Path, "/")
		if c == "" || strings.Contains(c, "/") && u.Scheme == "https" {
			return nil, fmt.Errorf("diagnostics blob_url %q: want https://<account>.blob.core.windows.net/<container>", rawURL)
		}
		if ts == nil {
			ts = &azauth.ManagedIdentity{}
		}
		return &BlobStore{Container: strings.TrimRight(u.String(), "/"), Tokens: ts}, nil
	}
	return nil, fmt.Errorf("diagnostics blob_url %q: want https://<account>.blob.core.windows.net/<container> or file:///<dir>", rawURL)
}

func loopbackHost(h string) bool {
	return h == "localhost" || strings.HasPrefix(h, "127.") || h == "::1"
}

// DirStore keeps bundles in a local directory, with <name>.meta.json next
// to each.
type DirStore struct{ Dir string }

const metaSuffix = ".meta.json"

func (d *DirStore) path(name string) (string, error) {
	clean := path.Clean("/" + name)[1:]
	if clean == "" || clean != name || strings.Contains(name, "..") {
		return "", fmt.Errorf("bad blob name %q", name)
	}
	return filepath.Join(d.Dir, filepath.FromSlash(name)), nil
}

// Location implements Store.
func (d *DirStore) Location() string {
	return "file:///" + strings.TrimPrefix(filepath.ToSlash(d.Dir), "/")
}

// Put implements Store.
func (d *DirStore) Put(_ context.Context, name string, r io.Reader, size int64, meta map[string]string) error {
	p, err := d.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, r)
	if err == nil && n != size {
		err = fmt.Errorf("got %d bytes, want %d", n, size)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(p+metaSuffix, mb, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// Get implements Store.
func (d *DirStore) Get(_ context.Context, name string) (io.ReadCloser, BlobInfo, error) {
	p, err := d.path(name)
	if err != nil {
		return nil, BlobInfo{}, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, BlobInfo{}, ErrNotFound
	}
	if err != nil {
		return nil, BlobInfo{}, err
	}
	fi, _ := f.Stat()
	return f, BlobInfo{Name: name, Size: fi.Size(), Modified: fi.ModTime(), Meta: readMeta(p)}, nil
}

func readMeta(p string) map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(p + metaSuffix); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// List implements Store.
func (d *DirStore) List(_ context.Context, prefix string) ([]BlobInfo, error) {
	var out []BlobInfo
	// walk the deepest directory the prefix names
	start := d.Dir
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		start = filepath.Join(d.Dir, filepath.FromSlash(prefix[:i]))
	}
	err := filepath.WalkDir(start, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if e.IsDir() || strings.HasSuffix(p, metaSuffix) || strings.HasPrefix(e.Name(), ".") {
			return nil
		}
		rel, _ := filepath.Rel(d.Dir, p)
		name := filepath.ToSlash(rel)
		if !strings.HasPrefix(name, prefix) {
			return nil
		}
		fi, err := e.Info()
		if err != nil {
			return nil
		}
		out = append(out, BlobInfo{Name: name, Size: fi.Size(), Modified: fi.ModTime(), Meta: readMeta(p)})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}
