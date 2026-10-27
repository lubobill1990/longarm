package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/jobs"
)

// lfsEnv configures git-lfs's filter as `git lfs install` does (globally,
// for every git these tests run - the worker's situation that made
// checkouts of shipped repositories fail).
func lfsEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		t.Skip("git-lfs not installed")
	}
	kv := [][2]string{
		{"filter.lfs.clean", "git-lfs clean -- %f"},
		{"filter.lfs.smudge", "git-lfs smudge -- %f"},
		{"filter.lfs.process", "git-lfs filter-process"},
		{"filter.lfs.required", "true"},
		{"core.autocrlf", "false"},
	}
	t.Setenv("GIT_CONFIG_COUNT", strconv.Itoa(len(kv)))
	for i, p := range kv {
		n := strconv.Itoa(i)
		t.Setenv("GIT_CONFIG_KEY_"+n, p[0])
		t.Setenv("GIT_CONFIG_VALUE_"+n, p[1])
	}
}

// TestLFSWorktree: a shipped repository with Git LFS files checks out
// although the mirror has no LFS server (pointer files, noted in the log);
// with the objects shipped into the mirror's LFS store the files have
// their content and the worktree is clean.
func TestLFSWorktree(t *testing.T) {
	lfsEnv(t)
	root := t.TempDir()
	s := &Store{Root: filepath.Join(root, "data")}
	ctx := context.Background()
	src := filepath.Join(root, "src")
	os.MkdirAll(src, 0o755)
	run(t, src, "init", "-q", "-b", "main")
	run(t, src, "lfs", "track", "*.bin")
	content := []byte("large binary content\x00\x01\x02\n")
	os.WriteFile(filepath.Join(src, "big.bin"), content, 0o644)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("text\n"), 0o644)
	run(t, src, "add", ".")
	run(t, src, "commit", "-qm", "c1")
	c1 := run(t, src, "rev-parse", "HEAD")
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	if got := run(t, src, "lfs", "ls-files", "--long"); !strings.Contains(got, oid) {
		t.Fatalf("lfs ls-files: %s", got)
	}
	bundle := filepath.Join(root, "b1")
	run(t, src, "bundle", "create", "-q", bundle, "HEAD")
	if err := s.ImportBundle(ctx, "proj", bundle, "main", c1); err != nil {
		t.Fatal(err)
	}

	// Without the objects: pointer files, a note.
	var log bytes.Buffer
	wt := filepath.Join(root, "wt1")
	if err := s.Checkout(ctx, "proj", c1, wt, "", "", &log); err != nil {
		t.Fatalf("checkout of an LFS repository without its objects: %v\n%s", err, log.String())
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "big.bin")); !bytes.HasPrefix(b, []byte("version https://git-lfs")) {
		t.Fatalf("big.bin = %q, want a pointer file", b)
	}
	if !strings.Contains(log.String(), "pointer files") || !strings.Contains(log.String(), "--lfs ship") {
		t.Fatalf("no LFS note in the log:\n%s", log.String())
	}
	if got := s.LFSPresent("proj", []string{oid}); len(got) != 0 {
		t.Fatalf("present before import: %v", got)
	}

	// Ship the object (a tar named by oid, as longarm sends it).
	obj, err := os.ReadFile(filepath.Join(src, ".git", "lfs", "objects", oid[:2], oid[2:4], oid))
	if err != nil {
		t.Fatal(err)
	}
	tarFile := writeLFSTar(t, filepath.Join(root, "lfs.tar"), map[string][]byte{oid: obj})
	if n, err := s.ImportLFS(ctx, "proj", tarFile); err != nil || n != 1 {
		t.Fatalf("ImportLFS = %d %v", n, err)
	}
	if got := s.LFSPresent("proj", []string{oid, strings.Repeat("0", 64), "../x"}); !slices.Equal(got, []string{oid}) {
		t.Fatalf("present = %v", got)
	}
	if n, err := s.ImportLFS(ctx, "proj", tarFile); err != nil || n != 0 {
		t.Fatalf("second ImportLFS = %d %v", n, err)
	}
	log.Reset()
	if err := s.Checkout(ctx, "proj", c1, filepath.Join(root, "wt2"), "", "", &log); err != nil {
		t.Fatalf("checkout with the objects: %v\n%s", err, log.String())
	}
	if b, _ := os.ReadFile(filepath.Join(root, "wt2", "big.bin")); !bytes.Equal(b, content) {
		t.Fatalf("big.bin = %q, want its content\n%s", b, log.String())
	}
	if st := run(t, filepath.Join(root, "wt2"), "status", "--porcelain"); st != "" {
		t.Fatalf("worktree not clean after git lfs checkout:\n%s", st)
	}
	// The existing worktree, reset, gets the content too.
	if err := s.Checkout(ctx, "proj", c1, wt, "", "", &log); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "big.bin")); !bytes.Equal(b, content) {
		t.Fatalf("reset worktree: big.bin = %q", b)
	}

	// Content that does not match its oid, and foreign entries, are refused.
	bad := strings.Repeat("ab", 32)
	if _, err := s.ImportLFS(ctx, "proj", writeLFSTar(t, filepath.Join(root, "bad.tar"), map[string][]byte{bad: []byte("x")})); err == nil {
		t.Fatal("mismatched object accepted")
	}
	if _, err := os.Stat(s.lfsObject("proj", bad)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("mismatched object stored")
	}
	if _, err := s.ImportLFS(ctx, "proj", writeLFSTar(t, filepath.Join(root, "evil.tar"), map[string][]byte{"../../x": []byte("x")})); err == nil {
		t.Fatal("foreign entry accepted")
	}
}

