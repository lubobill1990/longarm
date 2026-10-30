package update

import (
	"regexp"
	"strconv"
	"strings"
)

// Compare orders versions like "1.2.3", "v1.2.3", "1.2.3-rc.1",
// "1.2.3-4-gabcdef" (git describe): numeric dot parts first, then a
// version with a pre-release suffix sorts before the same version without
// one; suffixes compare part by part (numbers numerically). "dev" and other
// unparsable versions sort before everything. Returns -1, 0 or 1.
func Compare(a, b string) int {
	pa, sa, oka := split(a)
	pb, sb, okb := split(b)
	switch {
	case !oka && !okb:
		return strings.Compare(a, b)
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := 0; i < max(len(pa), len(pb)); i++ {
		x, y := at(pa, i), at(pb, i)
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case sa == sb:
		return 0
	case sa == "":
		return 1
	case sb == "":
		return -1
	}
	return comparePre(sa, sb)
}

func at(p []int, i int) int {
	if i < len(p) {
		return p[i]
	}
	return 0
}

func split(v string) ([]int, string, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	core, suffix, _ := strings.Cut(v, "-")
	core, _, _ = strings.Cut(core, "+")
	if core == "" {
		return nil, "", false
	}
	var parts []int
	for _, s := range strings.Split(core, ".") {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return nil, "", false
		}
		parts = append(parts, n)
	}
	return parts, suffix, true
}

func comparePre(a, b string) int {
	as, bs := strings.FieldsFunc(a, sep), strings.FieldsFunc(b, sep)
	for i := 0; i < min(len(as), len(bs)); i++ {
		x, ex := strconv.Atoi(as[i])
		y, ey := strconv.Atoi(bs[i])
		switch {
		case ex == nil && ey == nil:
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
		case ex == nil:
			return -1
		case ey == nil:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

func sep(r rune) bool { return r == '.' || r == '-' }

var versionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)

// ValidVersion reports whether v is a safe version label (it becomes part of
// file paths: no separators, no "..").
func ValidVersion(v string) bool { return versionRE.MatchString(v) && !strings.Contains(v, "..") }

// NewerRelease reports whether a is a newer release than b: both release
// versions ("0.1.17", "v1.2.3-rc.1"; not "dev" or another development
// build's label) and a sorts after b. Deciding whether to replace a running
// program with this one: never a downgrade, never on a development build.
func NewerRelease(a, b string) bool {
	_, _, oka := split(a)
	_, _, okb := split(b)
	return oka && okb && Compare(a, b) > 0
}
