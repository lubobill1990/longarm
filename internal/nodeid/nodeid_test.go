package nodeid

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`).MatchString

func TestNew(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := New()
		if !Valid(id) || id != strings.ToLower(id) || id[14] != '4' {
			t.Fatalf("bad id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate %s", id)
		}
		seen[id] = true
	}
}

func TestCheckName(t *testing.T) {
	ok := []string{"alice-pair", "alice-pc", "e2e-worker", "spike-1", "cpc-bob-x7k2h-admin", "bob-cpc-bob-x7k2h",
		"abc", "w-1234", "cafe-x", "a", "dead_beef", "g000", "1234.5"}
	for _, n := range ok {
		if err := CheckName(n, validName); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	bad := []string{"cafe", "1234", "dead-beef", "DEADBEEF", "0000-", "a-b-c-d", "3f2a9c1e-4b5d-4c3e-8f00-1234567890ab",
		"id:x", "name:x", "", "-x", "a b"}
	for _, n := range bad {
		if err := CheckName(n, validName); err == nil {
			t.Errorf("%q accepted", n)
		}
	}
	if got := SafeName("1234", "w-"); got != "w-1234" {
		t.Errorf("SafeName = %q", got)
	}
	if got := SafeName("box", "w-"); got != "box" {
		t.Errorf("SafeName = %q", got)
	}
	// rule 2 in both directions: every bare target that is a valid name
	// parses as a name, every one that is no name parses as an ID
	for _, s := range append(ok, bad...) {
		tg, err := ParseTarget(s)
		if CheckName(s, validName) == nil && (err != nil || tg.Mode != ByName) {
			t.Errorf("name %q parsed as %v (%v)", s, tg.Mode, err)
		}
		if LooksLikeID(s) && err == nil && tg.Mode == ByName {
			t.Errorf("id-like %q parsed as a name", s)
		}
	}
}

func TestParseTarget(t *testing.T) {
	for _, c := range []struct {
		in   string
		mode Mode
		val  string
		err  bool
	}{
		{"box", ByName, "box", false},
		{"3F2A9C1E-4b5d-4c3e-8f00-1234567890ab", ByID, "3f2a9c1e-4b5d-4c3e-8f00-1234567890ab", false},
		{"3f2a", ByIDPrefix, "3f2a", false},
		{"3f2a9c1e-4b", ByIDPrefix, "3f2a9c1e-4b", false},
		{"3f2", ByName, "3f2", false}, // too short to be a prefix: a name
		{"id:3f2a", ByIDPrefix, "3f2a", false},
		{"ID:3f2a9c1e-4b5d-4c3e-8f00-1234567890ab", ByID, "3f2a9c1e-4b5d-4c3e-8f00-1234567890ab", false},
		{"id:3f2", 0, "", true},
		{"id:box", 0, "", true},
		{"name:cafe", ByName, "cafe", false},
		{"name:", 0, "", true},
		{"", 0, "", true},
		{"a---", 0, "", true}, // looks like an id, too few hex digits
	} {
		got, err := ParseTarget(c.in)
		if (err != nil) != c.err {
			t.Errorf("%q: err %v", c.in, err)
			continue
		}
		if !c.err && (got.Mode != c.mode || got.Value != c.val) {
			t.Errorf("%q: got %v %q, want %v %q", c.in, got.Mode, got.Value, c.mode, c.val)
		}
	}
}

func TestResolve(t *testing.T) {
	cands := []Candidate{
		{ID: "3f2a9c1e-4b5d-4c3e-8f00-1234567890ab", Name: "box", Owner: "alice", State: "approved"},
		{ID: "3f2a9c1f-0000-4c3e-8f00-1234567890ab", Name: "laptop", Owner: "bob", State: "approved"},
		{ID: "77aa0000-0000-4000-8000-000000000001", Name: "box", Owner: "alice", State: "revoked"},
		{ID: "88bb0000-0000-4000-8000-000000000002", Name: "old", State: "revoked"},
		{ID: "99cc0000-0000-4000-8000-000000000003", Name: "old", State: "revoked"},
	}
	want := func(target, id string) {
		t.Helper()
		c, err := Resolve(target, cands, "worker")
		if err != nil || c.ID != id {
			t.Errorf("%q: got %s, %v; want %s", target, c.ID, err, id)
		}
	}
	want("box", cands[0].ID)                                     // the active one of a reused name
	want("3f2a9c1e", cands[0].ID)                                // unique prefix
	want("3f2a9c1f", cands[1].ID)                                //
	want("3F2A9C1E4B5D", cands[0].ID)                            // case and dashes don't matter
	want("77aa", cands[2].ID)                                    // revoked ones by id
	want("id:77aa0000-0000-4000-8000-000000000001", cands[2].ID) //
	want("name:laptop", cands[1].ID)                             //

	_, err := Resolve("3f2a", cands, "worker")
	var amb *AmbiguousError
	if !errors.As(err, &amb) || !errors.Is(err, ErrAmbiguous) || len(amb.Candidates) != 2 {
		t.Fatalf("3f2a: %v", err)
	}
	msg := err.Error()
	for _, s := range []string{"3f2a9c1e", "3f2a9c1f", "box", "laptop", "alice", "bob", "approved"} {
		if !strings.Contains(msg, s) {
			t.Errorf("ambiguity error lacks %q: %s", s, msg)
		}
	}
	if _, err := Resolve("old", cands, "worker"); !errors.Is(err, ErrAmbiguous) { // two revoked "old"s: never guess
		t.Errorf("old: %v", err)
	}
	if _, err := Resolve("nope", cands, "worker"); !errors.Is(err, ErrNotFound) {
		t.Errorf("nope: %v", err)
	}
	if _, err := Resolve("dead", cands, "worker"); !errors.Is(err, ErrNotFound) {
		t.Errorf("dead: %v", err)
	}
}

func TestShortIDs(t *testing.T) {
	ids := []string{
		"3f2a9c1e-4b5d-4c3e-8f00-1234567890ab",
		"3f2a9c1e-4b6d-4c3e-8f00-1234567890ab",
		"77aa0000-0000-4000-8000-000000000001",
	}
	s := ShortIDs(ids)
	if s[ids[2]] != "77aa0000" {
		t.Errorf("unique id: %q", s[ids[2]])
	}
	if s[ids[0]] != "3f2a9c1e-4b5" || s[ids[1]] != "3f2a9c1e-4b6" {
		t.Errorf("shared prefix: %q %q", s[ids[0]], s[ids[1]])
	}
	for _, id := range ids {
		c, err := Resolve(s[id], []Candidate{{ID: ids[0]}, {ID: ids[1]}, {ID: ids[2]}}, "node")
		if err != nil || c.ID != id {
			t.Errorf("short %q does not resolve to %s: %v", s[id], id, err)
		}
	}
}
