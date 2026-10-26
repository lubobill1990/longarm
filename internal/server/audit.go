package server

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
)

// maxAuditRecord caps one audit line (bytes).
const maxAuditRecord = 64 << 10

// AuditChainStart is the method of the record that begins a new hash chain:
// in a log written before records were chained, after an incomplete line
// (crash) and after audit.log changed behind longarmd's back.
const AuditChainStart = "audit/chain-start"

// Audit appends one JSON line per security-relevant call to a local file
// (DESIGN §6.4: who, where, what command, exit code). Never logs secret
// values - only env names.
//
// The lines form a hash chain: each record carries "seq" (1, 2, ...) and
// "prev", the hex SHA-256 of the previous line ("" for the first line of a
// file), so editing, deleting or reordering lines breaks the chain
// (VerifyAudit). Copies go to the owner's sinks (StartSinks).
type Audit struct {
	mu   sync.Mutex
	path string

	// Chain state, guarded by mu. size is what audit.log should measure;
	// another size means someone else changed the file.
	synced bool
	resync string // why the next write re-reads the file ("" at start)
	size   int64
	seq    int64
	prev   string

	sinks     []*sinkQueue
	stopSinks func()
}

// NewAudit returns an audit log writing to path ("" = disabled).
func NewAudit(path string) *Audit { return &Audit{path: path} }

// Log appends a record (with the acting user when the certificate is a
// device of a user).
func (a *Audit) Log(id pki.Identity, method string, fields map[string]any) {
	if id.User != "" && id.User != id.Name {
		if fields == nil {
			fields = map[string]any{}
		}
		fields["user"] = id.User
	}
	a.write(id.String(), method, fields)
}

// LogAs appends a record for a caller identified by a string (sshd).
func (a *Audit) LogAs(who, method string, fields map[string]any) {
	a.write(who, method, fields)
}

func (a *Audit) write(who, method string, fields map[string]any) {
	if a == nil || a.path == "" {
		return
	}
	// Marshal outside the lock; the chain fields are spliced in under it.
	b := auditBody(who, method, fields)
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.open()
	a.append(f, b)
	if f != nil {
		f.Close()
	}
}

// auditBody is the JSON object of a record without the chain fields.
func auditBody(who, method string, fields map[string]any) []byte {
	rec := map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "who": who, "method": method}
	for k, v := range fields {
		if k != "seq" && k != "prev" { // the chain's
			rec[k] = v
		}
	}
	b, _ := json.Marshal(rec)
	if len(b) > maxAuditRecord { // e.g. a huge argv: keep lines bounded for readers and the hub
		b, _ = json.Marshal(map[string]any{"time": rec["time"], "who": who, "method": method, "truncated": len(b),
			"head": string(b[:4096])})
	}
	return b
}

// open opens audit.log for appending and, at the first write or when its
// size isn't what longarmd left, re-reads its end to continue the chain.
// nil: not writable (the record still goes to the sinks).
func (a *Audit) open() *os.File {
	// Write-only: on Windows a handle with read access costs a virus scan
	// at each close (~20 ms); resume reads through its own.
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		a.synced, a.resync = false, "audit.log was not writable: "+err.Error()
		return nil
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		a.synced, a.resync = false, "audit.log was not readable: "+err.Error()
		return nil
	}
	if !a.synced || fi.Size() != a.size {
		reason := a.resync
		if a.synced {
			reason = fmt.Sprintf("audit.log changed by another writer (size %d, expected %d)", fi.Size(), a.size)
		}
		a.resume(f, fi.Size(), reason)
	}
	return f
}

// resume sets the chain state from the last line of audit.log (size
// bytes; f appends to it). It starts a new chain (a chain-start record)
// unless that line is a chained record and nothing unusual happened
// (reason "").
func (a *Audit) resume(f *os.File, size int64, reason string) {
	a.synced, a.resync, a.size, a.prev = true, "", size, ""
	if size == 0 {
		if reason != "" {
			a.seq = 0
			a.append(f, chainStart(reason, nil))
		}
		return
	}
	r, err := os.Open(a.path)
	if err != nil {
		a.synced, a.resync = false, "audit.log was not readable: "+err.Error()
		return
	}
	defer r.Close()
	start, end, complete, err := lastLine(r, size)
	if err != nil {
		a.synced, a.resync = false, "audit.log was not readable: "+err.Error()
		return
	}
	h := sha256.New()
	io.Copy(h, io.NewSectionReader(r, start, end-start))
	a.prev = hex.EncodeToString(h.Sum(nil))
	if !complete {
		// A crash mid-line: terminate it (it stays as evidence) and chain on
		// from it.
		if _, err := f.Write([]byte{'\n'}); err == nil {
			a.size++
		}
		if reason == "" {
			reason = "incomplete last line"
		}
	}
	var last chainFields
	if reason == "" && end-start <= 2*maxAuditRecord {
		b := make([]byte, end-start)
		if _, err := r.ReadAt(b, start); err == nil && json.Unmarshal(b, &last) == nil && last.chained() {
			a.seq = *last.Seq
			return
		}
	}
	if reason == "" {
		reason = "log without hash chain"
	}
	a.seq = 0
	a.append(f, chainStart(reason, map[string]any{"size": a.size}))
}

