package main

// Path rewriting of agent transcripts for longarm handoff: the session
// moves from a source worktree to a target worktree (another directory,
// possibly another OS), so the absolute paths it recorded - its cwd, the
// files its tools touched, its own state directory - are mapped from the
// source prefix to the target one.
//
// The rewrite is structural and byte-preserving: the JSON text is scanned
// token by token and only string values that change are re-encoded; keys,
// numbers, key order and every other string stay byte for byte. In string
// values of known path fields (cwd, file_path, ...) a leading source
// prefix is replaced and the rest's separators converted to the target's;
// in every other string value only exact occurrences of a source prefix
// followed by a path boundary are replaced (conservative: /repo never
// matches inside /repo2).

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// pathMap maps a source path prefix to a target one.
type pathMap struct {
	from, to       string
	fromWin, toWin bool // the paths' OS is Windows (\ separators, case-insensitive)
}

// hoPathKeys are the JSON keys whose string values are paths (seen in
// Claude Code and Copilot CLI transcripts).
var hoPathKeys = map[string]bool{
	"cwd": true, "file_path": true, "filePath": true, "path": true, "notebook_path": true, "workingDirectory": true,
	"filename": true, "outputFile": true, "outputFilePath": true, "realParentDir": true, "live_cwd": true, "root": true,
	"trackingPath": true, "gitRoot": true, "git_root": true, "projectDir": true, "projectPath": true, "dir": true,
	"directory": true, "absolutePath": true,
}

// pathRewriter applies maps (longest source first).
type pathRewriter struct {
	maps []pathMap
}

