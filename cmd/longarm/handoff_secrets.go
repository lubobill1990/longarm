package main

// The secret check of longarm handoff --via remote: before a snapshot goes
// to a branch of the repository's remote (where everybody with access to
// the repository can read it, and it stays in reflogs and caches after the
// branch is deleted), its changed and untracked files are scanned for
// credentials. A hit keeps the snapshot on longarm's own end-to-end
// channels (relay) unless --allow-secrets. The rules are a small built-in
// set (private keys, well-known token formats, .env-style assignments,
// key files by name); a hit is reported by file and rule, never by value.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// secretHit is a rule that matched a file.
type secretHit struct {
	file, rule string
}

type secretRule struct {
	name string
	re   *regexp.Regexp
}

var secretRules = []secretRule{
	{"private key", regexp.MustCompile(`-----BEGIN ([A-Z0-9]+ )*PRIVATE KEY( BLOCK)?-----`)},
	{"AWS access key", regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{50,})\b`)},
	{"GitLab token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"OpenAI / Anthropic key", regexp.MustCompile(`\bsk-(ant-|proj-)?[A-Za-z0-9_-]{32,}\b`)},
	{"Azure storage key", regexp.MustCompile(`(?i)AccountKey=[A-Za-z0-9+/]{40,}={0,2}`)},
	{"Azure DevOps / generic PAT in a URL", regexp.MustCompile(`https://[^/\s:@]*:[^/\s@]{16,}@`)},
	{"JSON Web Token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"secret assignment", regexp.MustCompile(`(?im)^\s*(export\s+)?["']?[A-Za-z0-9_.-]*(secret|token|passw(or)?d|pwd|api[_-]?key|access[_-]?key|private[_-]?key|client[_-]?secret|conn(ection)?[_-]?string)["']?\s*[:=]\s*["']?[^\s"'#]{8,}`)},
}

// secretNames are file names that are credentials by themselves.
var secretNameRE = regexp.MustCompile(`(?i)(^|/)(\.env(\.[a-z0-9_-]+)?|id_(rsa|dsa|ecdsa|ed25519)|[^/]*\.(pem|key|p12|pfx|jks|keystore|ppk)|\.npmrc|\.pypirc|\.netrc|credentials(\.json)?|secrets?\.(json|ya?ml|toml))$`)

// placeholder values that are not secrets (examples, templates).
var secretPlaceholderRE = regexp.MustCompile(`(?i)(x{6,}|\*{6,}|<[^>]*>|\$\{[^}]*\}|\$[A-Z_]+|changeme|example|placeholder|your[_-]|dummy|redacted)`)

// maxScanFile: larger files are checked by name only.
const maxScanFile = 2 << 20

// scanSnapshot checks the files that differ between base and snap (added
// or modified, i.e. the uncommitted and untracked ones): hits of the secret
// rules, and the files larger than maxFile.
func scanSnapshot(ctx context.Context, r *hoRepo, base, snap string, maxFile int64) (hits []secretHit, big []string, err error) {
	out, err := r.git(ctx, nil, nil, "diff", "--name-only", "-z", "--no-renames", "--diff-filter=AMT", base, snap)
	if err != nil {
		return nil, nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return nil, nil, nil
	}
	// sizes (paths with a newline can't be named to cat-file: refuse them)
	var in bytes.Buffer
	for _, f := range files {
		if strings.ContainsAny(f, "\n\r") {
			return nil, nil, fmt.Errorf("a file name with a line break: can't check it")
		}
		fmt.Fprintf(&in, "%s:%s\n", snap, f)
	}
	sizes, err := r.git(ctx, nil, in.Bytes(), "cat-file", "--batch-check=%(objecttype) %(objectsize)")
	if err != nil {
		return nil, nil, err
	}
	sl := strings.Split(sizes, "\n")
	if len(sl) != len(files) {
		return nil, nil, fmt.Errorf("cat-file: %d answers for %d files", len(sl), len(files))
	}
	var scan []string
	var batch int64
	flush := func() error {
		if len(scan) == 0 {
			return nil
		}
		var in bytes.Buffer
		for _, f := range scan {
			fmt.Fprintf(&in, "%s:%s\n", snap, f)
		}
		raw, err := r.ep.run(ctx, r.top, hoGitEnv, in.Bytes(), gitTimeout, "git", "cat-file", "--batch")
		if err != nil {
			return err
		}
		br := bufio.NewReader(bytes.NewReader(raw))
		for _, f := range scan {
			head, err := br.ReadString('\n')
			if err != nil {
				return fmt.Errorf("cat-file --batch: %v", err)
			}
			fs := strings.Fields(head)
			if len(fs) != 3 {
				return fmt.Errorf("cat-file --batch: %q", strings.TrimSpace(head))
			}
			n, _ := strconv.Atoi(fs[2])
			body := make([]byte, n+1)
			if _, err := ioReadFull(br, body); err != nil {
				return fmt.Errorf("cat-file --batch: %v", err)
			}
			hits = append(hits, matchSecrets(f, body[:n])...)
			clear(body)
		}
		clear(raw)
		scan, batch = nil, 0
		return nil
	}
	for i, f := range files {
		typ, sz, _ := strings.Cut(strings.TrimSpace(sl[i]), " ")
		n, _ := strconv.ParseInt(sz, 10, 64)
		if secretNameRE.MatchString(f) {
			hits = append(hits, secretHit{f, "credential file name"})
		}
		switch {
		case typ != "blob":
			continue // a submodule (commit) or a missing entry
		case n > maxFile:
			big = append(big, f)
			continue
		case n > maxScanFile:
			continue
		}
		if batch+n > 8<<20 {
			if err := flush(); err != nil {
				return nil, nil, err
			}
		}
		scan = append(scan, f)
		batch += n
	}
	if err := flush(); err != nil {
		return nil, nil, err
	}
	return dedupHits(hits), big, nil
}

// matchSecrets applies the content rules to a file (binary files: none).
func matchSecrets(file string, b []byte) []secretHit {
	if bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
		return nil
	}
	var hits []secretHit
	for _, r := range secretRules {
		for _, m := range r.re.FindAll(b, 20) {
			if r.name == "secret assignment" || r.name == "Azure DevOps / generic PAT in a URL" {
				if secretPlaceholderRE.Match(m) {
					continue
				}
			}
			hits = append(hits, secretHit{file, r.name})
			break
		}
	}
	return hits
}

func dedupHits(h []secretHit) []secretHit {
	sort.Slice(h, func(i, j int) bool {
		if h[i].file != h[j].file {
			return h[i].file < h[j].file
		}
		return h[i].rule < h[j].rule
	})
	var out []secretHit
	for i, x := range h {
		if i == 0 || x != h[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// describeHits names the files and rules (never a value).
func describeHits(h []secretHit) string {
	var parts []string
	for i, x := range h {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("and %d more", len(h)-5))
			break
		}
		parts = append(parts, path.Clean(x.file)+" ("+x.rule+")")
	}
	return strings.Join(parts, ", ")
}

func ioReadFull(r *bufio.Reader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := r.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
