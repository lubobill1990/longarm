package diag

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/lubobill1990/longarm/internal/pii"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/trust"
	"github.com/lubobill1990/longarm/internal/version"
)

// Defaults of the collectors.
const (
	DefaultSince    = 24 * time.Hour
	DefaultLogCap   = 20 << 20 // all process logs together
	AuditCap        = 5 << 20
	JobOutputTail   = 256 << 10
	maxGunzip       = 256 << 20
	maxSmallFile    = 4 << 20 // config and state files
	maxCrashFile    = 1 << 20
	maxCrashFiles   = 20
	defaultJobLimit = 500
)

// ---------------------------------------------------------------- system

// SystemInfo is system.json: build, OS, uptime, memory and the free space
// of the given directories (label -> path).
func SystemInfo(component string, dirs map[string]string) map[string]any {
	m := map[string]any{
		"component": component, "version": version.Version, "go": runtime.Version(),
		"os": runtime.GOOS, "arch": runtime.GOARCH, "num_cpu": runtime.NumCPU(),
		"collected_at": time.Now().UTC().Format(time.RFC3339), "utc_offset": time.Now().Format("-07:00"),
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		build := map[string]string{"module": bi.Main.Version}
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision", "vcs.time", "vcs.modified", "GOAMD64", "CGO_ENABLED", "-tags":
				build[s.Key] = s.Value
			}
		}
		m["build"] = build
	}
	for k, v := range osInfo() {
		m[k] = v
	}
	if s, err := platform.SampleSystem(); err == nil {
		m["uptime"] = s.Uptime.Round(time.Second).String()
		m["mem_total"] = HumanBytes(int64(s.MemTotal))
		m["mem_available"] = HumanBytes(int64(s.MemAvail))
	} else {
		m["system_sample_error"] = err.Error()
	}
	disks := map[string]any{}
	for label, p := range dirs {
		if p == "" {
			continue
		}
		free, total, err := platform.DiskUsage(p)
		if err != nil {
			disks[label] = map[string]string{"error": err.Error()}
			continue
		}
		pct := 0.0
		if total > 0 {
			pct = float64(free) * 100 / float64(total)
		}
		disks[label] = map[string]any{"free": HumanBytes(int64(free)), "total": HumanBytes(int64(total)), "free_percent": fmt.Sprintf("%.1f", pct)}
	}
	if len(disks) > 0 {
		m["disk"] = disks
	}
	if exe, err := os.Executable(); err == nil {
		m["executable"] = exe
	}
	return m
}

// ---------------------------------------------------------------- logs

// LogSet collects process logs newest first within a shared byte budget.
type LogSet struct {
	B      *Builder
	Since  time.Duration
	Budget int64 // remaining bytes (DefaultLogCap)
	Now    time.Time
}

// rotatedRE: the suffixes of rotated copies (logrotate: .1, .2.gz,
// -20261026; ours: .1, .old).
var rotatedRE = regexp.MustCompile(`^(\.\d+|[.-]\d{8}[0-9T:-]*|\.old)?(\.gz)?$`)

// RotatedFiles returns path and its rotated copies, newest first.
func RotatedFiles(p string) []string {
	dir, base := filepath.Split(p)
	if dir == "" {
		dir = "."
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type cand struct {
		path string
		mod  time.Time
	}
	var cs []cand
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), base) || !rotatedRE.MatchString(e.Name()[len(base):]) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		cs = append(cs, cand{filepath.Join(dir, e.Name()), fi.ModTime()})
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].path == p || cs[j].path == p { // the live file first
			return cs[i].path == p
		}
		return cs[i].mod.After(cs[j].mod)
	})
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.path
	}
	return out
}

// Add collects the log at p and its rotated copies as <zipDir>/<file>.
func (l *LogSet) Add(p, zipDir, reason string) { l.AddAs(p, zipDir, filepath.Base(p), reason) }

