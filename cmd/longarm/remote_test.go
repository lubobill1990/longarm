package main

import (
	"runtime"
	"testing"

	"github.com/lubobill1990/longarm/internal/client"
)

func TestSplitRemote(t *testing.T) {
	cfg := &client.Config{Workers: []client.Worker{{Name: "w1"}, {Name: "x"}}}
	for in, want := range map[string][3]string{
		"w1:C:/work/p":          {"w1", "C:/work/p", "ok"},
		"w1:":                   {"w1", "", "ok"},
		"x:rel/path":            {"x", "rel/path", "ok"},
		`C:\local\file`:         {"", "", ""},
		"a@127.0.0.1:7701:/tmp": {"a@127.0.0.1:7701", "/tmp", "ok"},
		"a@[::1]:7701:C:/x":     {"a@[::1]:7701", "C:/x", "ok"},
		"a@127.0.0.1:7701":      {"", "", ""},
		"plainfile":             {"", "", ""},
	} {
		w, p, ok := splitRemote(cfg, in)
		got := [3]string{w, p, ""}
		if ok {
			got[2] = "ok"
		}
		if got != want {
			t.Errorf("splitRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCRReader(t *testing.T) {
	r := crReader{r: stringsReader("a\nb\r\nc")}
	b := make([]byte, 100)
	n, _ := r.Read(b)
	if string(b[:n]) != "a\rb\rc" {
		t.Fatalf("%q", b[:n])
	}
}

type stringsReaderT struct {
	s    string
	done bool
}

func (s *stringsReaderT) Read(p []byte) (int, error) {
	if s.done {
		return 0, nil
	}
	s.done = true
	return copy(p, s.s), nil
}

func stringsReader(s string) *stringsReaderT { return &stringsReaderT{s: s} }

func TestSafeJoin(t *testing.T) {
	dir := t.TempDir()
	for _, ok := range []string{"a.txt", "sub/b", "x/./y"} {
		if _, err := safeJoin(dir, ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	bad := []string{"", ".", "..", "../x", "a/../../x", "/etc/passwd"}
	if runtime.GOOS == "windows" {
		bad = append(bad, `..\x`, "C:/x", "C:x")
	}
	for _, b := range bad {
		if p, err := safeJoin(dir, b); err == nil {
			t.Errorf("%q accepted as %s", b, p)
		}
	}
}
