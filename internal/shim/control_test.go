package shim

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/attach"
)

// slowWriter records writes; each write is slow so that unserialized
// writers would interleave.
type slowWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *slowWriter) Write(p []byte) (int, error) {
	for _, b := range p { // byte by byte: interleaving shows if writes overlap
		w.mu.Lock()
		w.buf.WriteByte(b)
		w.mu.Unlock()
		if b == 'x' || b == 'y' {
			time.Sleep(10 * time.Microsecond)
		}
	}
	return len(p), nil
}

func TestHubInputContiguousAndIdle(t *testing.T) {
	h := &hub{clients: map[*client]struct{}{}}
	w := &slowWriter{}
	var wg sync.WaitGroup
	a, b := bytes.Repeat([]byte("x"), 200), bytes.Repeat([]byte("y"), 200)
	for i := 0; i < 5; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); h.input(w, a, attach.SourceAttach, 0) }()
		go func() { defer wg.Done(); h.input(w, b, attach.SourceSend, 0) }()
	}
	wg.Wait()
	out := w.buf.String()
	for i := 0; i < len(out); i += 200 { // runs of 200 equal bytes
		if run := out[i : i+200]; run != string(a) && run != string(b) {
			t.Fatalf("interleaved writes at %d: %q", i, run)
		}
	}
	st := h.info()
	if st.InputSeq != 10 || st.LastInput.IsZero() {
		t.Fatalf("info %+v", st)
	}
	// Input arrived just now: an if-idle send is refused and writes nothing.
	n := w.buf.Len()
	if sent, _ := h.input(w, []byte("z"), attach.SourceSend, time.Hour); sent || w.buf.Len() != n || h.info().InputSeq != 10 {
		t.Fatal("if-idle send written although input was recent")
	}
	time.Sleep(30 * time.Millisecond)
	if sent, _ := h.input(w, []byte("z"), attach.SourceSend, 20*time.Millisecond); !sent || h.info().InputSeq != 11 || h.info().LastInputSource != attach.SourceSend {
		t.Fatal("if-idle send refused after the idle window")
	}
	h.output([]byte("o"))
	if h.info().LastOutput.IsZero() {
		t.Fatal("no last output")
	}
}

type failWriter struct{ n int }

func (w failWriter) Write(p []byte) (int, error) {
	if w.n >= 0 {
		return min(w.n, len(p)), nil // short, without an error
	}
	return 0, io.ErrClosedPipe
}

// A write that fails or is short is never reported as sent.
func TestHubInputWriteErrors(t *testing.T) {
	h := &hub{clients: map[*client]struct{}{}}
	if sent, err := h.input(failWriter{n: -1}, []byte("abc"), attach.SourceSend, 0); sent || err == nil || h.info().InputSeq != 0 {
		t.Fatalf("failed write: sent=%v err=%v seq=%d", sent, err, h.info().InputSeq)
	}
	if sent, err := h.input(failWriter{n: 1}, []byte("abc"), attach.SourceSend, 0); sent || !errors.Is(err, io.ErrShortWrite) || h.info().InputSeq != 1 {
		t.Fatalf("short write: sent=%v err=%v seq=%d", sent, err, h.info().InputSeq)
	}
}

// if-input-seq: written only while inSeq is unchanged; the after part
// follows the gap with no other writer in between, and counts as one input.
func TestHubSendIfSeqAndAfter(t *testing.T) {
	h := &hub{clients: map[*client]struct{}{}}
	w := &slowWriter{}
	h.input(w, []byte("a"), attach.SourceAttach, 0)
	stale, cur := uint64(0), uint64(1)
	if sent, _ := h.send(w, []byte("p"), nil, attach.SourceSend, sendCond{ifSeq: &stale}); sent || w.buf.String() != "a" {
		t.Fatalf("if-seq stale: sent=%v %q", sent, w.buf.String())
	}
	done := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond) // within the gap
		h.input(w, []byte("k"), attach.SourceAttach, 0)
		close(done)
	}()
	if sent, err := h.send(w, []byte("[paste]"), []byte("\r"), attach.SourceSend, sendCond{ifSeq: &cur, afterGap: 100 * time.Millisecond}); !sent || err != nil {
		t.Fatalf("if-seq current: sent=%v err=%v", sent, err)
	}
	<-done
	if got := w.buf.String(); got != "a[paste]\rk" {
		t.Fatalf("writes %q", got)
	}
	if st := h.info(); st.InputSeq != 3 || st.HumanInputSeq != 2 || !st.Has(attach.FeatureSendX) {
		t.Fatalf("info %+v", st)
	}
}

// Terminal replies of an attached client are written but are no input.
func TestHubRepliesNotInput(t *testing.T) {
	h := &hub{clients: map[*client]struct{}{}}
	w := &slowWriter{}
	h.input(w, []byte("x"), attach.SourceSend, 0)
	before := h.info()
	time.Sleep(5 * time.Millisecond)
	if sent, _ := h.input(w, []byte("\x1b[?1;2c"), attach.SourceReply, 0); !sent {
		t.Fatal("reply not written")
	}
	st := h.info()
	if w.buf.String() != "x\x1b[?1;2c" || st.InputSeq != 1 || st.HumanInputSeq != 0 || st.LastInputSource != attach.SourceSend || !st.LastInput.Equal(before.LastInput) {
		t.Fatalf("after a reply: %q %+v", w.buf.String(), st)
	}
	// an if-input-seq send taken after the reply still matches
	seq := st.InputSeq
	if sent, _ := h.send(w, []byte("y"), nil, attach.SourceSend, sendCond{ifSeq: &seq}); !sent {
		t.Fatal("a reply moved inputSeq")
	}
}
