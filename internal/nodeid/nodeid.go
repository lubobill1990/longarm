// Package nodeid is the identity of Longarm nodes (docs/security.md "Node
// identity"): every node - worker, admin worker, client device, admin
// device, release certificate holder - has a unique, immutable ID, a random
// UUID the hub assigns, and a display name that can change and, once the
// node is revoked, be reused.
//
// The rules that keep a target string from ever meaning two things:
//
//  1. Names are unique among the hub's non-revoked nodes.
//  2. A name never looks like an ID: names made of hex digits and dashes
//     only, 4 characters or more (LooksLikeID), are refused, and so is
//     anything that parses as a UUID. Names can't contain ':' (pki.ValidName),
//     so the explicit forms "id:..." and "name:..." can't be names either.
//
// Resolve turns a target into one node: an explicit "id:<prefix>" or
// "name:<name>"; else a full UUID; else a string that looks like an ID is a
// git-style ID prefix (at least 4 hex digits, unique among the candidates);
// else it is a name. Ambiguity is an error listing the candidates; nothing
// is ever guessed.
package nodeid

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// MinPrefix is the shortest ID prefix a target may use (git-style).
const MinPrefix = 4

// MinShort is the shortest ID prefix shown next to names (Short, ShortIDs).
const MinShort = 8

// New returns a random (version 4) UUID in canonical lower-case form.
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("nodeid: no randomness: " + err.Error())
	}
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Valid reports whether s is a UUID in canonical form (any case).
func Valid(s string) bool { return uuidRE.MatchString(s) }

// Canonical lower-cases a valid UUID ("" if s is not one).
func Canonical(s string) string {
	if !Valid(s) {
		return ""
	}
	return strings.ToLower(s)
}

var idLikeRE = regexp.MustCompile(`^[0-9a-fA-F-]{4,}$`)

// LooksLikeID reports whether s could be read as an ID or an ID prefix:
// hex digits and dashes only, 4 characters or more. Such strings are never
// names (rule 2) and, as targets, always mean an ID.
func LooksLikeID(s string) bool { return idLikeRE.MatchString(s) }

// Explicit target forms.
const (
	IDPrefixForm   = "id:"
	NamePrefixForm = "name:"
)

// CheckName returns why name can't be a node's name: rule 2 on top of the
// syntax check valid (pki.ValidName, passed in to keep this package free
// of pki).
func CheckName(name string, valid func(string) bool) error {
	if valid != nil && !valid(name) {
		return fmt.Errorf("invalid name %q (letters, digits, . _ -, at most 63, starting with a letter or digit)", name)
	}
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, IDPrefixForm) || strings.HasPrefix(lower, NamePrefixForm) {
		return fmt.Errorf("invalid name %q: %q and %q are reserved for targets", name, IDPrefixForm, NamePrefixForm)
	}
	if Valid(name) || LooksLikeID(name) {
		return fmt.Errorf("invalid name %q: a name made of hex digits and dashes only looks like a node ID (add a letter, e.g. w-%s)", name, name)
	}
	return nil
}

// SafeName makes an automatically chosen name (from a host name) usable:
// one that looks like an ID gets prefix (e.g. "w-"), so a machine called
// "1234" becomes worker "w-1234".
func SafeName(name, prefix string) string {
	if name != "" && (Valid(name) || LooksLikeID(name)) {
		return prefix + name
	}
	return name
}

// hex32 is an ID or prefix without dashes, lower case.
func hex32(s string) string { return strings.ToLower(strings.ReplaceAll(s, "-", "")) }

// HasPrefix reports whether id starts with prefix, dashes ignored.
func HasPrefix(id, prefix string) bool {
	p := hex32(prefix)
	return p != "" && strings.HasPrefix(hex32(id), p)
}

// Short is id's first MinShort hex digits (no uniqueness check; ShortIDs
// makes them unique within a set).
func Short(id string) string {
	if len(id) < MinShort {
		return id
	}
	return id[:MinShort]
}

// ShortIDs maps each id to its shortest prefix of at least MinShort
// characters that no other id in ids shares (git-style abbreviation).
func ShortIDs(ids []string) map[string]string {
	sorted := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			sorted = append(sorted, strings.ToLower(id))
		}
	}
	sort.Strings(sorted)
	common := func(a, b string) int {
		n := 0
		for n < len(a) && n < len(b) && a[n] == b[n] {
			n++
		}
		return n
	}
	out := make(map[string]string, len(sorted))
	for i, id := range sorted {
		n := MinShort
		if i > 0 {
			n = max(n, common(id, sorted[i-1])+1)
		}
		if i+1 < len(sorted) {
			n = max(n, common(id, sorted[i+1])+1)
		}
		if n < len(id) && id[n-1] == '-' { // never end on a dash
			n++
		}
		out[id] = id[:min(n, len(id))]
	}
	for _, id := range ids { // keep the callers' case
		if s, ok := out[strings.ToLower(id)]; ok {
			out[id] = s
		}
	}
	return out
}

// Candidate is a node a target may resolve to.
type Candidate struct {
	ID    string
	Name  string
	Owner string // the owning user (workers) or the certificate's user (devices); "" = fleet
	State string // approved | pending | revoked
}

func (c Candidate) revoked() bool { return c.State == "revoked" }

// Mode is how a target was read.
type Mode int