func writeLFSTar(t *testing.T, p string, files map[string][]byte) string {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for name, b := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFetchError: a fetch the remote refused for lack of credentials
// names the cause and the two ways out; URL credentials - a user and
// password, or a token as the user - never reach the message, which does
// not wrap git's error either.
func TestFetchError(t *testing.T) {
	ado := errors.New("git fetch --no-tags --force -- https://user:secret@dev.azure.com/org/p/_git/r +main:refs/longarm/remote/main: exit status 128: " +
		"fatal: unable to access 'https://dev.azure.com/org/p/_git/r/': The requested URL returned error: 403")
	err := fetchError("https://user:secret@dev.azure.com/org/p/_git/r", false, ado)
	msg := err.Error()
	for _, want := range []string{"could not authenticate", "returned error: 403", "--repo .", "dev.azure.com", "Git Credential Manager",
		"https://xxxxx@dev.azure.com/org/p/_git/r"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, ":secret@") || strings.Contains(msg, "user:") || errors.Is(err, ado) {
		t.Errorf("credentials or the raw error in %q", msg)
	}
	pat := errors.New("git fetch -- https://PATTOKEN123@dev.azure.com/o/p/_git/r +main:x: exit status 128: fatal: Authentication failed for 'https://PATTOKEN123@dev.azure.com/o/p/_git/r/'")
	if msg := fetchError("https://PATTOKEN123@dev.azure.com/o/p/_git/r", false, pat).Error(); strings.Contains(msg, "PATTOKEN123") ||
		!strings.Contains(msg, "could not authenticate") {
		t.Errorf("username-only token: %s", msg)
	}
	prompt := errors.New("fatal: could not read Username for 'https://github.com': terminal prompts disabled")
	if msg := fetchError("https://github.com/o/r", true, prompt).Error(); !strings.Contains(msg, "GH_TOKEN") || !strings.Contains(msg, "could not authenticate") {
		t.Errorf("with a token: %s", msg)
	}
	other := errors.New("fatal: couldn't find remote ref no-such-branch in https://tok@example.com/r")
	if got := fetchError("https://github.com/o/r", false, other).Error(); got != "fatal: couldn't find remote ref no-such-branch in https://xxxxx@example.com/r" {
		t.Errorf("unrelated failure: %v", got)
	}
}

func TestRedactURLs(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pw@host/p":                  "https://xxxxx@host/p",
		"fetch 'https://TOKEN@dev.azure.com/o' x": "fetch 'https://xxxxx@dev.azure.com/o' x",
		"ssh://git@host/r and http://a:b@c/d":     "ssh://xxxxx@host/r and http://xxxxx@c/d",
		"https://host/p?x=a@b git@github.com:o/r": "https://host/p?x=a@b git@github.com:o/r",
	} {
		if got := RedactURLs(in); got != want {
			t.Errorf("RedactURLs(%q) = %q, want %q", in, got, want)
		}
	}
	// a URL split across writes is still masked
	var b bytes.Buffer
	w := &redactWriter{w: &b}
	for _, part := range []string{"fatal: https://TOK", "EN@host/r\nnext ", "https://u:p@h/x"} {
		w.Write([]byte(part))
	}
	w.flush()
	if got := b.String(); got != "fatal: https://xxxxx@host/r\nnext https://xxxxx@h/x" {
		t.Fatalf("redactWriter: %q", got)
	}
}

