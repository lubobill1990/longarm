package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func TestTyped(t *testing.T) {
	for in, want := range map[string]string{"a\nb": "a\rb", "a\r\nb\n": "a\rb\r", "x": "x", "\n": "\r"} {
		if got := string(typed([]byte(in))); got != want {
			t.Errorf("typed(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeyBytes(t *testing.T) {
	for k, want := range map[string]string{"enter": "\r", "ESC": "\x1b", "ctrl-c": "\x03", "ctrl-z": "\x1a", "up": "\x1b[A", "shift-tab": "\x1b[Z"} {
		if b, err := keyBytes(k); err != nil || string(b) != want {
			t.Errorf("%s: %q %v", k, b, err)
		}
	}
	for _, bad := range []string{"ctrl-", "ctrl-cc", "f13", ""} {
		if _, err := keyBytes(bad); err == nil || exitCodeFor(err, true) != exitUsage {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTailWriterJSON(t *testing.T) {
	var out bytes.Buffer
	tw := &tailWriter{w: &out, asJSON: true}
	tw.chunk(&pb.FileChunk{Offset: 0, Data: []byte("{\"a\":1}\n{\"b\"")})
	tw.chunk(&pb.FileChunk{Offset: 12, Data: []byte(":2}\r\nrest")})
	tw.chunk(&pb.FileChunk{Offset: 0, Data: []byte("new\n")}) // truncated
	tw.chunk(&pb.FileChunk{Offset: 4, Data: []byte("tail")})
	tw.flush()
	var got []tailLine
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var tl tailLine
		if err := json.Unmarshal([]byte(l), &tl); err != nil {
			t.Fatal(l, err)
		}
		got = append(got, tl)
	}
	want := []tailLine{
		{Offset: 0, Next: 8, Line: `{"a":1}`},
		{Offset: 8, Next: 17, Line: `{"b":2}`},
		{Offset: 0, Next: 0, Truncated: true},
		{Offset: 0, Next: 4, Line: "new"},
		{Offset: 4, Next: 8, Line: "tail", Partial: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSessionsJSON(t *testing.T) {
	b := sessionsJSONBytes([]sessionRow{{worker: "w1", s: &pb.Session{Name: "dev", State: pb.JobState_JOB_STATE_RUNNING, InputSeq: 3}}}, []string{"w2"})
	var v struct {
		Sessions    []map[string]any `json:"sessions"`
		Source      string           `json:"source"`
		Unreachable []string         `json:"unreachable"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	s := v.Sessions[0]
	if v.Source != "workers" || len(v.Unreachable) != 1 || s["worker"] != "w1" || s["name"] != "dev" || s["inputSeq"] != "3" {
		t.Fatalf("%s", b)
	}
	for _, k := range []string{"attached", "exitCode", "hasExitCode", "pid", "cwd", "dir", "workdir", "lastActivity", "lastInputSource"} {
		if _, ok := s[k]; !ok {
			t.Errorf("field %s missing: %s", k, b)
		}
	}
	if b := sessionsJSONBytes(nil, nil); !strings.Contains(string(b), `"sessions": []`) {
		t.Fatalf("empty: %s", b)
	}
}

func TestWin32InputFilter(t *testing.T) {
	f := &win32InputFilter{}
	var out []byte
	for _, c := range []string{"\x1b[?9001h\x1b[?1004hready\r\n\x1b[?90", "01lnext \x1b[A", "\x1b", "[?9001", "h", "end"} {
		out = append(out, f.filter([]byte(c))...)
	}
	if got, want := string(out), "\x1b[?1004hready\r\nnext \x1b[Aend"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	f = &win32InputFilter{}
	if got := string(f.filter([]byte("\x1b[?90x"))); got != "\x1b[?90x" {
		t.Fatalf("a different sequence: %q", got)
	}
}