// AddAs is Add with the zip name base instead of p's (a file name that
// may hold a name, like --log-file).
func (l *LogSet) AddAs(p, zipDir, base, reason string) {
	now := l.Now
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-l.Since)
	files := RotatedFiles(p)
	if len(files) == 0 {
		return
	}
	for _, f := range files {
		name := zipDir + "/" + base + strings.TrimSuffix(filepath.Base(f)[len(filepath.Base(p)):], ".gz")
		fi, err := os.Stat(f)
		if err != nil {
			l.B.Errorf("%s: %v", f, err)
			continue
		}
		if l.Since > 0 && fi.ModTime().Before(cutoff) {
			l.B.Exclude(name, fmt.Sprintf("last written %s, before --since %s", fi.ModTime().UTC().Format(time.RFC3339), l.Since))
			continue
		}
		if l.Budget < 4<<10 { // not even a few lines left
			l.B.Exclude(name, "log size cap reached (newer logs first)")
			continue
		}
		data, cut, err := readTail(f, l.Budget)
		if err != nil {
			l.B.Errorf("%s: %v", f, err)
			continue
		}
		var dropped int
		if l.Since > 0 {
			data, dropped = dropBefore(data, cutoff)
		}
		if len(data) == 0 {
			l.B.Exclude(name, "no lines within --since")
			continue
		}
		var head string
		if cut > 0 || dropped > 0 {
			head = fmt.Sprintf("[longarm diag: %d earlier bytes left out (size cap / --since %s)]\n", int64(cut)+int64(dropped), l.Since)
		}
		l.Budget -= int64(len(data))
		l.B.AddText(name, reason, append([]byte(head), data...))
	}
}

// readTail reads at most max bytes from the end of the file at p
// (decompressing .gz), starting at a line boundary. cut is the number of
// bytes before it.
func readTail(p string, max int64) (data []byte, cut int64, err error) {
	f, err := platform.OpenShared(p)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	if strings.HasSuffix(p, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, 0, err
		}
		all, err := io.ReadAll(io.LimitReader(zr, maxGunzip))
		if err != nil {
			return nil, 0, err
		}
		if int64(len(all)) > max {
			cut = int64(len(all)) - max
			all = all[cut:]
		}
		return trimPartial(all, cut)
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := fi.Size()
	if size > max {
		cut = size - max
	}
	if _, err := f.Seek(cut, io.SeekStart); err != nil {
		return nil, 0, err
	}
	data, err = io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return nil, 0, err
	}
	return trimPartial(data, cut)
}

func trimPartial(data []byte, cut int64) ([]byte, int64, error) {
	if cut > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			cut += int64(i + 1)
			data = data[i+1:]
		}
	}
	return data, cut, nil
}

// tsRE finds a timestamp near the start of a log line: slog text
// (time=2026-10-26T10:11:12.123+08:00), JSON ("time":"...") or a bare
// RFC 3339 prefix.
var tsRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)

func lineTime(line []byte) (time.Time, bool) {
	if len(line) > 160 {
		line = line[:160]
	}
	m := tsRE.Find(line)
	if m == nil {
		return time.Time{}, false
	}
	s := strings.Replace(string(m), " ", "T", 1)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999Z0700", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// dropBefore removes the lines before the first one stamped at or after
// cutoff (lines without a time stay with the line before them). Data
// without any timestamps is kept.
func dropBefore(data []byte, cutoff time.Time) ([]byte, int) {
	off := 0
	sawTime := false
	for off < len(data) {
		end := bytes.IndexByte(data[off:], '\n')
		line := data[off:]
		if end >= 0 {
			line = data[off : off+end]
		}
		if t, ok := lineTime(line); ok {
			sawTime = true
			if !t.Before(cutoff) {
				return data[off:], off
			}
		}
		if end < 0 {
			break
		}
		off += end + 1
	}
	if !sawTime {
		return data, 0
	}
	return nil, len(data)
}

// AddFileTail adds the last max bytes of a text file (job output, shim
// logs).
func (b *Builder) AddFileTail(p, name, reason string, max int64) {
	data, cut, err := readTail(p, max)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			b.Errorf("%s: %v", p, err)
		}
		return
	}
	if cut > 0 {
		data = append([]byte(fmt.Sprintf("[longarm diag: last %s; %d earlier bytes left out]\n", HumanBytes(max), cut)), data...)
	}
	b.AddText(name, reason, data)
}