// TestFetchURLRedacts: a failing fetch of a URL with credentials leaves
// them out of the job's log (git's forwarded output), the returned error
// and PrepareJob's (persisted) reason.
func TestFetchURLRedacts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	s := &Store{Root: t.TempDir()}
	u := "https://SECRETTOKEN:pw@127.0.0.1:1/o/r.git" // nothing listens on port 1
	var log bytes.Buffer
	_, err := s.FetchURL(context.Background(), "r", u, "main", "", &log)
	if err == nil {
		t.Fatal("fetch succeeded")
	}
	spec := jobs.Spec{ID: "j1", Workspace: &Workspace{Repo: "r", URL: u, Ref: "main"}}
	_, perr := s.PrepareJob(context.Background(), jobs.Dir(t.TempDir()), spec, nil, &log)
	if perr == nil {
		t.Fatal("PrepareJob succeeded")
	}
	for what, txt := range map[string]string{"log": log.String(), "error": err.Error(), "prepare error": perr.Error()} {
		if strings.Contains(txt, "SECRETTOKEN") || strings.Contains(txt, ":pw@") {
			t.Errorf("%s holds the credentials: %s", what, txt)
		}
	}
	if !strings.Contains(log.String(), "xxxxx@127.0.0.1") {
		t.Errorf("log: %s", log.String())
	}
}

