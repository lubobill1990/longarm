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
		meta skuid 1001 ct mark set ct mark or 0x03e90000 return # la-alice
		meta skuid 1002 ct mark set ct mark or 0x03ea0000 return # la-bob
	}
	chain answer {
		ct mark and 0xffff0000 == 0x03e90000 meta skuid 1001 return # la-alice to itself
		ct mark and 0xffff0000 == 0x03ea0000 meta skuid 1002 return # la-bob to itself
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
	// uid 2299 is tag 0x08fb0000, whoever else is in the list
	if !strings.Contains(got, "meta skuid 2299 ct mark set ct mark or 0x08fb0000 return") {
		t.Fatalf("tag of uid 2299 missing")
	}
	// Stable: purging and adding accounts does not move anybody's tag.
	got, err = Ruleset([]User{{"la-new", 1500}, {"la-u", 2299}})
	if err != nil || !strings.Contains(got, "meta skuid 2299 ct mark set ct mark or 0x08fb0000 return") ||
		!strings.Contains(got, "meta skuid 1500 ct mark set ct mark or 0x05dc0000 return") {
		t.Fatalf("tags moved: %v\n%s", err, got)
	}
	if _, err := Ruleset(make([]User, 70000)); err != nil {
		t.Fatalf("uid 0 entries are dropped: %v", err)
	}
	// Uids beyond 16 bits: their low 16 bits, unless two collide or the
	// low bits are 0 / 0xffff.
	got, err = Ruleset([]User{{"la-a", 100001}, {"la-b", 100002}})
	if err != nil || !strings.Contains(got, "meta skuid 100001 ct mark set ct mark or 0x86a10000 return") {
		t.Fatalf("large uids: %v\n%s", err, got)
	}
	for _, us := range [][]User{
		{{"la-a", 1001}, {"la-b", 1001 + 65536}},
		{{"la-a", 65536}},
		{{"la-a", 65535}},
		{{"la-a", 131071}},
	} {
		if _, err := Ruleset(us); err == nil {
			t.Errorf("%v accepted", us)
		}
	}
	for _, uid := range []int{1, 1000, 65534, 70000} {
		if g := Tag(uid); g == markBlocked || g&markKeep != 0 || g == 0 {
			t.Errorf("tag of %d: %#x", uid, g)
		}
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