const (
	ByName     Mode = iota // an exact name
	ByID                   // a full UUID
	ByIDPrefix             // a git-style ID prefix
)

func (m Mode) String() string {
	switch m {
	case ByID:
		return "id"
	case ByIDPrefix:
		return "id prefix"
	}
	return "name"
}

// Target is a parsed target string.
type Target struct {
	Mode  Mode
	Value string // the name, the canonical ID, or the prefix (lower case)
	Raw   string
}

// ErrNotFound: no candidate matches (errors.Is).
var ErrNotFound = errors.New("not found")

// ErrAmbiguous: several candidates match (errors.Is).
var ErrAmbiguous = errors.New("ambiguous")

// ParseTarget reads a target string (see the package comment).
func ParseTarget(s string) (Target, error) {
	raw := s
	s = strings.TrimSpace(s)
	if s == "" {
		return Target{}, errors.New("empty target")
	}
	switch lower := strings.ToLower(s); {
	case strings.HasPrefix(lower, IDPrefixForm):
		return parseID(strings.TrimSpace(s[len(IDPrefixForm):]), raw)
	case strings.HasPrefix(lower, NamePrefixForm):
		n := strings.TrimSpace(s[len(NamePrefixForm):])
		if n == "" {
			return Target{}, errors.New("empty name in " + raw)
		}
		return Target{Mode: ByName, Value: n, Raw: raw}, nil
	}
	if Valid(s) || LooksLikeID(s) {
		return parseID(s, raw)
	}
	return Target{Mode: ByName, Value: s, Raw: raw}, nil
}

func parseID(s, raw string) (Target, error) {
	if Valid(s) {
		return Target{Mode: ByID, Value: strings.ToLower(s), Raw: raw}, nil
	}
	if !LooksLikeID(s) && s != "" && strings.Trim(s, "0123456789abcdefABCDEF-") != "" {
		return Target{}, fmt.Errorf("%q is not a node ID or ID prefix (hex digits)", raw)
	}
	if h := hex32(s); len(h) < MinPrefix || len(h) > 32 {
		return Target{}, fmt.Errorf("an ID prefix needs %d to 32 hex digits, %q has %d", MinPrefix, raw, len(h))
	}
	return Target{Mode: ByIDPrefix, Value: strings.ToLower(s), Raw: raw}, nil
}

// Matches reports whether c matches t (by name, ID or prefix).
func (t Target) Matches(c Candidate) bool {
	switch t.Mode {
	case ByID:
		return strings.EqualFold(c.ID, t.Value)
	case ByIDPrefix:
		return HasPrefix(c.ID, t.Value)
	}
	return c.Name == t.Value
}

// Resolve picks the one candidate target means; what names the kind of
// node in messages ("worker", "node"). By name, a non-revoked node wins (a
// name is unique among those); with none, a single revoked node of that
// name is returned (the caller decides what a revoked node means). An ID
// or prefix may match revoked nodes too; several matches are ambiguous.
func Resolve(target string, cands []Candidate, what string) (Candidate, error) {
	t, err := ParseTarget(target)
	if err != nil {
		return Candidate{}, err
	}
	return t.Resolve(cands, what)
}

// Resolve is the package-level Resolve for a parsed target.
func (t Target) Resolve(cands []Candidate, what string) (Candidate, error) {
	if what == "" {
		what = "node"
	}
	var hits []Candidate
	for _, c := range cands {
		if t.Matches(c) {
			hits = append(hits, c)
		}
	}
	if t.Mode == ByName {
		var active []Candidate
		for _, c := range hits {
			if !c.revoked() {
				active = append(active, c)
			}
		}
		if len(active) > 0 {
			hits = active
		}
	}
	switch len(hits) {
	case 0:
		return Candidate{}, &NotFoundError{What: what, Target: t}
	case 1:
		return hits[0], nil
	}
	return Candidate{}, &AmbiguousError{What: what, Target: t, Candidates: hits}
}

// NotFoundError is Resolve's "no such node".
type NotFoundError struct {
	What   string
	Target Target
}

func (e *NotFoundError) Error() string {
	if e.Target.Mode == ByName {
		return fmt.Sprintf("no %s %q", e.What, e.Target.Value)
	}
	return fmt.Sprintf("no %s with %s %q", e.What, e.Target.Mode, e.Target.Value)
}

func (e *NotFoundError) Is(err error) bool { return err == ErrNotFound }

// AmbiguousError lists the candidates a target matched.
type AmbiguousError struct {
	What       string
	Target     Target
	Candidates []Candidate
}

func (e *AmbiguousError) Is(err error) bool { return err == ErrAmbiguous }

func (e *AmbiguousError) Error() string {
	ids := make([]string, 0, len(e.Candidates))
	for _, c := range e.Candidates {
		ids = append(ids, c.ID)
	}
	short := ShortIDs(ids)
	cs := append([]Candidate(nil), e.Candidates...)
	sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
	var b strings.Builder
	fmt.Fprintf(&b, "%s %q is ambiguous: it matches %d %ss (use more of the ID, or id:<prefix> / name:<name>):", e.Target.Mode, e.Target.Raw, len(cs), e.What)
	for _, c := range cs {
		owner := c.Owner
		if owner == "" {
			owner = "-"
		}
		fmt.Fprintf(&b, "\n  %s  %s  owner %s  %s", short[c.ID], c.Name, owner, c.State)
	}
	return b.String()
}
