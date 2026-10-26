// Package pii removes personal and secret data from text that leaves a
// machine: telemetry (internal/telemetry) and diagnostic bundles
// (internal/diag). docs/telemetry.md lists what is replaced.
//
// Names that identify people or machines (OS user, host, Longarm node and
// user names) are not dropped but replaced by a keyed hash, the same within
// one fleet: "<host:3f2a9c>" in two reports is the same machine, so a bug
// can still be followed across nodes, but the name itself isn't sent.
// Secrets, e-mail addresses and IP addresses are replaced by a fixed tag.
//
// Scrubbing is best effort on free text: callers send as little free text
// as they can (no command lines, no job output) and scrub what remains.
package pii

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Kinds of names Hash and Options.Names know.
const (
	KindHost = "host"
	KindUser = "user" // OS or Longarm user
	KindNode = "node" // Longarm node (worker, client device)
)

// Options configure a Scrubber.
type Options struct {
	// Salt keys the name hashes; one per fleet (the hub hands it out) so
	// reports of the same fleet line up. Empty: a fixed salt.
	Salt []byte
	// Names maps a name to its kind ("host", "user", "node"): replaced by
	// "<kind:hash>" wherever it appears as a word.
	Names map[string]string
	// NoLocal leaves out the local machine's host name, OS user and home
	// directory (New adds them by default).
	NoLocal bool
}

// Scrubber replaces personal and secret data in text. Safe for concurrent
// use; a nil *Scrubber returns its input unchanged.
type Scrubber struct {
	salt  []byte
	names *regexp.Regexp // known names, longest first
	kinds map[string]string
	homes []string // home directories, longest first
	mu    sync.Mutex
	cache map[string]string
}

// New builds a Scrubber.
func New(o Options) *Scrubber {
	s := &Scrubber{salt: o.Salt, kinds: map[string]string{}, cache: map[string]string{}}
	if len(s.salt) == 0 {
		s.salt = []byte("longarm-pii-v1")
	}
	add := func(name, kind string) {
		name = strings.TrimSpace(name)
		if len(name) < 3 || commonWord[strings.ToLower(name)] {
			return // "a", "go", "root": would mangle unrelated text
		}
		s.kinds[strings.ToLower(name)] = kind
	}
	for n, k := range o.Names {
		add(n, k)
	}
	if !o.NoLocal {
		if h, err := os.Hostname(); err == nil {
			add(h, KindHost)
			if i := strings.IndexByte(h, '.'); i > 0 {
				add(h[:i], KindHost)
			}
		}
		if u, err := user.Current(); err == nil {
			name := u.Username
			if i := strings.LastIndexAny(name, `\/`); i >= 0 {
				add(name[:i], KindHost) // DOMAIN\user: the domain
				name = name[i+1:]
			}
			add(name, KindUser)
			if u.Name != "" {
				add(u.Name, KindUser)
			}
		}
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			s.homes = append(s.homes, h, filepath.ToSlash(h))
		}
	}
	sort.Slice(s.homes, func(i, j int) bool { return len(s.homes[i]) > len(s.homes[j]) })
	if len(s.kinds) > 0 {
		names := make([]string, 0, len(s.kinds))
		for n := range s.kinds {
			names = append(names, regexp.QuoteMeta(n))
		}
		sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
		// word-ish boundaries: a name inside a longer identifier stays
		s.names = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])(` + strings.Join(names, "|") + `)($|[^A-Za-z0-9_-])`)
	}
	return s
}

// commonWord are names too generic to replace (they occur in normal text).
var commonWord = map[string]bool{
	"root": true, "admin": true, "user": true, "users": true, "localhost": true, "worker": true,
	"client": true, "hub": true, "test": true, "home": true, "system": true, "default": true,
}

// Hash is the stable pseudonym of a name: "<kind:6 hex>".
func (s *Scrubber) Hash(kind, name string) string {
	if s == nil {
		return name
	}
	key := kind + "\x00" + strings.ToLower(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.cache[key]; ok {
		return h
	}
	m := hmac.New(sha256.New, s.salt)
	m.Write([]byte(key))
	h := "<" + kind + ":" + hex.EncodeToString(m.Sum(nil))[:6] + ">"
	s.cache[key] = h
	return h
}

// rule is one pattern of secret or personal data and its replacement.
type rule struct {
	re   *regexp.Regexp
	repl string
}

// rules run in order; secrets first (a token may contain an '@').
var rules = []rule{
	// key=value / "key": "value" pairs whose key says secret
	{regexp.MustCompile(`(?i)((?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|account[_-]?key|sharedaccesskey|client[_-]?secret|authorization|cookie|sig|instrumentationkey)["']?\s*[:=]\s*["']?)((?i:bearer|basic)\s+)?([^\s"'&;,]{3,})`), "${1}<secret>"},
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`), "${1} <secret>"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "<jwt>"},
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|tskey-[A-Za-z0-9-]{10,}|sk-[A-Za-z0-9_-]{20,}|xox[abpr]-[A-Za-z0-9-]{10,})`), "<secret>"},
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), "<private-key>"},
	// Longarm one-time join codes (H62-ZQV; the hub's alphabet has no I L O U)
	{regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{3}-[0-9A-HJKMNP-TV-Z]{3}\b`), "\x00code"},
	// URL query strings (SAS tokens, codes) and user info
	{regexp.MustCompile(`(https?://)[^/\s@]+@`), "${1}<userinfo>@"},
	{regexp.MustCompile(`(https?://[^\s?#"']+)\?[^\s"'#]*`), "${1}?<query>"},
	{regexp.MustCompile(`(?i)\b[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}\b`), "<email>"},
	// IPv4 (loopback and unspecified stay: they say "local")
	{regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`), "\x00ip4"},
	// IPv6 with at least three groups (not times like 12:30:45)
	{regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){2,7}[0-9a-f]{1,4}\b|\b(?:[0-9a-f]{1,4}:){1,6}:(?:[0-9a-f]{1,4})?\b`), "\x00ip6"},
}

