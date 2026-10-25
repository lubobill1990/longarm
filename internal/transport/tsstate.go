package transport

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tailscale.com/tsnet"
)

// Each control server gets its own state dir next to the base dir
// ("tsnet" -> "tsnet-controlplane.tailscale.com", "tsnet-hs.example.com"):
// a node whose hub switches tailnet providers keeps the old identity, so
// switching back needs no new key while that state is still valid. A
// tailscale listener without a control URL (Tailscale's default, set up by
// hand with TS_AUTHKEY) keeps the base dir.

// StateDirFor is the state dir of control server control under base.
func StateDirFor(base, control string) string {
	if control == "" {
		return base
	}
	return base + "-" + controlSlug(control)
}

// controlSlug turns a control URL into a file name part: its host (and a
// non-default port), e.g. "hs.example.com" or "127.0.0.1-8080".
func controlSlug(control string) string {
	host := control
	if u, err := url.Parse(control); err == nil && u.Host != "" {
		host = u.Host
		if p := strings.Trim(u.Path, "/"); p != "" {
			host += "-" + p
		}
	}
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), ".-")
	if s == "" {
		s = "control"
	}
	return s
}

// SameControl reports whether two control URLs name the same server
// (scheme and host case-insensitive, trailing slash ignored).
func SameControl(a, b string) bool {
	return normControl(a) == normControl(b)
}

func normControl(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return strings.ToLower(s)
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	return u.String()
}

// MigrateStateDir moves a node's state from the base dir (Longarm before
// per-control dirs, where the hub's Headscale state lived in "tsnet") to
// the dir of control, if that one doesn't exist yet; the waiting key file
// moves along.
func MigrateStateDir(base, control string) {
	dst := StateDirFor(base, control)
	if control == "" || dst == base {
		return
	}
	if _, err := os.Stat(dst); err == nil {
		return
	}
	if Joined(base) {
		if c := JoinedControl(base); c != "" && !SameControl(c, control) {
			return // the base state belongs to another control server
		}
		if err := os.Rename(base, dst); err != nil {
			return
		}
	}
	if _, err := os.Stat(base + ".authkey"); err == nil {
		if _, err := os.Stat(dst + ".authkey"); err != nil {
			os.Rename(base+".authkey", dst+".authkey")
		}
	}
}

// JoinedControl is the control URL recorded in dir's joined marker ("" for
// markers written before it was recorded, or none).
func JoinedControl(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, JoinedMarker))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 {
		return ""
	}
	return strings.TrimSpace(lines[1])
}

// StateDirs lists the tailscale state dirs under base (base itself and
// base-<control>) that have been up.
func StateDirs(base string) []string {
	var out []string
	if Joined(base) {
		out = append(out, base)
	}
	m, _ := filepath.Glob(base + "-*")
	for _, d := range m {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() && Joined(d) {
			out = append(out, d)
		}
	}
	return out
}

// LogoutDir logs out the (stopped) node whose state is in dir: it starts
// it from that state without a key and asks the control server to log it
// out (best effort: a node whose control server is unreachable stays
// registered until it expires or an admin deletes it). control "" = the
// URL in the joined marker (or Tailscale's default).
func LogoutDir(ctx context.Context, dir, control, hostname string) error {
	if !Joined(dir) {
		return nil
	}
	if control == "" {
		control = JoinedControl(dir)
	}
	if hostname == "" {
		hostname = "longarm-logout"
	}
	s := &tsnet.Server{Dir: dir, ControlURL: control, Hostname: hostname,
		Logf: func(string, ...any) {}, UserLogf: func(string, ...any) {}}
	defer s.Close()
	lc, err := s.LocalClient()
	if err != nil {
		return err
	}
	// Wait (briefly) for the node to be logged in, else there is nothing
	// the control server would accept.
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, err := lc.StatusWithoutPeers(ctx)
		if err == nil && st.BackendState == "Running" {
			break
		}
		if err == nil && (st.BackendState == "NeedsLogin" || st.AuthURL != "") {
			return errors.New("not logged in")
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("control server not reached")
		}
		time.Sleep(250 * time.Millisecond)
	}
	return lc.Logout(ctx)
}
