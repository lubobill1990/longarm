package platform

import (
	"os"
	"strings"
)

// Fresh PATH (Windows; docs/troubleshooting.md): longarmd's environment is
// fixed when it starts, so a tool installed later (winget, npm -g, a Go
// zip) would be missing from every job's PATH until it restarts. Before a
// job, exec or ssh session starts, longarmd rebuilds its PATH from the
// registry (Machine, then User, as Explorer does for a new process) merged
// with the PATH it has. RefreshPath does it (a no-op elsewhere).

// FreshPathOff (LONGARM_FRESH_PATH=0 in longarmd's environment) keeps
// longarmd's PATH as it started.
func FreshPathOff() bool { return os.Getenv("LONGARM_FRESH_PATH") == "0" }

// MergePath is the PATH of a new process: the registry's entries first,
// then the rest of current (the daemon's PATH), each directory once
// (case-insensitively, a trailing separator ignored), empty entries
// dropped. Entries in front of current that the registry does not have
// (a PATH set up as "C:\mine;%PATH%" for the daemon) stay in front.
// Applying it again to its own result changes nothing.
func MergePath(current string, registry []string, sep string) string {
	key := func(p string) string {
		return strings.ToLower(strings.TrimRight(strings.TrimSpace(p), `\/`))
	}
	inRegistry := map[string]bool{}
	for _, p := range registry {
		inRegistry[key(p)] = true
	}
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		k := key(p)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, strings.TrimSpace(p))
	}
	cur := strings.Split(current, sep)
	i := 0
	for ; i < len(cur) && (key(cur[i]) == "" || !inRegistry[key(cur[i])]); i++ {
		add(cur[i])
	}
	for _, p := range registry {
		add(p)
	}
	for _, p := range cur[i:] {
		add(p)
	}
	return strings.Join(out, sep)
}

// ExpandWindowsEnv expands %NAME% references in s (REG_EXPAND_SZ) with
// lookup (names are case-insensitive there); unknown names and a lone %
// stay as they are, like ExpandEnvironmentStrings.
func ExpandWindowsEnv(s string, lookup func(name string) (string, bool)) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '%')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		name := s[i+1 : i+1+j]
		if v, ok := lookup(name); ok && name != "" {
			b.WriteString(s[:i])
			b.WriteString(v)
			s = s[i+2+j:]
			continue
		}
		// not a variable: keep the first % and look again from the second
		b.WriteString(s[:i+1+j])
		s = s[i+1+j:]
	}
}