// homeRE finds a user name in common home directory paths.
var homeRE = regexp.MustCompile(`(?i)([A-Z]:[\\/]+Users[\\/]+|/home/|/Users/)([^\\/\s"':]+)`)

// String scrubs s.
func (s *Scrubber) String(in string) string {
	if s == nil || in == "" {
		return in
	}
	out := in
	for _, h := range s.homes {
		out = strings.ReplaceAll(out, h, "~")
	}
	out = homeRE.ReplaceAllStringFunc(out, func(m string) string {
		sm := homeRE.FindStringSubmatch(m)
		if strings.HasPrefix(sm[2], "<") {
			return m
		}
		return sm[1] + s.Hash(KindUser, sm[2])
	})
	for _, r := range rules {
		switch r.repl {
		case "\x00ip4":
			out = r.re.ReplaceAllStringFunc(out, func(ip string) string {
				if strings.HasPrefix(ip, "127.") || ip == "0.0.0.0" {
					return ip
				}
				return "<ip>"
			})
		case "\x00code":
			out = r.re.ReplaceAllStringFunc(out, func(c string) string {
				// SHA-256, AES-128: letters then digits is a name, not a code
				if strings.Trim(c[:3], "ABCDEFGHJKMNPQRSTVWXYZ") == "" && strings.Trim(c[4:], "0123456789") == "" {
					return c
				}
				return "<join-code>"
			})
		case "\x00ip6":
			out = r.re.ReplaceAllStringFunc(out, func(ip string) string {
				if ip == "::1" || ip == "::" || !strings.Contains(ip, "::") && strings.Count(ip, ":") < 3 {
					return ip
				}
				return "<ip>"
			})
		default:
			out = r.re.ReplaceAllString(out, r.repl)
		}
	}
	if s.names != nil {
		// the pattern consumes a delimiter on each side: repeat until
		// adjacent names ("a b") are both replaced
		for range 3 {
			next := s.names.ReplaceAllStringFunc(out, func(m string) string {
				sm := s.names.FindStringSubmatch(m)
				return sm[1] + s.Hash(s.kinds[strings.ToLower(sm[2])], sm[2]) + sm[3]
			})
			if next == out {
				break
			}
			out = next
		}
	}
	return out
}

// Bytes scrubs b.
func (s *Scrubber) Bytes(b []byte) []byte {
	if s == nil {
		return b
	}
	return []byte(s.String(string(b)))
}

// Map scrubs the values of m in place (property bags of telemetry events).
func (s *Scrubber) Map(m map[string]string) map[string]string {
	if s == nil {
		return m
	}
	for k, v := range m {
		m[k] = s.String(v)
	}
	return m
}