func chainStart(reason string, fields map[string]any) []byte {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["reason"] = reason
	return auditBody("longarmd", AuditChainStart, fields)
}

// append chains body (a JSON object), writes it to f (nil: no file) and
// hands it to the sinks.
func (a *Audit) append(f *os.File, body []byte) {
	a.seq++
	line := make([]byte, 0, len(body)+100)
	line = append(line, `{"seq":`...)
	line = strconv.AppendInt(line, a.seq, 10)
	line = append(line, `,"prev":"`...)
	line = append(line, a.prev...)
	line = append(line, `",`...)
	line = append(line, body[1:]...)
	sum := sha256.Sum256(line)
	a.prev = hex.EncodeToString(sum[:])
	line = append(line, '\n')
	if f != nil {
		if _, err := f.Write(line); err != nil {
			a.synced, a.resync = false, "write to audit.log failed: "+err.Error()
		} else {
			a.size += int64(len(line))
		}
	}
	for _, q := range a.sinks {
		q.push(line)
	}
}

// lastLine finds the last line of r (size bytes): [start, end) without the
// newline; complete reports whether it ends with one.
func lastLine(r io.ReaderAt, size int64) (start, end int64, complete bool, err error) {
	var b [1]byte
	if _, err = r.ReadAt(b[:], size-1); err != nil {
		return
	}
	end = size
	if complete = b[0] == '\n'; complete {
		end--
	}
	buf := make([]byte, 32<<10)
	for pos := end; pos > 0; {
		n := min(int64(len(buf)), pos)
		pos -= n
		if _, err = r.ReadAt(buf[:n], pos); err != nil {
			return
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return pos + int64(i) + 1, end, complete, nil
		}
	}
	return 0, end, complete, nil
}

type chainFields struct {
	Seq    *int64  `json:"seq"`
	Prev   *string `json:"prev"`
	Method string  `json:"method"`
}

func (c chainFields) chained() bool { return c.Seq != nil && c.Prev != nil }

// AuditVerification is the result of VerifyAudit.
type AuditVerification struct {
	Records   int     // chained records
	Unchained int     // lines before the chain (written by an older longarmd)
	Starts    []int   // line numbers of chain-start records
	FirstSeq  int64   // seq of the first chained record
	LinksOut  bool    // the first line chains to a line not in this file (a sink copy, a rotated log)
	Partial   bool    // the last line has no newline (being written, or a crash)
	Broken    *string // the first break, "line L (seq S): reason"; nil = intact
}

// VerifyAudit checks the hash chain and seq continuity of an audit log.
// It can't notice a changed or removed last line, nor a log rewritten
// with a recomputed chain - the off-machine copies (sinks, the hub) can.
func VerifyAudit(r io.Reader) (AuditVerification, error) {
	var v AuditVerification
	br := bufio.NewReaderSize(r, 64<<10)
	prevHash, lastSeq, chained := "", int64(0), false
	var pending *string // a junk line, fine only before a chain-start (crash)
	broken := func(n int, seq int64, format string, args ...any) *string {
		s := fmt.Sprintf("line %d (seq %d): %s", n, seq, fmt.Sprintf(format, args...))
		return &s
	}
	for n := 1; ; n++ {
		line, err := br.ReadBytes('\n')
		if err == io.EOF {
			if len(line) > 0 {
				v.Partial = true
			}
			break
		}
		if err != nil {
			return v, err
		}
		line = line[:len(line)-1]
		sum := sha256.Sum256(line)
		h := hex.EncodeToString(sum[:])
		var c chainFields
		if json.Unmarshal(line, &c) != nil || !c.chained() {
			if chained {
				if pending != nil {
					v.Broken = pending
					return v, nil
				}
				pending = broken(n, 0, "not a chained record")
			} else {
				v.Unchained++
			}
			prevHash = h
			continue
		}
		seq := *c.Seq
		start := c.Method == AuditChainStart
		if pending != nil && !(start && *c.Prev == prevHash) {
			v.Broken = pending
			return v, nil
		}
		pending = nil
		switch {
		case *c.Prev == prevHash:
		case n == 1:
			v.LinksOut = true
		default:
			v.Broken = broken(n, seq, "prev is not the hash of line %d (edited, deleted or reordered lines)", n-1)
			return v, nil
		}
		switch {
		case start:
			if seq != 1 {
				v.Broken = broken(n, seq, "chain start with seq %d", seq)
				return v, nil
			}
			v.Starts = append(v.Starts, n)
		case !chained && n > 1:
			v.Broken = broken(n, seq, "chain begins without %s", AuditChainStart)
			return v, nil
		case chained && seq != lastSeq+1:
			v.Broken = broken(n, seq, "seq %d follows %d", seq, lastSeq)
			return v, nil
		}
		if !chained {
			v.FirstSeq = seq
		}
		chained, lastSeq, prevHash = true, seq, h
		v.Records++
	}
	v.Broken = pending
	return v, nil
}
