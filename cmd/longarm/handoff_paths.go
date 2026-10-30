package main

// Path safety of longarm handoff. The file list of a session comes from
// the source machine - a worker that may be compromised - and the names in
// it are joined onto the target's agent directory. Every such name is
// untrusted: it is validated as a plain relative path, the joined result
// must stay inside the session's own subtree, no existing component of
// the destination may be a symlink or junction, and new files are created
// exclusively. Names never reach a path on this machine: the client's
// temporary copies are named by index in a private directory.

import (
	"context"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// maxHandoffFiles caps the files of one session a handoff moves.
const maxHandoffFiles = 20000

// unsafePathErr is a refused source-supplied name (exit 2).
func unsafePathErr(rel, why string) error {
	return usagef("refusing the session: the source listed an unsafe file name %q (%s)", rel, why)
}

// winReservedRE matches Windows device names (CON, NUL, COM1, ...), also
// with an extension.
var winReservedRE = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9¹²³]|lpt[0-9¹²³]|conin\$|conout\$|clock\$)(\..*)?$`)

var driveRE = regexp.MustCompile(`^[A-Za-z]:`)

// checkRel validates a '/'-separated relative path from the source for a
// target of the given OS (win: Windows rules too). The whole name is
// refused, never repaired: no absolute paths, drive letters, UNC or empty
// components, "." / "..", backslashes, control characters; on Windows
// targets no ":" (drives, alternate data streams), device names, or
// components ending in "." or " ".
func checkRel(rel string, win bool) error {
	switch {
	case rel == "":
		return unsafePathErr(rel, "empty")
	case strings.Contains(rel, `\`):
		return unsafePathErr(rel, "backslash")
	case strings.HasPrefix(rel, "/"):
		return unsafePathErr(rel, "absolute")
	case driveRE.MatchString(rel):
		return unsafePathErr(rel, "drive letter")
	}
	for _, r := range rel {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return unsafePathErr(rel, "control character")
		}
	}
	for _, c := range strings.Split(rel, "/") {
		switch c {
		case "":
			return unsafePathErr(rel, "empty component")
		case ".", "..":
			return unsafePathErr(rel, "dot component")
		}
		if win {
			switch {
			case strings.Contains(c, ":"):
				return unsafePathErr(rel, `":" on a Windows target`)
			case winReservedRE.MatchString(strings.TrimRight(c, ". ")):
				return unsafePathErr(rel, "Windows device name")
			case strings.HasSuffix(c, ".") || strings.HasSuffix(c, " "):
				return unsafePathErr(rel, "trailing dot or space on a Windows target")
			case strings.ContainsAny(c, `<>"|?*`):
				return unsafePathErr(rel, "character Windows does not allow")
			}
		}
	}
	return nil
}

