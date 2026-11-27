package screen

// Windows sessions for the remote desktop's session menu: a Remote
// Desktop (RDP) sign-in has a session of its own, which stays when its
// window is closed (disconnected); the SYSTEM agent shows the console
// session only, so another session is shown by moving it to the console
// (what `tscon <id> /dest:console` does).

// Session is a signed-in Windows session.
type Session struct {
	ID      uint32
	User    string // DOMAIN\user ("" = nobody: the sign-in screen)
	State   string // "active", "disconnected", ...
	Station string // "console", "RDP-Tcp#3", "" (disconnected)
	Console bool   // the one on the console (what the agent shows)
}

// Sessions lists this machine's user sessions and the console's
// (ErrUnsupported: not Windows).
func Sessions() ([]Session, error) { return sessions() }

// ConnectToConsole moves session id to the console; the session that was
// there is disconnected (signed in: it stays, like an RDP disconnect). An
// RDP client connected to id loses it. Needs SYSTEM.
func ConnectToConsole(id uint32) error { return connectToConsole(id) }

// ReattachCandidate is the session to move to the console when a viewer
// connects: only when nobody is signed in at the console and exactly one
// user session waits disconnected (with several, the viewer chooses).
func ReattachCandidate(ss []Session) (Session, bool) {
	var out []Session
	for _, s := range ss {
		if s.Console && s.User != "" {
			return Session{}, false // somebody is signed in there
		}
		if !s.Console && s.User != "" && s.State == "disconnected" {
			out = append(out, s)
		}
	}
	if len(out) != 1 {
		return Session{}, false
	}
	return out[0], true
}