// AddSmallFile adds a small text file as is (scrubbed); missing files are
// skipped silently.
func (b *Builder) AddSmallFile(p, name, reason string) {
	fi, err := os.Stat(p)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			b.Errorf("%s: %v", p, err)
		}
		return
	}
	if fi.Size() > maxSmallFile {
		b.AddFileTail(p, name, reason, maxSmallFile)
		return
	}
	data, err := platform.ReadFileShared(p)
	if err != nil {
		b.Errorf("%s: %v", p, err)
		return
	}
	b.AddText(name, reason, data)
}

// AddCrashFiles adds <dir>/crash-*.txt (written by internal/telemetry when
// a process panics), newest first.
func (b *Builder) AddCrashFiles(dir, zipDir string) {
	files := RotatedGlob(filepath.Join(dir, "crash-*.txt"))
	for i, f := range files {
		name := zipDir + "/" + filepath.Base(f)
		if i >= maxCrashFiles {
			b.Exclude(name, fmt.Sprintf("only the newest %d crash reports", maxCrashFiles))
			continue
		}
		b.AddFileTail(f, name, "crash report (panic) written by the process", maxCrashFile)
	}
}

// RotatedGlob returns the files matching pattern, newest first.
func RotatedGlob(pattern string) []string {
	m, _ := filepath.Glob(pattern)
	sort.Slice(m, func(i, j int) bool {
		a, _ := os.Stat(m[i])
		c, _ := os.Stat(m[j])
		if a == nil || c == nil {
			return m[i] > m[j]
		}
		return a.ModTime().After(c.ModTime())
	})
	return m
}

// ---------------------------------------------------------------- config

// secretKeyRE matches TOML keys whose values are secrets (or name where
// one is kept): their values are replaced by "<removed>".
var secretKeyRE = regexp.MustCompile(`(?i)(pass(word|wd|phrase)?|secret|token|api[_-]?key|private[_-]?key|access[_-]?key|account[_-]?key|credential|cookie|sas|signature|conn(ection)?[_-]?string|auth[_-]?key|instrumentation[_-]?key|webhook)`)

// RemovedValue replaces secret values in configuration files.
const RemovedValue = "<removed>"

// SanitizeTOML parses a TOML document and replaces the values of keys
// that hold secrets (passwords, tokens, keys, webhook URLs, ...) at any
// depth. It returns the re-encoded document and the number of values
// removed. Comments are not kept.
func SanitizeTOML(data []byte) ([]byte, int, error) {
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, 0, err
	}
	n := sanitizeValue(m)
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), n, nil
}

func sanitizeValue(v any) int {
	n := 0
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if secretKeyRE.MatchString(k) {
				if s, ok := val.(string); ok && s == "" {
					continue
				}
				t[k] = RemovedValue
				n++
				continue
			}
			n += sanitizeValue(val)
		}
	case []map[string]any:
		for _, e := range t {
			n += sanitizeValue(e)
		}
	case []any:
		for _, e := range t {
			n += sanitizeValue(e)
		}
	}
	return n
}

// AddTOML adds a TOML configuration file with its secret values removed.
// A file that doesn't parse is left out (its secrets can't be told apart).
func (b *Builder) AddTOML(p, name, reason string) {
	data, err := platform.ReadFileShared(p)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		b.Errorf("%s: %v", p, err)
		return
	}
	out, n, err := SanitizeTOML(data)
	if err != nil {
		b.Exclude(name, "not valid TOML, so secrets can't be removed reliably: "+oneLine(err.Error()))
		return
	}
	b.scrub["config_secret"] += n
	b.AddText(name, reason+" (secret values removed)", out)
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// ---------------------------------------------------------------- certificates

