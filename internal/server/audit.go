package server

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
)

// maxAuditRecord caps one audit line (bytes).
const maxAuditRecord = 64 << 10

// Audit appends one JSON line per security-relevant call to a local file
// (DESIGN §6.4: who, where, what command, exit code). Never logs secret
// values - only env names.
type Audit struct {
	mu   sync.Mutex
	path string
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
	rec := map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "who": who, "method": method}
	for k, v := range fields {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	if len(b) > maxAuditRecord { // e.g. a huge argv: keep lines bounded for readers and the hub
		b, _ = json.Marshal(map[string]any{"time": rec["time"], "who": who, "method": method, "truncated": len(b),
			"head": string(b[:4096])})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}