// cleanAbs normalizes an absolute path of ep for comparisons ('/'
// separators, cleaned; lower case on Windows).
func cleanAbs(ep *hoEndpoint, p string) string {
	p = path.Clean(strings.ReplaceAll(p, `\`, "/"))
	if ep.windows() {
		p = strings.ToLower(p)
	}
	return p
}

// contained reports whether p is root or below it (after cleaning).
func contained(ep *hoEndpoint, root, p string) bool {
	r, q := cleanAbs(ep, root), cleanAbs(ep, p)
	return q == r || strings.HasPrefix(q, strings.TrimSuffix(r, "/")+"/")
}

// topOf finds the targetTop f belongs to.
func topOf(tops []agentFile, f agentFile) (agentFile, bool) {
	for _, t := range tops {
		if t.base == f.base && (f.rel == t.rel || strings.HasPrefix(f.rel, t.rel+"/")) {
			return t, true
		}
	}
	return agentFile{}, false
}

// sessionTop is the subtree of the target a session file may go to: one
// of the adapter's targetTops (the file itself or a directory it is in),
// or Claude Code's todos/<id>-*.json.
func sessionTop(ad agentAdapter, id string, f agentFile) (agentFile, bool) {
	if t, ok := topOf(ad.targetTops(id), f); ok {
		return t, true
	}
	if ad.name() == "claude" && f.base == "config" {
		if name, ok := strings.CutPrefix(f.rel, "todos/"); ok && !strings.Contains(name, "/") &&
			strings.HasPrefix(name, id+"-") && strings.HasSuffix(name, ".json") {
			return agentFile{base: "config", rel: f.rel}, true
		}
	}
	return agentFile{}, false
}

// checkSessionNames validates the source's file list of a session before
// anything happens: the count, every name and size, and that each belongs
// to the session's own files.
func checkSessionNames(ad agentAdapter, dst *hoEndpoint, sess *agentSession) error {
	return checkSessionFiles(ad, dst, sess, func(string) string { return "" })
}

// checkSessionFiles is checkSessionNames plus: each file lands in the
// session's own subtree of the target's base directory.
func checkSessionFiles(ad agentAdapter, dst *hoEndpoint, sess *agentSession, baseDir func(string) string) error {
	if len(sess.files) > maxHandoffFiles {
		return usagef("the session has %d files, more than handoff moves (%d)", len(sess.files), maxHandoffFiles)
	}
	seen := map[string]bool{}
	for _, f := range sess.files {
		if err := checkRel(f.rel, dst.windows()); err != nil {
			return err
		}
		if f.size < 0 {
			return unsafePathErr(f.rel, "negative size")
		}
		top, ok := sessionTop(ad, sess.ID, f)
		if !ok {
			return unsafePathErr(f.rel, "outside the session's files")
		}
		base := baseDir(f.base)
		if base == "" {
			continue
		}
		target, root := dst.join(base, f.rel), dst.join(base, top.rel)
		if !contained(dst, root, target) || !contained(dst, base, root) {
			return unsafePathErr(f.rel, "escapes the session's directory")
		}
		k := cleanAbs(dst, target)
		if seen[k] {
			return unsafePathErr(f.rel, "listed twice")
		}
		seen[k] = true
	}
	return nil
}

// linkChecker refuses destinations with a symlink / junction in any
// existing component below root, and destinations that exist already.
type linkChecker struct {
	ep   *hoEndpoint
	root string
	ok   map[string]bool // directories checked (cleaned)
}

func newLinkChecker(ep *hoEndpoint, root string) *linkChecker {
	return &linkChecker{ep: ep, root: root, ok: map[string]bool{}}
}

// lstat reports whether p exists and is a link (not followed).
func (lc *linkChecker) lstat(ctx context.Context, p string) (exists, link, dir bool, err error) {
	if lc.ep.local() {
		fi, err := os.Lstat(p)
		if os.IsNotExist(err) {
			return false, false, false, nil
		}
		if err != nil {
			return false, false, false, err
		}
		return true, fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0, fi.IsDir(), nil
	}
	if !slices.Contains(lc.ep.features, "file-lstat") {
		return false, false, false, usagef("handoff: worker %s (longarmd %s) cannot check paths for symlinks (file-lstat); update longarmd", lc.ep.name, lc.ep.version)
	}
	fi, err := lc.ep.conn.Files().Stat(ctx, &pb.FileStatRequest{Path: p, NoFollow: true})
	if err != nil {
		return false, false, false, err
	}
	return fi.Exists, fi.IsSymlink, fi.IsDir, nil
}

// check verifies target (a new file below root): every existing directory
// between root and it is a real directory, and target does not exist.
func (lc *linkChecker) check(ctx context.Context, target string) error {
	if !contained(lc.ep, lc.root, target) {
		return unsafePathErr(target, "outside "+lc.root)
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(cleanAbs(lc.ep, target), strings.TrimSuffix(cleanAbs(lc.ep, lc.root), "/")), "/")
	// the same components in the endpoint's spelling
	parts := strings.Split(strings.ReplaceAll(target, `\`, "/"), "/")
	n := len(strings.Split(rel, "/"))
	if rel == "" || n > len(parts) {
		return unsafePathErr(target, "not below "+lc.root)
	}
	cur := lc.root
	missing := false
	for i, c := range parts[len(parts)-n:] {
		cur = lc.ep.join(cur, c)
		last := i == n-1
		if missing {
			continue // below a directory that does not exist yet: new
		}
		key := cleanAbs(lc.ep, cur)
		if !last && lc.ok[key] {
			continue
		}
		exists, link, dir, err := lc.lstat(ctx, cur)
		if err != nil {
			return err
		}
		switch {
		case !exists:
			missing = true
		case link:
			return usagef("refusing to write %s on %s: %s is a symlink or junction", target, lc.ep.name, cur)
		case last:
			return usagef("refusing to write %s on %s: it exists already", target, lc.ep.name)
		case !dir:
			return usagef("refusing to write %s on %s: %s is not a directory", target, lc.ep.name, cur)
		default:
			lc.ok[key] = true
		}
	}
	return nil
}

// errBudget: a file the source sends is larger than it said or than the
// handoff's size limit.
func errBudget(p string, limit int64) error {
	return usagef("refusing %s: more than %s (the session's size limit, or the size the source listed)", p, humanSize(limit))
}
