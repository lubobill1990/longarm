// Package logfile implements size-capped job/session logs that keep absolute
// byte offsets stable across rotation.
//
// A log is a stream of bytes; clients address it by absolute offset (longarm logs
// --from N, resumable follow). On disk it is at most two segments:
//
//	out.log         current segment: stream bytes [base, base+size)
//	out.log.base    decimal base of out.log (missing = 0, i.e. never rotated)
//	out.log.1       previous segment, stream bytes [base1, base)
//	out.log.1.base  decimal base1
//
// When writing would grow out.log past Max, the writer renames it to out.log.1
// (dropping the older one), records the bases and starts a fresh out.log, so a
// log never uses more than ~2×Max on disk. Plain `tail out.log` keeps working;
// readers asking for bytes that were rotated away get data from the oldest
// offset still available (the returned start tells them how much was lost).
package logfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
)

const (
	baseSuffix = ".base"
	prevSuffix = ".1"
)

// Writer appends to a log, rotating it at Max bytes. Not safe for concurrent
// use; one process (the shim, or longarmd before the shim exists) writes a log.
//
// A failed rotation (e.g. on Windows another program holds out.log open
// without delete sharing) never breaks the writer: it keeps appending to the
// current segment beyond the cap and retries the rotation later.
type Writer struct {
	path    string
	max     int64
	f       *os.File
	base    int64
	size    int64
	retryAt time.Time
	// OnRotateError, if set, is told about failed rotations.
	OnRotateError func(error)
}

// rotateRetry is how long to wait after a failed rotation.
const rotateRetry = 10 * time.Second

// OpenWriter opens (or creates) the log at path for appending. max <= 0
// disables rotation.
func OpenWriter(path string, max int64) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Writer{path: path, max: max, f: f, base: readBase(path), size: fi.Size()}, nil
}

// Write appends p, rotating first whenever the current segment is full.
func (w *Writer) Write(p []byte) (int, error) {
	if w.f == nil {
		return 0, os.ErrClosed
	}
	written := 0
	for len(p) > 0 {
		chunk := p
		if w.max > 0 {
			if w.size >= w.max && !time.Now().Before(w.retryAt) {
				if err := w.rotate(); err != nil {
					w.retryAt = time.Now().Add(rotateRetry)
					if w.OnRotateError != nil {
						w.OnRotateError(err)
					}
					if w.f == nil {
						return written, err
					}
				}
			}
			if room := w.max - w.size; room > 0 && int64(len(chunk)) > room {
				chunk = chunk[:room]
			}
		}
		n, err := w.f.Write(chunk)
		written += n
		w.size += int64(n)
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// rotate moves the current segment to path.1 and starts an empty one. The
// order matters for lock-free readers (see Read): the current path vanishes
// first, then the bases are updated, then the new segment appears. If the
// rename fails, the current segment is reopened and stays in use.
func (w *Writer) rotate() error {
	_ = w.f.Sync()
	if err := w.f.Close(); err != nil {
		return err
	}
	w.f = nil
	reopen := func() error {
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		w.f = f
		return nil
	}
	prev := w.path + prevSuffix
	if err := platform.ReplaceFile(w.path, prev); err != nil {
		if rerr := reopen(); rerr != nil {
			return fmt.Errorf("rotate %s: %v; reopen: %w", w.path, err, rerr)
		}
		return fmt.Errorf("rotate %s: %w", w.path, err)
	}
	errPrev := writeBase(prev, w.base)
	w.base += w.size
	errCur := writeBase(w.path, w.base)
	if err := reopen(); err != nil {
		return err
	}
	w.size = 0
	return errors.Join(errPrev, errCur)
}

// Sync flushes the current segment to disk.
func (w *Writer) Sync() error {
	if w.f == nil {
		return nil
	}
	return w.f.Sync()
}

// Close closes the current segment.
func (w *Writer) Close() error {
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// End returns the absolute offset after the last byte written.
func (w *Writer) End() int64 { return w.base + w.size }

func readBase(path string) int64 {
	b, err := platform.ReadFileShared(path + baseSuffix)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func writeBase(path string, base int64) error {
	dir, name := filepath.Split(path + baseSuffix)
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(strconv.FormatInt(base, 10))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = platform.ReplaceFile(tmp.Name(), path+baseSuffix)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// Size returns the absolute end offset of the log at path (0 if missing).
func Size(path string) int64 {
	for i := 0; i < 20; i++ {
		base := readBase(path)
		fi, err := os.Stat(path)
		if err == nil && readBase(path) == base {
			return base + fi.Size()
		}
		if err != nil && base == 0 {
			return 0
		}
		time.Sleep(5 * time.Millisecond) // mid-rotation
	}
	return readBase(path)
}

var errRetry = errors.New("log rotated during read")

// Read returns up to max bytes of the log at path starting at absolute
// offset (negative: that many bytes before the end). start is the offset of
// data[0] - larger than the requested offset when those bytes were rotated
// away - and end the log's current end offset. An offset at or past the end
// yields no data and start == offset (callers may wait for growth).
func Read(path string, offset int64, max int) (data []byte, start, end int64, err error) {
	for i := 0; i < 100; i++ {
		data, start, end, err = tryRead(path, offset, max)
		if !errors.Is(err, errRetry) {
			return data, start, end, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, 0, 0, fmt.Errorf("%s: %w", path, errRetry)
}

func tryRead(path string, offset int64, max int) ([]byte, int64, int64, error) {
	f, err := platform.OpenShared(path)
	if errors.Is(err, os.ErrNotExist) {
		if readBase(path) > 0 {
			return nil, 0, 0, errRetry // between rotation steps
		}
		if offset < 0 {
			offset = 0
		}
		return nil, offset, 0, nil
	}
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	base := readBase(path)
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, 0, err
	}
	end := base + fi.Size()
	start := offset
	if start < 0 {
		start = max64(end+offset, 0)
	}
	if start >= end {
		return nil, start, end, nil
	}
	var data []byte
	if start < base {
		var s int64
		if data, s, err = readPrev(path, start, base, max); err != nil {
			return nil, 0, 0, err
		}
		if start = s; len(data) == 0 {
			start = base // nothing older is left; continue with the current segment
		}
	}
	if len(data) == 0 {
		if data, err = readAt(f, start-base, min64(int64(max), end-start)); err != nil {
			return nil, 0, 0, err
		}
	}
	// Valid only if no rotation happened meanwhile: same base, and the file
	// we read is still the current segment.
	if readBase(path) != base {
		return nil, 0, 0, errRetry
	}
	if cur, err := os.Stat(path); err != nil || !os.SameFile(fi, cur) {
		return nil, 0, 0, errRetry
	}
	return data, start, end, nil
}

// readPrev reads from the previous segment; start < base (the current base).
func readPrev(path string, start, base int64, max int) ([]byte, int64, error) {
	prev := path + prevSuffix
	f, err := platform.OpenShared(prev)
	if errors.Is(err, os.ErrNotExist) {
		return nil, base, nil // rotated away entirely
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	pbase := readBase(prev)
	if start < pbase {
		start = pbase
	}
	data, err := readAt(f, start-pbase, min64(int64(max), base-start))
	return data, start, err
}

func readAt(f *os.File, off, n int64) ([]byte, error) {
	if n <= 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	k, err := f.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:k], nil
}

// Remove deletes every file of the log.
func Remove(path string) error {
	var errs []error
	for _, p := range []string{path, path + baseSuffix, path + prevSuffix, path + prevSuffix + baseSuffix} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