// CertInfo is the metadata of one certificate (never the key).
type CertInfo struct {
	File              string `json:"file"`
	Subject           string `json:"subject"`
	Identity          string `json:"identity,omitempty"`
	Role              string `json:"role,omitempty"`
	Name              string `json:"name,omitempty"` // pseudonym
	User              string `json:"user,omitempty"` // pseudonym
	IsCA              bool   `json:"is_ca,omitempty"`
	Serial            string `json:"serial"`
	Fingerprint       string `json:"fingerprint"`
	Issuer            string `json:"issuer"`
	IssuerFingerprint string `json:"issuer_fingerprint,omitempty"`
	NotBefore         string `json:"not_before"`
	NotAfter          string `json:"not_after"`
	ExpiresIn         string `json:"expires_in"`
	Expired           bool   `json:"expired,omitempty"`
	Trust             string `json:"trust,omitempty"`
	DNSNames          int    `json:"dns_names,omitempty"`
	IPAddresses       int    `json:"ip_addresses,omitempty"`
}

// keyFileRE are files that hold private keys or credentials: never read.
var keyFileRE = regexp.MustCompile(`(?i)(\.key|\.authkey|_key|token|secret|password|\.pfx|\.p12)$`)

// IsSecretFile reports whether a file name says it holds a key or a
// credential.
func IsSecretFile(name string) bool { return keyFileRE.MatchString(name) }

// AddCertDir adds certificate metadata for every certificate file in dir
// (*.crt, *.pem, *.cer) as name; key files are recorded as excluded.
func (b *Builder) AddCertDir(dir, label, name string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			b.Errorf("%s: %v", dir, err)
		}
		return
	}
	var certs []*x509.Certificate
	var files []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		fn := e.Name()
		if IsSecretFile(fn) {
			b.Exclude(label+"/"+fn, "private key / credential: never collected")
			continue
		}
		switch strings.ToLower(filepath.Ext(fn)) {
		case ".crt", ".pem", ".cer":
		default:
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, fn))
		if err != nil {
			b.Errorf("%s: %v", fn, err)
			continue
		}
		for rest := data; ; {
			var blk *pem.Block
			blk, rest = pem.Decode(rest)
			if blk == nil {
				break
			}
			if blk.Type != "CERTIFICATE" {
				continue // keys in a .pem are skipped, never copied
			}
			c, err := x509.ParseCertificate(blk.Bytes)
			if err != nil {
				b.Errorf("%s: %v", fn, err)
				continue
			}
			certs = append(certs, c)
			files = append(files, label+"/"+fn)
		}
	}
	if len(certs) == 0 {
		return
	}
	infos := make([]CertInfo, len(certs))
	for i, c := range certs {
		infos[i] = b.certInfo(c, files[i], certs)
	}
	b.AddJSON(name, "certificate metadata ("+label+"): identity, role, expiry, serial, issuer fingerprint, trust extension - no keys", infos)
}

func (b *Builder) certInfo(c *x509.Certificate, file string, pool []*x509.Certificate) CertInfo {
	ci := CertInfo{File: file, Subject: c.Subject.String(), IsCA: c.IsCA, Serial: pki.Serial(c), Fingerprint: pki.Fingerprint(c),
		Issuer: c.Issuer.String(), NotBefore: c.NotBefore.UTC().Format(time.RFC3339), NotAfter: c.NotAfter.UTC().Format(time.RFC3339),
		DNSNames: len(c.DNSNames), IPAddresses: len(c.IPAddresses)}
	left := time.Until(c.NotAfter)
	ci.Expired = left < 0
	ci.ExpiresIn = left.Round(time.Hour).String()
	if id, err := pki.IdentityFromCert(c); err == nil {
		ci.Role = string(id.Role)
		ci.Name = b.Scrub.Hash(pii.KindNode, id.Name)
		if id.User != "" {
			ci.User = b.Scrub.Hash(pii.KindUser, id.User)
		}
		ci.Identity = "longarm://" + ci.Role + "/" + ci.Name
		if ci.User != "" {
			ci.Identity += "?user=" + ci.User
		}
	}
	for _, p := range pool {
		if p.IsCA && p != c && c.CheckSignatureFrom(p) == nil {
			ci.IssuerFingerprint = pki.Fingerprint(p)
			break
		}
	}
	if !c.IsCA {
		switch te, err := pki.TrustOf(c); {
		case err != nil:
			ci.Trust = "invalid: " + oneLine(err.Error())
		case te == nil:
			ci.Trust = "none"
		case te.Endorsement == nil:
			ci.Trust = "node key bound, not endorsed (key " + pki.KeyID(te.NodeKey) + ")"
		default:
			e := te.Endorsement
			st := "endorsed"
			if err := e.Verify(); err != nil {
				st = "endorsement INVALID (" + oneLine(err.Error()) + ")"
			}
			ci.Trust = fmt.Sprintf("%s as %s %s of user %s by key %s on %s", st, e.Kind, b.Scrub.Hash(pii.KindNode, e.Name),
				b.Scrub.Hash(pii.KindUser, e.User), pki.KeyID(e.UserKey), time.Unix(e.Issued, 0).UTC().Format(time.RFC3339))
		}
	}
	return ci
}