// TestImportLFSBounds: sparse and other non-regular entries are refused,
// objects, totals and counts are bounded before anything is copied, and a
// cancelled import stops.
func TestImportLFSBounds(t *testing.T) {
	root := t.TempDir()
	s := &Store{Root: filepath.Join(root, "data"), LFSLimits: LFSLimits{MaxObjectBytes: 64, MaxTotalBytes: 100, MaxObjects: 3}}
	ctx := context.Background()
	obj := func(b []byte) (string, []byte) { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]), b }

	// A PAX (GNU 1.0) sparse entry: 512 bytes on the wire, a terabyte of
	// holes once expanded.
	sparseOid := strings.Repeat("0", 64)
	sparse := filepath.Join(root, "sparse.tar")
	os.WriteFile(sparse, sparseTar(sparseOid, 1<<40), 0o644)
	if _, err := s.ImportLFS(ctx, "proj", sparse); err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("sparse entry: %v", err)
	}
	if _, err := os.Stat(s.lfsObject("proj", sparseOid)); err == nil {
		t.Fatal("sparse object stored")
	}

	oid, big := obj(bytes.Repeat([]byte("x"), 65))
	if _, err := s.ImportLFS(ctx, "proj", writeLFSTar(t, filepath.Join(root, "big.tar"), map[string][]byte{oid: big})); err == nil ||
		!strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized object: %v", err)
	}
	files := map[string][]byte{}
	for i := 0; i < 2; i++ {
		o, b := obj(bytes.Repeat([]byte{byte('a' + i)}, 60))
		files[o] = b
	}
	if _, err := s.ImportLFS(ctx, "proj", writeLFSTar(t, filepath.Join(root, "total.tar"), files)); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("total over the limit: %v", err)
	}
	files = map[string][]byte{}
	for i := 0; i < 4; i++ {
		o, b := obj([]byte{byte(i)})
		files[o] = b
	}
	if _, err := s.ImportLFS(ctx, "proj", writeLFSTar(t, filepath.Join(root, "count.tar"), files)); err == nil || !strings.Contains(err.Error(), "objects") {
		t.Fatalf("too many objects: %v", err)
	}
	if got := s.LFSPresent("proj", keys(files)); len(got) != 0 {
		t.Fatalf("objects of a refused upload stored: %v", got)
	}

	// links and directories
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: oid, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	tw.Close()
	os.WriteFile(filepath.Join(root, "link.tar"), buf.Bytes(), 0o644)
	if _, err := s.ImportLFS(ctx, "proj", filepath.Join(root, "link.tar")); err == nil {
		t.Fatal("symlink entry accepted")
	}

	// cancelled
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	o, b := obj([]byte("ok"))
	if _, err := s.ImportLFS(cctx, "proj", writeLFSTar(t, filepath.Join(root, "ok.tar"), map[string][]byte{o: b})); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled import: %v", err)
	}
	if n, err := s.ImportLFS(ctx, "proj", filepath.Join(root, "ok.tar")); err != nil || n != 1 {
		t.Fatalf("import within the limits: %d %v", n, err)
	}
	if max := s.MaxLFSUploadBytes(); max < 100 || max > 100+3*4096+1<<20 {
		t.Fatalf("upload cap %d", max)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sparseTar hand-writes a tar holding one PAX GNU 1.0 sparse file named
// name with realSize bytes, all holes (archive/tar can't write these).
func sparseTar(name string, realSize int64) []byte {
	block := func(name string, typ byte, size int) []byte {
		h := make([]byte, 512)
		copy(h[0:], name)
		copy(h[100:], "0000644\x00")
		copy(h[108:], "0000000\x00")
		copy(h[116:], "0000000\x00")
		copy(h[124:], fmt.Sprintf("%011o\x00", size))
		copy(h[136:], "00000000000\x00")
		h[156] = typ
		copy(h[257:], "ustar\x0000")
		copy(h[148:], "        ")
		sum := 0
		for _, c := range h {
			sum += int(c)
		}
		copy(h[148:], fmt.Sprintf("%06o\x00 ", sum))
		return h
	}
	pad := func(b []byte) []byte { return append(b, make([]byte, (512-len(b)%512)%512)...) }
	rec := func(k, v string) string {
		line := " " + k + "=" + v + "\n"
		n := len(line)
		for {
			s := fmt.Sprintf("%d%s", n, line)
			if len(s) == n {
				return s
			}
			n = len(s)
		}
	}
	pax := rec("GNU.sparse.major", "1") + rec("GNU.sparse.minor", "0") + rec("GNU.sparse.name", name) +
		rec("GNU.sparse.realsize", fmt.Sprint(realSize))
	sparseMap := pad([]byte("1\n" + fmt.Sprint(realSize) + "\n0\n")) // one empty data segment at the end
	var out []byte
	out = append(out, block("PaxHeaders/"+name[:20], 'x', len(pax))...)
	out = append(out, pad([]byte(pax))...)
	out = append(out, block("GNUSparseFile.0/"+name[:20], '0', len(sparseMap))...)
	out = append(out, sparseMap...)
	return append(out, make([]byte, 1024)...)
}

// TestLFSWorktreeNested: LFS attributes in a nested .gitattributes only -
// the checkout still skips the smudge, and shipped objects are checked out.
func TestLFSWorktreeNested(t *testing.T) {
	lfsEnv(t)
	root := t.TempDir()
	s := &Store{Root: filepath.Join(root, "data")}
	ctx := context.Background()
	src := filepath.Join(root, "src")
	os.MkdirAll(filepath.Join(src, "assets"), 0o755)
	run(t, src, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(src, "assets", ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644)
	content := []byte("nested lfs content\n")
	os.WriteFile(filepath.Join(src, "assets", "big.bin"), content, 0o644)
	run(t, src, "add", ".")
	run(t, src, "commit", "-qm", "c1")
	c1 := run(t, src, "rev-parse", "HEAD")
	if _, err := os.Stat(filepath.Join(src, ".gitattributes")); err == nil {
		t.Fatal("test setup: root .gitattributes")
	}
	bundle := filepath.Join(root, "b1")
	run(t, src, "bundle", "create", "-q", bundle, "HEAD")
	if err := s.ImportBundle(ctx, "proj", bundle, "main", c1); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if err := s.Checkout(ctx, "proj", c1, filepath.Join(root, "wt1"), "", "", &log); err != nil {
		t.Fatalf("checkout: %v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "pointer files") {
		t.Fatalf("no LFS note for nested attributes:\n%s", log.String())
	}
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	obj, err := os.ReadFile(filepath.Join(src, ".git", "lfs", "objects", oid[:2], oid[2:4], oid))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportLFS(ctx, "proj", writeLFSTar(t, filepath.Join(root, "lfs.tar"), map[string][]byte{oid: obj})); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkout(ctx, "proj", c1, filepath.Join(root, "wt2"), "", "", &log); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "wt2", "assets", "big.bin")); !bytes.Equal(b, content) {
		t.Fatalf("nested LFS file = %q\n%s", b, log.String())
	}
}
