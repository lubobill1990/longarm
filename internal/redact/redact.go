// Package redact replaces known secret values in output streams before they
// reach disk or the network (DESIGN §5.4: secrets never enter logs).
package redact

import (
	"bytes"
	"io"
	"sort"
	"sync"
)

// Mask replaces every occurrence of a secret value.
const Mask = "***"

// MinLen is the shortest secret value that is redacted. Shorter values (e.g.
// "1", "on") would mangle unrelated output and protect nothing.
const MinLen = 4

// Redactor holds a set of secret values.
type Redactor struct {
	secrets [][]byte // longest first
	first   [256]bool
	maxLen  int
}

// New builds a Redactor; values shorter than MinLen are ignored. Returns nil
// if nothing needs redacting (a nil *Redactor is valid and redacts nothing).
func New(values []string) *Redactor {
	r := &Redactor{}
	seen := map[string]bool{}
	for _, v := range values {
		if len(v) < MinLen || seen[v] {
			continue
		}
		seen[v] = true
		r.secrets = append(r.secrets, []byte(v))
		r.first[v[0]] = true
		if len(v) > r.maxLen {
			r.maxLen = len(v)
		}
	}
	if len(r.secrets) == 0 {
		return nil
	}
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
	return r
}

// Bytes returns b with all secrets masked (b itself if there is nothing to do).
func (r *Redactor) Bytes(b []byte) []byte {
	if r == nil {
		return b
	}
	return r.replace(b)
}

func (r *Redactor) replace(b []byte) []byte {
	out, _ := r.replaceUntil(b, len(b))
	return out
}

// replaceUntil masks every secret that starts before limit (matches may
// extend past limit) and returns the output plus how many input bytes were
// consumed.
func (r *Redactor) replaceUntil(b []byte, limit int) ([]byte, int) {
	var out []byte
	last, i := 0, 0
	for i < limit {
		if r.first[b[i]] {
			if n := r.matchAt(b, i); n > 0 {
				if out == nil {
					out = make([]byte, 0, len(b))
				}
				out = append(out, b[last:i]...)
				out = append(out, Mask...)
				i += n
				last = i
				continue
			}
		}
		i++
	}
	if out == nil {
		return b[:i], i
	}
	return append(out, b[last:i]...), i
}

func (r *Redactor) matchAt(b []byte, i int) int {
	for _, s := range r.secrets {
		if bytes.HasPrefix(b[i:], s) {
			return len(s)
		}
	}
	return 0
}

// holdFrom returns the earliest position p such that b[p:] is a proper
// prefix of some secret, i.e. a secret starting at p could still complete
// once more data arrives (len(b) if there is none). Nothing at or after p may
// be decided yet: deciding early would let a shorter secret that is a prefix
// of a longer one mask only the head of the longer one.
func (r *Redactor) holdFrom(b []byte) int {
	start := len(b) - (r.maxLen - 1)
	if start < 0 {
		start = 0
	}
	for p := start; p < len(b); p++ {
		if !r.first[b[p]] {
			continue
		}
		rest := b[p:]
		for _, s := range r.secrets {
			if len(s) > len(rest) && bytes.HasPrefix(s, rest) {
				return p
			}
		}
	}
	return len(b)
}

// Writer is a streaming redacting io.WriteCloser. A secret split across
// Write calls is still caught: only the minimal possibly-secret suffix is held
// back until the next Write or Flush/Close.
type Writer struct {
	mu      sync.Mutex
	w       io.Writer
	r       *Redactor
	pending []byte // raw (unredacted) input not decided yet
}

// NewWriter wraps w. With a nil Redactor it is a pass-through.
func NewWriter(w io.Writer, r *Redactor) *Writer { return &Writer{w: w, r: r} }

// Write implements io.Writer. It reports len(p) on success even though some
// bytes may be held back or replaced.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.r == nil {
		return w.w.Write(p)
	}
	buf := append(w.pending, p...)
	out, used := w.r.replaceUntil(buf, w.r.holdFrom(buf))
	if len(out) > 0 {
		if _, err := w.w.Write(out); err != nil {
			return 0, err
		}
	}
	w.pending = append(w.pending[:0:0], buf[used:]...)
	return len(p), nil
}

// Flush redacts and writes any held-back bytes (no more data will come to
// complete a longer secret).
func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return nil
	}
	_, err := w.w.Write(w.r.replace(w.pending))
	w.pending = nil
	return err
}

// Close flushes; it does not close the underlying writer.
func (w *Writer) Close() error { return w.Flush() }
