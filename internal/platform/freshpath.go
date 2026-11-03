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
// with the PATH it has. RefreshPath does it (a no-op elsewhere); NoteStartPath,
// as longarmd starts, remembers what was put in front of its PATH.

// FreshPathOff (LONGARM_FRESH_PATH=0 in longarmd's environment) keeps
// longarmd's PATH as it started.
func FreshPathOff() bool { return os.Getenv("LONGARM_FRESH_PATH") == "0" }

// MergePath is the PATH of a new process: prefix (those of its entries
// still in current), then the registry's entries, then the rest of current
// (the daemon's PATH), each directory once (case-insensitively, a trailing
// separator ignored), empty entries dropped. Applying it again to its own
// result changes nothing.
//
// prefix is what was in front of the registry's entries on longarmd's PATH
// when it started (StartPrefix: a PATH set up as "C:\mine;%PATH%" for the
// daemon). It is not inferred from current: a directory the registry had
// then and has dropped since (an upgrade replacing C:\old-go\bin by
// C:\new-go\bin) must not stay ahead of what replaced it.
func MergePath(current string, prefix, registry []string, sep string) string {
	cur := strings.Split(current, sep)
	inCurrent := map[string]bool{}
	for _, p := range cur {
		inCurrent[pathKey(p)] = true
	}
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		k := pathKey(p)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, strings.TrimSpace(p))
	}
	for _, p := range prefix {
		if inCurrent[pathKey(p)] {
			add(p)
		}
	}
	for _, p := range registry {
		add(p)
	}
	for _, p := range cur {
		add(p)
	}
	return strings.Join(out, sep)
}

// StartPrefix is the leading entries of current that registry doesn't
// have: what was put in front of the daemon's PATH (MergePath's prefix).
func StartPrefix(current string, registry []string, sep string) []string {
	inRegistry := map[string]bool{}
	for _, p := range registry {
		inRegistry[pathKey(p)] = true
	}
	var prefix []string
	for _, p := range strings.Split(current, sep) {
		k := pathKey(p)
		if k == "" {
			continue
		}
		if inRegistry[k] {
			break
		}
		prefix = append(prefix, strings.TrimSpace(p))
	}
	return prefix
}

// pathKey is the directory p names, for comparing PATH entries.
func pathKey(p string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(p), `\/`))
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
