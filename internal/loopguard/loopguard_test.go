package loopguard

import (
	"strings"
	"testing"
)

func TestRulesetGolden(t *testing.T) {
	got, err := Ruleset([]User{{"la-bob", 1002}, {"la-alice", 1001}, {"la-alice-again", 1001}, {"root", 0}})
	if err != nil {
		t.Fatal(err)
	}
	want := `add table inet longarm
delete table inet longarm
table inet longarm {
	# Longarm per-user isolation: no loopback connections between different
	# accounts (docs/security.md). Managed by longarmd - do not edit.
	chain output {
		type filter hook output priority 0; policy accept;
		oif != "lo" return
		ct direction original ct mark and 0xffff0000 == 0xffff0000 tcp flags & (syn | ack) == syn reject with tcp reset
		ct state invalid tcp flags & (syn | ack) == syn | ack meta skuid { 1001, 1002 } counter drop
		ct state new jump sender
		ct direction original tcp flags & (syn | ack) == syn jump sender
		ct direction reply ct mark and 0xffff0000 != 0x00000000 tcp flags & (syn | ack) == syn | ack jump answer
		ct direction reply ct mark and 0xffff0000 != 0x00000000 meta l4proto udp jump answer
	}
	chain sender {
		ct mark set ct mark and 0x0000ffff
		meta skuid 1001 ct mark set ct mark or 0x00010000 return # la-alice
		meta skuid 1002 ct mark set ct mark or 0x00020000 return # la-bob
	}
	chain answer {
		ct mark and 0xffff0000 == 0x00010000 meta skuid 1001 return # la-alice to itself
		ct mark and 0xffff0000 == 0x00020000 meta skuid 1002 return # la-bob to itself
		meta skuid != { 1001, 1002 } return
		ct mark set ct mark or 0xffff0000 counter drop
	}
}
`
	if got != want {
		t.Fatalf("ruleset:\n%s\nwant:\n%s", got, want)
	}
}

func TestRulesetNoUsers(t *testing.T) {
	got, err := Ruleset(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, RemoveScript()) || strings.Contains(got, "skuid") || strings.Contains(got, "drop") ||
		!strings.Contains(got, "hook output") {
		t.Fatalf("empty ruleset:\n%s", got)
	}
}

func TestRulesetTags(t *testing.T) {
	var users []User
	for i := 0; i < 300; i++ {
		users = append(users, User{"la-u", 2000 + i})
	}
	got, err := Ruleset(users)
	if err != nil {
		t.Fatal(err)
	}
	// index 300 (uid 2299) is tag 0x012c0000; no tag equals the blocked mark
	if !strings.Contains(got, "meta skuid 2299 ct mark set ct mark or 0x012c0000 return") {
		t.Fatalf("tag of the 300th account missing")
	}
	if tag(MaxUsers-1) == markBlocked || tag(MaxUsers-1)&markKeep != 0 {
		t.Fatalf("the last tag %#x collides", tag(MaxUsers-1))
	}
	if _, err := Ruleset(make([]User, MaxUsers+1)); err != nil {
		t.Fatalf("uid 0 entries are dropped, not counted: %v", err)
	}
	many := make([]User, MaxUsers+1)
	for i := range many {
		many[i] = User{"x", 1000 + i}
	}
	if _, err := Ruleset(many); err == nil {
		t.Fatal("too many accounts accepted")
	}
}

func TestCommentSanitized(t *testing.T) {
	got, _ := Ruleset([]User{{"la-x\ndelete table inet filter", 1500}})
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "delete table inet filter") {
			t.Fatalf("name broke out of its comment:\n%s", got)
		}
	}
}

func TestParseMode(t *testing.T) {
	for _, ok := range []string{"", Isolate, Require, Allow} {
		if _, err := ParseMode(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	if _, err := ParseMode("on"); err == nil {
		t.Error("\"on\" accepted")
	}
}
