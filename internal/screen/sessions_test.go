package screen

import "testing"

// TestReattachCandidate: a viewer gets the one disconnected session only
// when nobody is signed in at the console.
func TestReattachCandidate(t *testing.T) {
	signIn := Session{ID: 1, State: "connected", Station: "Console", Console: true}
	mine := Session{ID: 2, User: `CPC\alice`, State: "disconnected"}
	other := Session{ID: 3, User: `CPC\bob`, State: "disconnected"}
	rdp := Session{ID: 4, User: `CPC\carol`, State: "active", Station: "RDP-Tcp#0"}
	for _, c := range []struct {
		name string
		ss   []Session
		want uint32 // 0: none
	}{
		{"one waiting", []Session{signIn, mine}, 2},
		{"one waiting, one connected over RDP", []Session{signIn, mine, rdp}, 2},
		{"two waiting: the viewer chooses", []Session{signIn, mine, other}, 0},
		{"somebody at the console", []Session{{ID: 1, User: `CPC\dan`, State: "active", Console: true}, mine}, 0},
		{"nothing waiting", []Session{signIn, rdp}, 0},
	} {
		s, ok := ReattachCandidate(c.ss)
		if got := map[bool]uint32{true: s.ID}[ok]; got != c.want {
			t.Errorf("%s: %d (%v), want %d", c.name, s.ID, ok, c.want)
		}
	}
}
