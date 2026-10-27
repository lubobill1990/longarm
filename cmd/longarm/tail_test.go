package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// A stream ending in a prefix of the sequence keeps those bytes.
func TestWin32InputFilterFlush(t *testing.T) {
	f := &win32InputFilter{}
	out := string(f.filter([]byte("payload\x1b[?90"))) + string(f.flush())
	if out != "payload\x1b[?90" {
		t.Fatalf("got %q", out)
	}
	if len(f.flush()) != 0 {
		t.Fatal("flushed twice")
	}
}

// tail --json: a huge line without newlines is cut into partial records of
// at most maxTailRecord bytes (memory stays bounded), not inside a
// character, and the offsets stay contiguous.
func TestTailWriterLongLines(t *testing.T) {
	old := maxTailRecord
	maxTailRecord = 10
	defer func() { maxTailRecord = old }()
	var out bytes.Buffer
	tw := &tailWriter{w: &out, asJSON: true}
	data := strings.Repeat("a", 9) + "é" + strings.Repeat("b", 20) // é (2 bytes) straddles the cap
	off := int64(0)
	for _, c := range []string{data[:5], data[5:17], data[17:], "c\r\n"} {
		if err := tw.chunk(&pb.FileChunk{Offset: off, Data: []byte(c)}); err != nil {
			t.Fatal(err)
		}
		off += int64(len(c))
		if len(tw.pending) >= maxTailRecord {
			t.Fatalf("pending grew to %d", len(tw.pending))
		}
	}
	var lines []tailLine
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var tl tailLine
		if err := json.Unmarshal([]byte(l), &tl); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, tl)
	}
	joined := ""
	for i, l := range lines {
		joined += l.Line
		if l.Partial != (i < len(lines)-1) || len(l.Line) > 10 || strings.ContainsRune(l.Line, '�') {
			t.Errorf("record %d: %+v", i, l)
		}
		if i > 0 && l.Offset != lines[i-1].Next {
			t.Errorf("record %d offsets: %+v after %+v", i, l, lines[i-1])
		}
	}
	if joined != data+"c" || lines[len(lines)-1].Next != off {
		t.Fatalf("records %+v", lines)
	}
}

// Scanning is incremental: many small chunks of one long line do not
// rescan what was scanned before (the line still comes out whole).
func TestTailWriterManyChunks(t *testing.T) {
	var out bytes.Buffer
	tw := &tailWriter{w: &out, asJSON: true}
	off := int64(0)
	for i := 0; i < 2000; i++ {
		tw.chunk(&pb.FileChunk{Offset: off, Data: []byte("xy")})
		off += 2
		if tw.scanned != len(tw.pending) {
			t.Fatalf("scanned %d of %d", tw.scanned, len(tw.pending))
		}
	}
	tw.chunk(&pb.FileChunk{Offset: off, Data: []byte("\n")})
	var tl tailLine
	if err := json.Unmarshal(out.Bytes(), &tl); err != nil || len(tl.Line) != 4000 || tl.Next != off+1 {
		t.Fatalf("%+v %v", tl, err)
	}
}
