// Package diag collects diagnostic bundles (bug reports with logs):
// `longarmd diag` on a worker, `longarm diag` on a client device. A bundle
// is a zip of text files - versions, system facts, the process logs of the
// last hours, configuration without secrets, certificate metadata, policy
// summaries, job metadata - plus manifest.json, which lists every file with
// its size and the reason it is there.
//
// Every text file goes through a pii.Scrubber (fleet salt, names known
// locally) before it is added; binary files are left out, not scrubbed.
// Private keys, tokens, job command lines and output (unless asked for
// with --include-job), and the audit log (unless --include-audit) are
// never collected. docs/usage.md "Bug reports / diagnostics".
//
// The hub side - storing uploaded bundles in Azure Blob Storage - is
// Store / BlobStore / DirStore; report ids are NewReportID.
package diag

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lubobill1990/longarm/internal/pii"
	"github.com/lubobill1990/longarm/internal/version"
)

// Format of manifest.json.
const Format = "longarm-diag/1"

// ManifestFile is the name of the manifest in the zip.
const ManifestFile = "manifest.json"

// Builder accumulates the files of a bundle. Not safe for concurrent use.
type Builder struct {
	// Component is "longarmd" or "longarm".
	Component string
	Scrub     *pii.Scrubber
	// Node is the pseudonym of the collecting node ("<node:3f2a9c>").
	Node string
	// Since is the log window.
	Since time.Duration
	// Options records the flags (include_audit, include_jobs) in the
	// manifest.
	Options map[string]any

	files    []*File
	names    map[string]bool
	excluded []Excluded
	errs     []string
	scrub    map[string]int
	created  time.Time
}

// File is one file of the bundle.
type File struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Reason   string `json:"reason"`
	Scrubbed bool   `json:"scrubbed"`
	data     []byte
}

// Excluded is a file the bundle deliberately leaves out.
type Excluded struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Manifest is manifest.json.
type Manifest struct {
	Format    string         `json:"format"`
	Component string         `json:"component"`
	Version   string         `json:"version"`
	Created   time.Time      `json:"created"`
	Node      string         `json:"node,omitempty"`
	OS        string         `json:"os"`
	Arch      string         `json:"arch"`
	Since     string         `json:"since"`
	Options   map[string]any `json:"options,omitempty"`
	Files     []*File        `json:"files"`
	Excluded  []Excluded     `json:"excluded,omitempty"`
	// Scrub counts the replacements per kind (secret, ip, email, host,
	// user, node, ...) over all files; never the values.
	Scrub  map[string]int `json:"scrub"`
	Errors []string       `json:"errors,omitempty"`
}

// NewBuilder starts a bundle.
func NewBuilder(component string, s *pii.Scrubber) *Builder {
	return &Builder{Component: component, Scrub: s, names: map[string]bool{}, scrub: map[string]int{}, created: time.Now().UTC()}
}

// placeholders the scrubber writes, by kind (counted in the manifest).
var placeholders = []struct{ tag, kind string }{
	{"<secret>", "secret"}, {"<jwt>", "jwt"}, {"<private-key>", "private_key"}, {"<join-code>", "join_code"},
	{"<userinfo>", "url_userinfo"}, {"?<query>", "url_query"}, {"<email>", "email"}, {"<ip>", "ip"},
	{"<" + pii.KindHost + ":", "host"}, {"<" + pii.KindUser + ":", "user"}, {"<" + pii.KindNode + ":", "node"},
}

func countTags(s string) map[string]int {
	m := map[string]int{}
	for _, p := range placeholders {
		if n := strings.Count(s, p.tag); n > 0 {
			m[p.kind] += n
		}
	}
	return m
}

// ScrubString scrubs s and counts the replacements.
func (b *Builder) ScrubString(s string) string {
	out := b.Scrub.String(s)
	before := countTags(s)
	for k, n := range countTags(out) {
		if d := n - before[k]; d > 0 {
			b.scrub[k] += d
		}
	}
	return out
}

// IsText reports whether data looks like text (valid UTF-8, no NUL bytes).
func IsText(data []byte) bool {
	return utf8.Valid(data) && bytes.IndexByte(data, 0) < 0
}

// AddText adds a text file after scrubbing it. Data that isn't text is
// excluded (binary files are never scrubbed and sent).
func (b *Builder) AddText(name, reason string, data []byte) {
	if !IsText(data) {
		b.Exclude(name, "binary content (only text files are collected)")
		return
	}
	b.add(name, reason, []byte(b.ScrubString(string(data))), true)
}