func newPathRewriter(maps []pathMap) *pathRewriter {
	var ms []pathMap
	for _, m := range maps {
		m.from = strings.TrimRight(m.from, `/\`)
		m.to = strings.TrimRight(m.to, `/\`)
		if m.from == "" || m.from == m.to {
			continue
		}
		ms = append(ms, m)
	}
	sort.SliceStable(ms, func(i, j int) bool { return len(ms[i].from) > len(ms[j].from) })
	return &pathRewriter{maps: ms}
}

func isPathSep(c byte, win bool) bool { return c == '/' || (win && c == '\\') }

// prefixLen returns how much of s the source prefix m.from covers (0: not
// a prefix at a path boundary).
func (m pathMap) prefixLen(s string) int {
	n := len(m.from)
	if len(s) < n {
		return 0
	}
	head := s[:n]
	if m.fromWin {
		if !strings.EqualFold(strings.ReplaceAll(head, "/", `\`), strings.ReplaceAll(m.from, "/", `\`)) {
			return 0
		}
	} else if head != m.from {
		return 0
	}
	if len(s) == n || isPathSep(s[n], m.fromWin) {
		return n
	}
	return 0
}

// convertRest turns the remainder of a mapped path into the target's
// separators.
func (m pathMap) convertRest(rest string) string {
	switch {
	case m.toWin && !m.fromWin:
		return strings.ReplaceAll(rest, "/", `\`)
	case !m.toWin && m.fromWin:
		return strings.ReplaceAll(rest, `\`, "/")
	}
	return rest
}

// mapPath maps a path value (a known path field): the longest matching
// prefix, the rest converted. ok false: no map applies.
func (r *pathRewriter) mapPath(s string) (string, bool) {
	for _, m := range r.maps {
		if n := m.prefixLen(s); n > 0 {
			return m.to + m.convertRest(s[n:]), true
		}
	}
	return s, false
}

// variant is one spelling of a source prefix in free text and what
// replaces it.
type variant struct{ from, to string }

func (r *pathRewriter) variants() []variant {
	var vs []variant
	for _, m := range r.maps {
		vs = append(vs, variant{m.from, m.to})
		if m.fromWin {
			slash := strings.ReplaceAll(m.from, `\`, "/")
			toSlash := m.to
			if m.toWin {
				toSlash = strings.ReplaceAll(m.to, `\`, "/")
			}
			vs = append(vs, variant{slash, toSlash})
			// JSON text inside a string (a tool result holding JSON)
			vs = append(vs, variant{strings.ReplaceAll(m.from, `\`, `\\`), strings.ReplaceAll(m.to, `\`, `\\`)})
		}
	}
	sort.SliceStable(vs, func(i, j int) bool { return len(vs[i].from) > len(vs[j].from) })
	return vs
}

// boundaryAfter: the character after a match ends the path prefix (not a
// longer name such as /repo2 or /repo.old).
func boundaryAfter(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	c := s[i]
	return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c >= 0x80)
}

// boundaryBefore: a match does not start inside a longer path or word.
func boundaryBefore(s string, i int) bool {
	if i == 0 {
		return true
	}
	c := s[i-1]
	return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '/' || c == '\\' || c >= 0x80)
}

// replaceText replaces exact occurrences of the source prefixes in free
// text (case-sensitive; at path boundaries).
func (r *pathRewriter) replaceText(s string, vs []variant) (string, bool) {
	var b strings.Builder
	changed := false
	last := 0
	for i := 0; i < len(s); i++ {
		for _, v := range vs {
			if len(v.from) == 0 || !strings.HasPrefix(s[i:], v.from) {
				continue
			}
			if !boundaryBefore(s, i) || !boundaryAfter(s, i+len(v.from)) {
				continue
			}
			b.WriteString(s[last:i])
			b.WriteString(v.to)
			i += len(v.from) - 1
			last = i + 1
			changed = true
			break
		}
	}
	if !changed {
		return s, false
	}
	b.WriteString(s[last:])
	return b.String(), true
}

// rewriteValue rewrites one JSON string value under key.
func (r *pathRewriter) rewriteValue(key, s string, vs []variant) (string, bool) {
	if hoPathKeys[key] {
		if out, ok := r.mapPath(s); ok {
			return out, true
		}
	}
	return r.replaceText(s, vs)
}

// rewriteJSON rewrites the string values of JSON text (one document or
// JSON lines); everything else is kept byte for byte. n counts the values
// changed. Malformed text is scanned the same way (strings are found by
// their quotes), so a partial last line is harmless.
func (r *pathRewriter) rewriteJSON(b []byte) (out []byte, n int) {
	if len(r.maps) == 0 {
		return b, 0
	}
	vs := r.variants()
	var buf bytes.Buffer
	buf.Grow(len(b) + len(b)/16)
	key := ""
	i := 0
	for i < len(b) {
		c := b[i]
		if c != '"' {
			if c == '\n' {
				key = ""
			}
			buf.WriteByte(c)
			i++
			continue
		}
		j := i + 1
		for j < len(b) && b[j] != '"' {
			if b[j] == '\\' {
				j++
			}
			j++
		}
		if j >= len(b) { // unterminated: keep the rest
			buf.Write(b[i:])
			break
		}
		raw := b[i : j+1]
		i = j + 1
		k := i
		for k < len(b) && (b[k] == ' ' || b[k] == '\t' || b[k] == '\r' || b[k] == '\n') {
			k++
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			buf.Write(raw)
			continue
		}
		if k < len(b) && b[k] == ':' { // a key
			key = s
			buf.Write(raw)
			continue
		}
		ns, changed := r.rewriteValue(key, s, vs)
		if !changed {
			buf.Write(raw)
			continue
		}
		buf.Write(jsonString(ns))
		n++
	}
	return buf.Bytes(), n
}

// jsonString encodes s as a JSON string without HTML escaping.
func jsonString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// rewriteYAMLPaths rewrites the top-level path keys (cwd, git_root) of a
// simple YAML file (Copilot CLI's workspace.yaml); other lines are kept.
func (r *pathRewriter) rewriteYAMLPaths(b []byte) (out []byte, n int) {
	lines := strings.SplitAfter(string(b), "\n")
	for i, l := range lines {
		k, v, ok := strings.Cut(l, ":")
		if !ok || (k != "cwd" && k != "git_root") {
			continue
		}
		eol := ""
		if strings.HasSuffix(v, "\r\n") {
			eol = "\r\n"
		} else if strings.HasSuffix(v, "\n") {
			eol = "\n"
		}
		val := strings.TrimSpace(v)
		switch {
		case len(val) >= 2 && val[0] == '"':
			var s string
			if json.Unmarshal([]byte(val), &s) != nil {
				continue
			}
			val = s
		case len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'':
			val = strings.ReplaceAll(val[1:len(val)-1], "''", "'")
		}
		nv, ok := r.mapPath(val)
		if !ok {
			continue
		}
		lines[i] = k + ": " + string(jsonString(nv)) + eol
		n++
	}
	return []byte(strings.Join(lines, "")), n
}

// claudeProjectDirName is Claude Code's directory name under
// ~/.claude/projects for a working directory: every character other than
// an ASCII letter or digit becomes "-" (C:\src\app -> C--src-app,
// /home/u/.x -> -home-u--x). Claude Code shortens names over 200
// characters with a hash longarm does not reproduce: ok false then.
func claudeProjectDirName(p string) (string, bool) {
	b := []byte(p)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	// multi-byte characters: one "-" per UTF-16 unit in Claude Code; ASCII paths only here
	for _, r := range p {
		if r >= 0x80 {
			return string(b), false
		}
	}
	return string(b), len(b) <= 200
}