// TrustSummary describes the end-to-end trust state in dir (no keys:
// whether they exist, their ids).
func (b *Builder) TrustSummary(dir string, forClient bool) map[string]any {
	st := trust.Store{Dir: dir}
	m := map[string]any{}
	if p, err := st.Policy(forClient); err != nil {
		m["policy_error"] = oneLine(err.Error())
	} else {
		m["policy"] = trust.Summary(p)
		m["active"] = p.Active()
	}
	if f, err := st.LoadPolicyFile(); err == nil {
		m["require_workers"] = f.RequireWorkers
		m["trusted_users"] = len(f.Users)
		m["denied_nodes"] = len(f.DenyNodes)
	}
	if e, err := st.Endorsement(); err != nil {
		m["endorsement_error"] = oneLine(err.Error())
	} else if e != nil {
		m["endorsement"] = map[string]any{"kind": e.Kind, "user": b.Scrub.Hash(pii.KindUser, e.User), "name": b.Scrub.Hash(pii.KindNode, e.Name),
			"user_key": pki.KeyID(e.UserKey), "issued": time.Unix(e.Issued, 0).UTC().Format(time.RFC3339), "valid": e.Verify() == nil}
	} else {
		m["endorsement"] = "none"
	}
	for _, f := range []string{trust.NodeKeyFile, trust.UserKeyFile} {
		_, err := os.Stat(filepath.Join(dir, f))
		m[strings.ReplaceAll(f, ".", "_")+"_present"] = err == nil
		if err == nil {
			b.Exclude("trust/"+f, "private key: never collected")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, trust.RenewFile)); err == nil {
		m["renewal_requested"] = true
	}
	return m
}

// TrustNames returns the user names of trust.toml in dir (for the
// scrubber).
func TrustNames(dir string) []string {
	f, err := trust.Store{Dir: dir}.LoadPolicyFile()
	if err != nil {
		return nil
	}
	var out []string
	for u := range f.Users {
		out = append(out, u)
	}
	return out
}

// ---------------------------------------------------------------- names

// ReadIdentity returns the identity of the certificate in tlsDir/node.crt
// and the DER of tlsDir/ca.crt (for pii.FleetSalt).
func ReadIdentity(tlsDir string) (pki.Identity, []byte) {
	var id pki.Identity
	var caDER []byte
	if b, err := os.ReadFile(filepath.Join(tlsDir, pki.NodeCertFile)); err == nil {
		if c, err := pki.ParseCertPEM(b); err == nil {
			id, _ = pki.IdentityFromCert(c)
		}
	}
	if b, err := os.ReadFile(filepath.Join(tlsDir, pki.CACertFile)); err == nil {
		if c, err := pki.ParseCertPEM(b); err == nil {
			caDER = c.Raw
		}
	}
	return id, caDER
}

// Names collects the names a scrubber should know.
type Names map[string]string

// Add records name as kind (empty names are ignored).
func (n Names) Add(kind string, names ...string) {
	for _, s := range names {
		if s = strings.TrimSpace(s); s != "" {
			n[s] = kind
		}
	}
}

// Scrubber builds the bundle's scrubber: the fleet salt (from the CA's
// DER; empty = the fixed salt) and the names, plus the local machine's.
func (n Names) Scrubber(caDER []byte) *pii.Scrubber {
	var salt []byte
	if len(caDER) > 0 {
		salt = pii.FleetSalt(caDER)
	}
	return pii.New(pii.Options{Salt: salt, Names: n})
}

// ReadLines reads a small text file's non-empty lines.
func ReadLines(p string) []string {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	return out
}
