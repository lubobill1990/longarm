package diag

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/azauth"
	"github.com/lubobill1990/longarm/internal/pii"
)

func scrubber() *pii.Scrubber {
	return pii.New(pii.Options{Salt: []byte("t"), NoLocal: true, Names: map[string]string{"alice": pii.KindUser, "w-desk": pii.KindNode}})
}

func readZip(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(b)
	}
	return out
}

func TestSanitizeTOML(t *testing.T) {
	in := `listen = ["tcp://192.0.2.3:7701"]
[hub]
address = "hub.example.com:7443"
poll_token = "pt-123456"
[audit]
webhook = "https://hooks.example.com/services/T0/B0/XXXX"
webhook_token_file = "/etc/tok"
[tailnet.tailscale]
oauth_secret_file = "/etc/s"
api_key = ""
[[worker]]
name = "w1"
password = "hunter22"
[releases]
public_key = "RWQ..."
`
	out, n, err := SanitizeTOML([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, bad := range []string{"pt-123456", "hooks.example.com", "/etc/tok", "/etc/s", "hunter22"} {
		if strings.Contains(s, bad) {
			t.Errorf("%q left in\n%s", bad, s)
		}
	}
	for _, keep := range []string{"hub.example.com:7443", `public_key = "RWQ..."`, `name = "w1"`, "tcp://192.0.2.3:7701"} {
		if !strings.Contains(s, keep) {
			t.Errorf("%q missing\n%s", keep, s)
		}
	}
	if n != 5 {
		t.Errorf("removed %d values, want 5\n%s", n, s)
	}
	if _, _, err := SanitizeTOML([]byte("password = \"x\n")); err == nil {
		t.Fatal("broken TOML accepted")
	}
	// a broken file is left out entirely
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.toml"), []byte("password = 'hunter22\n"), 0o600)
	b := NewBuilder("t", scrubber())
	b.AddTOML(filepath.Join(dir, "x.toml"), "config/x.toml", "x")
	if len(b.Files()) != 0 || len(b.ExcludedFiles()) != 1 {
		t.Fatalf("broken TOML: %+v %+v", b.Files(), b.ExcludedFiles())
	}
}

func TestBundleManifest(t *testing.T) {
	b := NewBuilder("longarmd", scrubber())
	b.Since = time.Hour
	b.AddText("a.txt", "reason a", []byte("alice on w-desk from 192.0.2.7 token=abcdef123\n"))
	b.AddText("bin.dat", "binary", []byte{0, 1, 2, 0xff})
	b.AddJSON("x.json", "reason x", map[string]string{"mail": "bob@example.com"})
	b.AddText("a.txt", "dup", []byte("second"))
	var buf bytes.Buffer
	if err := b.WriteZip(&buf); err != nil {
		t.Fatal(err)
	}
	z := readZip(t, buf.Bytes())
	var m Manifest
	if err := json.Unmarshal([]byte(z[ManifestFile]), &m); err != nil {
		t.Fatal(err)
	}
	if m.Format != Format || m.Component != "longarmd" || m.Since != "1h0m0s" {
		t.Fatalf("manifest %+v", m)
	}
	if len(m.Files) != len(z)-1 {
		t.Fatalf("manifest lists %d files, zip has %d", len(m.Files), len(z)-1)
	}
	for _, f := range m.Files {
		got, ok := z[f.Name]
		if !ok || int64(len(got)) != f.Size || f.Reason == "" || !f.Scrubbed {
			t.Errorf("file %+v: in zip %v, size %d", f, ok, len(got))
		}
	}
	if _, ok := z["bin.dat"]; ok || len(m.Excluded) != 1 || m.Excluded[0].Name != "bin.dat" {
		t.Fatalf("binary file: %+v", m.Excluded)
	}
	if _, ok := z["a-2.txt"]; !ok {
		t.Fatal("duplicate name not renamed")
	}
	a := z["a.txt"]
	for _, bad := range []string{"alice", "w-desk", "192.0.2.7", "abcdef123"} {
		if strings.Contains(a, bad) || strings.Contains(z["x.json"], "bob@") {
			t.Fatalf("%q not scrubbed: %s", bad, a)
		}
	}
	want := map[string]int{"user": 1, "node": 1, "ip": 1, "secret": 1, "email": 1}
	for k, v := range want {
		if m.Scrub[k] != v {
			t.Errorf("scrub[%s] = %d, want %d (%v)", k, m.Scrub[k], v, m.Scrub)
		}
	}
	if strings.Contains(z[ManifestFile], "alice") {
		t.Fatal("manifest holds a name")
	}
	if !strings.Contains(b.Summary(), "a.txt") || !strings.Contains(b.Summary(), "bin.dat") {
		t.Fatal(b.Summary())
	}
}

func TestLogs(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	line := func(at time.Time, msg string) string {
		return fmt.Sprintf("time=%s level=INFO msg=%q\n", at.Format(time.RFC3339Nano), msg)
	}
	p := filepath.Join(dir, "longarmd.log")
	// current: an old line, then recent ones
	os.WriteFile(p, []byte(line(now.Add(-48*time.Hour), "ancient")+line(now.Add(-time.Minute), "recent")+"  continuation\n"), 0o600)
	// .1: within the window
	os.WriteFile(p+".1", []byte(line(now.Add(-2*time.Hour), "rotated-one")), 0o600)
	os.Chtimes(p+".1", now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	// .2.gz: compressed, within the window
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(line(now.Add(-3*time.Hour), "gzipped")))
	zw.Close()
	os.WriteFile(p+".2.gz", gz.Bytes(), 0o600)
	os.Chtimes(p+".2.gz", now.Add(-3*time.Hour), now.Add(-3*time.Hour))
	// .3: too old
	os.WriteFile(p+".3", []byte(line(now.Add(-72*time.Hour), "too-old")), 0o600)
	os.Chtimes(p+".3", now.Add(-72*time.Hour), now.Add(-72*time.Hour))
	// not a rotation of it
	os.WriteFile(p+".bak.txt", []byte("other"), 0o600)

	if got := RotatedFiles(p); len(got) != 4 || got[0] != p || got[1] != p+".1" || got[2] != p+".2.gz" || got[3] != p+".3" {
		t.Fatalf("rotated %v", got)
	}
	b := NewBuilder("t", scrubber())
	ls := &LogSet{B: b, Since: 24 * time.Hour, Budget: DefaultLogCap}
	ls.Add(p, "logs", "log")
	cur := b.File("logs/longarmd.log")
	if cur == nil || strings.Contains(string(cur.Data()), "ancient") || !strings.Contains(string(cur.Data()), "recent") ||
		!strings.Contains(string(cur.Data()), "continuation") || !strings.Contains(string(cur.Data()), "earlier bytes left out") {
		t.Fatalf("current log: %v", cur)
	}
	if f := b.File("logs/longarmd.log.1"); f == nil || !strings.Contains(string(f.Data()), "rotated-one") {
		t.Fatal("rotated .1 missing")
	}
	if f := b.File("logs/longarmd.log.2"); f == nil || !strings.Contains(string(f.Data()), "gzipped") {
		t.Fatal("rotated .2.gz missing")
	}
	if b.File("logs/longarmd.log.3") != nil {
		t.Fatal("old rotation included")
	}

	// the cap: newest first, the oldest left out
	big := strings.Repeat(line(now, "x"), 2000) // ~150 KiB
	os.WriteFile(p, []byte(big), 0o600)
	b = NewBuilder("t", scrubber())
	ls = &LogSet{B: b, Since: 24 * time.Hour, Budget: 100 << 10}
	ls.Add(p, "logs", "log")
	cur = b.File("logs/longarmd.log")
	if cur == nil || cur.Size > 101<<10 || cur.Size < 90<<10 {
		t.Fatalf("capped log size %v", cur)
	}
	if b.File("logs/longarmd.log.1") != nil {
		t.Fatal("older log included past the cap")
	}
	excl := 0
	for _, e := range b.ExcludedFiles() {
		if strings.Contains(e.Reason, "cap") {
			excl++
		}
	}
	if excl != 2 {
		t.Fatalf("excluded %+v", b.ExcludedFiles())
	}
}

func TestReportID(t *testing.T) {
	at := time.Date(2026, 10, 27, 23, 30, 0, 0, time.FixedZone("x", -5*3600)) // 28th UTC
	seen := map[string]bool{}
	for range 300 {
		id := NewReportID(at)
		if !ReportIDRE.MatchString(id) || !strings.HasPrefix(id, "LAR-20261028-") || strings.ContainsAny(id[13:], "ILOU") {
			t.Fatalf("id %q", id)
		}
		seen[id] = true
	}
	if len(seen) < 299 {
		t.Fatal("ids repeat")
	}
	p, err := BlobPath("lar-20261028-7f3k9q")
	if err != nil || p != "2026/10/28/LAR-20261028-7F3K9Q.zip" {
		t.Fatalf("%q %v", p, err)
	}
	for _, bad := range []string{"LAR-2026102-7F3K9Q", "LAR-20261028-7F3K9", "../x", "LAR-20261028-7F3K9I"} {
		if _, err := BlobPath(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	// a report id survives scrubbing (it's in notes and logs)
	if s := scrubber().String("see LAR-20261028-7F3K9Q"); s != "see LAR-20261028-7F3K9Q" {
		t.Fatal(s)
	}
}

func TestOpenStore(t *testing.T) {
	for _, bad := range []string{"", "ftp://x/y", "https://acct.blob.core.windows.net/", "https://acct.blob.core.windows.net/c/d",
		"https://acct.blob.core.windows.net/c?sv=1&sig=x", "http://acct.blob.core.windows.net/c", "file://"} {
		if _, err := OpenStore(bad, azauth.Static("t")); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	s, err := OpenStore("https://acct.blob.core.windows.net/diag/", azauth.Static("t"))
	if err != nil || s.Location() != "https://acct.blob.core.windows.net/diag" {
		t.Fatal(s, err)
	}
	if _, err := OpenStore("http://127.0.0.1:10000/devstoreaccount1/diag", azauth.Static("t")); err != nil {
		t.Fatal(err) // an emulator: account in the path
	}
	dir := t.TempDir()
	s, err = OpenStore("file:///"+filepath.ToSlash(dir), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ds, ok := s.(*DirStore); !ok || filepath.Clean(ds.Dir) != filepath.Clean(dir) {
		t.Fatalf("%#v", s)
	}
}

func TestDirStore(t *testing.T) {
	ctx := context.Background()
	s := &DirStore{Dir: t.TempDir()}
	data := []byte("PK zip bytes")
	if err := s.Put(ctx, "2026/10/27/LAR-20261027-AAAAAA.zip", bytes.NewReader(data), int64(len(data)), map[string]string{"node": "<node:1>"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "2026/10/27/LAR-20261027-BBBBBB.zip", bytes.NewReader(data), 99, nil); err == nil {
		t.Fatal("short upload stored")
	}
	if err := s.Put(ctx, "../x.zip", bytes.NewReader(data), int64(len(data)), nil); err == nil {
		t.Fatal("path escape")
	}
	l, err := s.List(ctx, "2026/10/27/")
	if err != nil || len(l) != 1 || l[0].Meta["node"] != "<node:1>" || l[0].Size != int64(len(data)) {
		t.Fatalf("%+v %v", l, err)
	}
	if l, _ := s.List(ctx, "2026/10/26/"); len(l) != 0 {
		t.Fatal(l)
	}
	rc, bi, err := s.Get(ctx, l[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) || bi.Meta["node"] != "<node:1>" {
		t.Fatal("get")
	}
	if _, _, err := s.Get(ctx, "2026/10/27/nope.zip"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

// fakeBlob is an Azure Blob Storage container ("acct"/"diag").
type fakeBlob struct {
	mu    sync.Mutex
	blobs map[string][]byte
	meta  map[string]map[string]string
	reqs  []*http.Request
	fail  int // answer this status to the next request
	page  int // List page size
}

func (f *fakeBlob) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	if r.Header.Get("Authorization") != "Bearer tok-1" || r.Header.Get("x-ms-version") != BlobAPIVersion || r.Header.Get("x-ms-date") == "" {
		w.Header().Set("x-ms-error-code", "NoAuthenticationInformation")
		w.WriteHeader(401)
		return
	}
	if f.fail != 0 {
		w.Header().Set("x-ms-error-code", "AuthorizationPermissionMismatch")
		w.WriteHeader(f.fail)
		fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?><Error><Code>AuthorizationPermissionMismatch</Code><Message>This request is not authorized.
RequestId:1</Message></Error>`)
		f.fail = 0
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/diag") {
		w.WriteHeader(404)
		return
	}
	name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/diag"), "/")
	switch {
	case r.Method == http.MethodPut:
		if r.Header.Get("x-ms-blob-type") != "BlockBlob" || r.ContentLength < 0 {
			w.WriteHeader(400)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if int64(len(b)) != r.ContentLength {
			w.WriteHeader(400)
			return
		}
		f.blobs[name] = b
		m := map[string]string{}
		for k, v := range r.Header {
			if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-ms-meta-") {
				m[strings.TrimPrefix(lk, "x-ms-meta-")] = v[0]
			}
		}
		f.meta[name] = m
		w.WriteHeader(201)
	case r.Method == http.MethodGet && r.URL.Query().Get("comp") == "list":
		if r.URL.Query().Get("restype") != "container" || r.URL.Query().Get("include") != "metadata" {
			w.WriteHeader(400)
			return
		}
		prefix, marker := r.URL.Query().Get("prefix"), r.URL.Query().Get("marker")
		var names []string
		for n := range f.blobs {
			if strings.HasPrefix(n, prefix) && n > marker {
				names = append(names, n)
			}
		}
		sortStrings(names)
		next := ""
		if f.page > 0 && len(names) > f.page {
			names, next = names[:f.page], names[f.page-1]
		}
		type item struct {
			XMLName xml.Name
			Value   string `xml:",chardata"`
		}
		type blob struct {
			Name       string `xml:"Name"`
			Properties struct {
				LastModified  string `xml:"Last-Modified"`
				ContentLength int    `xml:"Content-Length"`
			} `xml:"Properties"`
			Metadata struct {
				Items []item
			} `xml:"Metadata"`
		}
		var res struct {
			XMLName    xml.Name `xml:"EnumerationResults"`
			Blobs      []blob   `xml:"Blobs>Blob"`
			NextMarker string   `xml:"NextMarker"`
		}
		for _, n := range names {
			bl := blob{Name: n}
			bl.Properties.LastModified = time.Now().UTC().Format(http.TimeFormat)
			bl.Properties.ContentLength = len(f.blobs[n])
			for k, v := range f.meta[n] {
				bl.Metadata.Items = append(bl.Metadata.Items, item{XMLName: xml.Name{Local: k}, Value: v})
			}
			res.Blobs = append(res.Blobs, bl)
		}
		res.NextMarker = next
		w.Header().Set("Content-Type", "application/xml")
		xml.NewEncoder(w).Encode(res)
	case r.Method == http.MethodGet:
		b, ok := f.blobs[name]
		if !ok {
			w.Header().Set("x-ms-error-code", "BlobNotFound")
			w.WriteHeader(404)
			return
		}
		for k, v := range f.meta[name] {
			w.Header().Set("x-ms-meta-"+k, v)
		}
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		w.Write(b)
	default:
		w.WriteHeader(405)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func TestBlobStore(t *testing.T) {
	f := &fakeBlob{blobs: map[string][]byte{}, meta: map[string]map[string]string{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	s := &BlobStore{Container: srv.URL + "/diag", Tokens: azauth.Static("tok-1")}
	data := bytes.Repeat([]byte("zipdata"), 1000)
	for _, id := range []string{"LAR-20261027-AAAAAA", "LAR-20261027-BBBBBB", "LAR-20261027-CCCCCC"} {
		p, _ := BlobPath(id)
		if err := s.Put(ctx, p, bytes.NewReader(data), int64(len(data)), map[string]string{"node": "<node:abc123>", "component": "longarmd"}); err != nil {
			t.Fatal(err)
		}
	}
	put := f.reqs[0]
	if put.Method != http.MethodPut || put.URL.Path != "/diag/2026/10/27/LAR-20261027-AAAAAA.zip" || put.Header.Get("Content-Type") != "application/zip" ||
		put.Header.Get("X-Ms-Meta-Node") != "<node:abc123>" || put.ContentLength != int64(len(data)) {
		t.Fatalf("put request %s %s %v", put.Method, put.URL, put.Header)
	}
	f.page = 2 // pagination
	l, err := s.List(ctx, "2026/10/27/")
	if err != nil || len(l) != 3 || l[0].Name != "2026/10/27/LAR-20261027-AAAAAA.zip" || l[2].Size != int64(len(data)) ||
		l[1].Meta["component"] != "longarmd" || l[0].Modified.IsZero() {
		t.Fatalf("list %+v %v", l, err)
	}
	if last := f.reqs[len(f.reqs)-1]; last.URL.Query().Get("marker") == "" || last.URL.Query().Get("prefix") != "2026/10/27/" {
		t.Fatalf("second page %s", last.URL)
	}
	rc, bi, err := s.Get(ctx, l[1].Name)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) || bi.Meta["node"] != "<node:abc123>" || bi.Size != int64(len(data)) {
		t.Fatalf("get %d %+v", len(got), bi)
	}
	// errors
	if _, _, err := s.Get(ctx, "2026/10/27/nope.zip"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: %v", err)
	}
	f.fail = 403
	err = s.Put(ctx, "x.zip", bytes.NewReader(data), int64(len(data)), nil)
	var be *BlobError
	if !errors.As(err, &be) || be.Status != 403 || be.Code != "AuthorizationPermissionMismatch" ||
		!strings.Contains(err.Error(), "Storage Blob Data Contributor") || !strings.Contains(err.Error(), "This request is not authorized.") ||
		strings.Contains(err.Error(), "RequestId") {
		t.Fatalf("403: %v", err)
	}
	bad := &BlobStore{Container: srv.URL + "/diag", Tokens: azauth.Static("wrong")}
	if _, err := bad.List(ctx, ""); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("401: %v", err)
	}
	// a short body never becomes a blob
	if err := s.Put(ctx, "short.zip", strings.NewReader("abc"), 10, nil); err == nil {
		t.Fatal("short body stored")
	}
	if _, ok := f.blobs["short.zip"]; ok {
		t.Fatal("partial blob")
	}
}
