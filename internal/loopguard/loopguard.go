// Package loopguard keeps the per-user isolation accounts (la-<user>) of a
// Linux worker from talking to each other over loopback (docs/security.md
// "Loopback between users").
//
// It installs one nftables table, inet longarm, owned by longarmd and
// replaced atomically whenever the set of accounts changes. The rules work
// on uids only - no cgroups, no lists of listening ports:
//
//   - On lo, the first packet of every connection (conntrack "new", and every
//     TCP SYN) records its sender in bits 16-31 of the conntrack mark: the
//     tag of the sending account (meta skuid of the socket; the low 16 bits
//     of its uid, see Tag), 0 for every other uid (root, system services,
//     people's own accounts).
//   - The listener's answer - the TCP SYN-ACK, which the kernel attributes to
//     the listening socket (also a SYN-cookie or retransmitted one: the skb
//     carries the listener or its request socket, skb_to_full_sk), and every
//     UDP reply - is checked in the output hook: if an account opened the
//     connection and the answering socket belongs to another account, or
//     the answer has no full socket the rule can attribute, the answer is
//     dropped and the connection marked blocked.
//   - The client's retransmitted SYN of a blocked connection is answered with
//     a TCP reset: connect() fails with "connection refused" after about a
//     second instead of hanging.
//
// Connections between processes of the same account, from or to non-account
// processes (root: longarmd's own forwards, system services) and everything
// not on lo are untouched. A UDP datagram that opens a "connection" is not
// an answer: it reaches another account's socket; only that socket's
// replies are dropped (the output hook only knows the sender's socket).
//
// The tags are stable: an account keeps its tag while accounts come and go,
// so conntrack entries of long-lived connections never point at another
// account after the table is replaced.
package loopguard

import (
	"fmt"
	"sort"
	"strings"
)

// Modes of [isolation] loopback.
const (
	// Isolate installs the rules; if that fails longarmd warns and runs on.
	Isolate = "isolate"
	// Require installs the rules or refuses to start.
	Require = "require"
	// Allow installs nothing (and removes a table left from before).
	Allow = "allow"
)

// Table is the nftables table longarmd owns (family inet: IPv4 and IPv6).
const Table = "longarm"

// Conntrack mark layout: bits 16-31 carry the sender; the low 16 bits are
// left to others (e.g. wg-quick's fwmark copies).
const (
	markMask    = 0xffff0000
	markKeep    = 0x0000ffff
	markBlocked = 0xffff0000
	markShift   = 16
)

// ParseMode validates an [isolation] loopback value ("" = default).
func ParseMode(s string) (string, error) {
	switch s {
	case "", Isolate, Require, Allow:
		return s, nil
	}
	return "", fmt.Errorf("[isolation] loopback %q: want %q, %q or %q", s, Isolate, Require, Allow)
}

// User is an isolation account.
type User struct {
	Name string
	UID  int
}

// Tag is the sender tag of uid in the conntrack mark (bits 16-31): the low
// 16 bits of the uid. It depends on the uid alone, so it never changes
// while the account exists, whichever accounts are added or purged (a
// position in the account list would shift, and conntrack entries of
// running connections would then name another account). Account uids
// below 65535 - useradd's usual range - are their own tag. Two accounts
// whose uids agree in the low 16 bits, or a uid whose low 16 bits are 0
// or 0xffff (0 = "not an account", 0xffff = blocked), cannot be told apart:
// Ruleset refuses them.
func Tag(uid int) uint32 { return uint32(uid&0xffff) << markShift }