// AddJSON adds v as indented JSON (scrubbed like text).
func (b *Builder) AddJSON(name, reason string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		b.Errorf("%s: %v", name, err)
		return
	}
	b.AddText(name, reason, append(data, '\n'))
}

func (b *Builder) add(name, reason string, data []byte, scrubbed bool) {
	name = path.Clean(strings.ReplaceAll(name, `\`, "/"))
	name = b.Scrub.String(name) // names in file names too
	for base, i := name, 2; b.names[name]; i++ {
		ext := path.Ext(base)
		name = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(base, ext), i, ext)
	}
	b.names[name] = true
	b.files = append(b.files, &File{Name: name, Size: int64(len(data)), Reason: reason, Scrubbed: scrubbed, data: data})
}

// Exclude records a file that is deliberately not collected.
func (b *Builder) Exclude(name, reason string) {
	b.excluded = append(b.excluded, Excluded{Name: b.ScrubString(name), Reason: reason})
}

// Errorf records a collection problem (in the manifest; scrubbed).
func (b *Builder) Errorf(format string, args ...any) {
	b.errs = append(b.errs, b.ScrubString(fmt.Sprintf(format, args...)))
}

// Files returns the collected files.
func (b *Builder) Files() []*File { return b.files }

// File returns the collected file name (nil if absent).
func (b *Builder) File(name string) *File {
	for _, f := range b.files {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// Data of a collected file.
func (f *File) Data() []byte { return f.data }

// Excluded returns what was left out.
func (b *Builder) ExcludedFiles() []Excluded { return b.excluded }

// Errors returns the collection problems.
func (b *Builder) Errors() []string { return b.errs }

// TotalSize is the sum of the files' sizes.
func (b *Builder) TotalSize() int64 {
	var n int64
	for _, f := range b.files {
		n += f.Size
	}
	return n
}

// Manifest builds the manifest.
func (b *Builder) Manifest() *Manifest {
	files := append([]*File(nil), b.files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	sc := map[string]int{}
	for k, v := range b.scrub {
		sc[k] = v
	}
	return &Manifest{Format: Format, Component: b.Component, Version: version.Version, Created: b.created, Node: b.Node,
		OS: runtime.GOOS, Arch: runtime.GOARCH, Since: b.Since.String(), Options: b.Options, Files: files,
		Excluded: b.excluded, Scrub: sc, Errors: b.errs}
}

// WriteZip writes the bundle (manifest.json first).
func (b *Builder) WriteZip(w io.Writer) error {
	zw := zip.NewWriter(w)
	mf, err := json.MarshalIndent(b.Manifest(), "", "  ")
	if err != nil {
		return err
	}
	put := func(name string, data []byte) error {
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: b.created})
		if err != nil {
			return err
		}
		_, err = fw.Write(data)
		return err
	}
	if err := put(ManifestFile, append(mf, '\n')); err != nil {
		return err
	}
	for _, f := range b.files {
		if err := put(f.Name, f.data); err != nil {
			return err
		}
	}
	return zw.Close()
}

// WriteFile writes the zip to path (0600) and returns its size.
func (b *Builder) WriteFile(p string) (int64, error) {
	var buf bytes.Buffer
	if err := b.WriteZip(&buf); err != nil {
		return 0, err
	}
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		return 0, err
	}
	return int64(buf.Len()), nil
}

// Summary is the human-readable list of what the bundle contains, shown
// before it is written or uploaded.
func (b *Builder) Summary() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "diagnostic bundle (%s %s): %d files, %s before compression\n", b.Component, version.Version, len(b.files)+1, HumanBytes(b.TotalSize()))
	files := append([]*File(nil), b.files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	for _, f := range files {
		fmt.Fprintf(&sb, "  %-34s %9s  %s\n", f.Name, HumanBytes(f.Size), f.Reason)
	}
	if len(b.excluded) > 0 {
		fmt.Fprintf(&sb, "not included:\n")
		for _, e := range b.excluded {
			fmt.Fprintf(&sb, "  %-34s %s\n", e.Name, e.Reason)
		}
	}
	var kinds []string
	for k, n := range b.scrub {
		if n > 0 {
			kinds = append(kinds, fmt.Sprintf("%s %d", k, n))
		}
	}
	sort.Strings(kinds)
	if len(kinds) > 0 {
		fmt.Fprintf(&sb, "replaced: %s\n", strings.Join(kinds, ", "))
	}
	if len(b.errs) > 0 {
		fmt.Fprintf(&sb, "collection problems: %d (listed in manifest.json)\n", len(b.errs))
	}
	return sb.String()
}

// HumanBytes formats n like "1.2 MiB".
func HumanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
