package shim

import (
	"bytes"
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