// normalize sorts users by uid and drops duplicates and root.
func normalize(users []User) []User {
	out := make([]User, 0, len(users))
	seen := map[int]bool{}
	for _, u := range users {
		if u.UID <= 0 || seen[u.UID] {
			continue
		}
		seen[u.UID] = true
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// checkTags fails when two accounts would share a tag or one has an
// unusable one.
func checkTags(users []User) error {
	byTag := map[uint32]User{}
	for _, u := range users {
		t := Tag(u.UID)
		if t == 0 || t == markBlocked {
			return fmt.Errorf("loopback isolation: account %s has uid %d, whose low 16 bits (%#x) cannot tag connections; give it another uid", u.Name, u.UID, u.UID&0xffff)
		}
		if o, ok := byTag[t]; ok {
			return fmt.Errorf("loopback isolation: accounts %s (uid %d) and %s (uid %d) agree in the low 16 bits of their uids and cannot be told apart; give one another uid", o.Name, o.UID, u.Name, u.UID)
		}
		byTag[t] = u
	}
	return nil
}

// Ruleset returns the nft script that (re)creates the table for users in
// one transaction (nft -f): idempotent, whether the table exists or not.
func Ruleset(users []User) (string, error) {
	users = normalize(users)
	if err := checkTags(users); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(RemoveScript())
	fmt.Fprintf(&b, "table inet %s {\n", Table)
	b.WriteString("\t# Longarm per-user isolation: no loopback connections between different\n" +
		"\t# accounts (docs/security.md). Managed by longarmd - do not edit.\n")
	b.WriteString("\tchain output {\n\t\ttype filter hook output priority 0; policy accept;\n")
	if len(users) == 0 {
		b.WriteString("\t}\n}\n")
		return b.String(), nil
	}
	uids := make([]string, len(users))
	for i, u := range users {
		uids[i] = fmt.Sprint(u.UID)
	}
	set := "{ " + strings.Join(uids, ", ") + " }"
	b.WriteString("\t\toif != \"lo\" return\n")
	// A blocked connection's retransmitted SYN: refuse it at once.
	fmt.Fprintf(&b, "\t\tct direction original ct mark and 0x%08x == 0x%08x tcp flags & (syn | ack) == syn reject with tcp reset\n",
		uint32(markMask), uint32(markBlocked))
	// An account's answer to a connection conntrack could not follow: the
	// sender is unknown, fail closed.
	fmt.Fprintf(&b, "\t\tct state invalid tcp flags & (syn | ack) == syn | ack meta skuid %s counter drop\n", set)
	b.WriteString("\t\tct state new jump sender\n")
	b.WriteString("\t\tct direction original tcp flags & (syn | ack) == syn jump sender\n")
	fmt.Fprintf(&b, "\t\tct direction reply ct mark and 0x%08x != 0x00000000 tcp flags & (syn | ack) == syn | ack jump answer\n", uint32(markMask))
	fmt.Fprintf(&b, "\t\tct direction reply ct mark and 0x%08x != 0x00000000 meta l4proto udp jump answer\n", uint32(markMask))
	b.WriteString("\t}\n")

	// sender: record which account opened the connection (0 = none).
	b.WriteString("\tchain sender {\n")
	fmt.Fprintf(&b, "\t\tct mark set ct mark and 0x%08x\n", uint32(markKeep))
	for _, u := range users {
		fmt.Fprintf(&b, "\t\tmeta skuid %d ct mark set ct mark or 0x%08x return # %s\n", u.UID, Tag(u.UID), comment(u.Name))
	}
	b.WriteString("\t}\n")

	// answer: the listener's answer to a connection an account opened.
	b.WriteString("\tchain answer {\n")
	for _, u := range users {
		fmt.Fprintf(&b, "\t\tct mark and 0x%08x == 0x%08x meta skuid %d return # %s to itself\n", uint32(markMask), Tag(u.UID), u.UID, comment(u.Name))
	}
	// Not an account's socket: root, system services, other people. An
	// answer without a full socket (meta skuid cannot match: rare, as
	// SYN-ACKs - SYN cookies included - carry the listener) matches neither
	// rule and is dropped: fail closed.
	fmt.Fprintf(&b, "\t\tmeta skuid != %s return\n", set)
	fmt.Fprintf(&b, "\t\tct mark set ct mark or 0x%08x counter drop\n", uint32(markBlocked))
	b.WriteString("\t}\n}\n")
	return b.String(), nil
}

// comment makes s safe as an nft line comment.
func comment(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// RemoveScript deletes the table if it exists (nft -f; a no-op otherwise).
func RemoveScript() string {
	return fmt.Sprintf("add table inet %s\ndelete table inet %s\n", Table, Table)
}
