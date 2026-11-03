package e2e

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

var updateFixtures = flag.Bool("update", false, "TestProgramsFixtures: rewrite docs/programs/fixtures from this run")

// fixturesDir holds the golden outputs programs copy (docs/programs.md).
var fixturesDir = filepath.Join("..", "docs", "programs", "fixtures")

// programFixtures: fixture file -> the longarm command that produces it.
// %s is the job id.
var programFixtures = map[string][]string{
	"version.json":   {"version", "--json"},
	"worker-ls.json": {"worker", "ls", "--json"},
	"whoami.json":    {"whoami", "--json"},
	"jobs-id.json":   {"jobs", "--json", "--id", "%s"},
	"sessions.json":  {"sessions", "--json"},
	"doctor.json":    {"doctor", "--json"},
}

// opaque are the fixture paths whose content is not part of the contract
// (only that they exist with their type): doctor's per-check data differs
// by platform and check, a worker's agents by what runs on it.
var opaque = []string{"doctor.json: checks[].data", "worker-ls.json: nodes[].metrics.agents"}

// mayBeZero are fields left out at their zero value (protojson) that the
// test can't keep from being zero: missing is fine.
var mayBeZero = []string{
	"worker-ls.json: nodes[].metrics.cpu_percent", "worker-ls.json: nodes[].metrics.net_rx_bytes_per_sec",
	"worker-ls.json: nodes[].metrics.net_tx_bytes_per_sec", "worker-ls.json: nodes[].metrics.top_cpu[].cpu_percent",
	"worker-ls.json: nodes[].metrics.top_mem[].cpu_percent",
}

// TestProgramsFixtures produces the outputs programs parse from a real hub
// and worker and compares them with the golden fixtures: every field of a
// fixture must still exist with the same JSON type (a field added is
// fine; one removed or retyped fails). -update rewrites the fixtures,
// anonymised.
func TestProgramsFixtures(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	alice := h.client
	os.WriteFile(filepath.Join(alice, "workers.toml"), nil, 0o644) // the hub's workers only
	w := h.newWorkerDir(t, "worker-1", "")
	h.joinWorker(t, w, true)
	w.start(t, true)
	h.waitNode(t, "worker-1", "online", func(n map[string]any) bool { return isTrue(n["online"]) })

	id := strings.TrimSpace(h.mustLongarm(alice, "run", "worker-1", "--name", "build", "--", bins.helper, "print", "1", "0", "0"))
	h.mustLongarm(alice, "wait", "worker-1", id)
	waitFor(t, "the job in the hub's index", 20*time.Second, func() bool {
		return h.longarm(alice, "jobs", "--hub-only", "--json", "--id", id).code == 0
	})
	h.mustLongarm(alice, "session", "start", "worker-1", "-s", "agent1", "--", bins.helper, "repl")
	h.mustLongarm(alice, "session", "send", "worker-1", "-s", "agent1", "--text", "hello\n")

	anon := newAnonymiser(t, map[string]string{
		w.dataDir:     "/var/lib/longarmd",
		alice:         "/home/alice/.config/longarm",
		h.root:        "/tmp/longarm",
		os.TempDir():  "/tmp",
		bins.helper:   "claude",
		bins.longarm:  "/usr/local/bin/longarm",
		bins.longarmd: "/usr/local/bin/longarmd",
	})
	for _, name := range sortedKeys(programFixtures) {
		args := slices.Clone(programFixtures[name])
		for i, a := range args {
			if strings.Contains(a, "%s") {
				args[i] = fmt.Sprintf(a, id)
			}
		}
		r := h.longarm(alice, args...)
		var got any
		if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
			t.Errorf("longarm %v (exit %d): not JSON: %v\n%s\n%s", args, r.code, err, r.stdout, r.stderr)
			continue
		}
		path := filepath.Join(fixturesDir, name)
		if *updateFixtures {
			b, _ := json.MarshalIndent(anon.value(got), "", "  ")
			if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v (go test ./e2e -run TestProgramsFixtures -update writes it)", name, err)
			continue
		}
		var want any
		if err := json.Unmarshal(b, &want); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		for _, e := range schemaDiff(name+": ", want, got) {
			t.Errorf("%s (longarm %s)", e, strings.Join(args, " "))
		}
	}
	if *updateFixtures {
		return
	}
	// every fixture file is produced here
	files, _ := filepath.Glob(filepath.Join(fixturesDir, "*.json"))
	for _, f := range files {
		if _, ok := programFixtures[filepath.Base(f)]; !ok {
			t.Errorf("%s: no command produces it (programFixtures)", f)
		}
	}
}

// schemaDiff lists what of want (a fixture) is missing from got or has
// another JSON type there. Every value of got is checked, each element of
// an array too; a field need only be in one of an array's elements (the
// others may leave it out at its zero value). null in got is fine where
// the fixture has null or the path is in nullable; where the fixture has
// null, any type is. An empty array in the fixture says nothing about its
// elements.
func schemaDiff(path string, want, got any) []string {
	return newShape(want).diff(path, []any{got})
}

// nullable are the paths that are null when not set although the fixture
// shows them set: sessions --json has every field, unset times and
// objects as null.
var nullable = []string{
	"sessions.json: sessions[].createdAt", "sessions.json: sessions[].startedAt",
	"sessions.json: sessions[].endedAt", "sessions.json: sessions[].lastOutput",
	"sessions.json: sessions[].lastInput", "sessions.json: sessions[].lastActivity",
	"sessions.json: sessions[].workspace",
}

// shape is what a fixture says about a path: the JSON types seen there,
// whether null was, and those of its fields and array elements (all of
// the fixture's elements together).
type shape struct {
	types  map[string]bool
	null   bool
	fields map[string]*shape
	elem   *shape // nil: no elements seen
}

func newShape(v any) *shape {
	s := &shape{types: map[string]bool{}}
	s.add(v)
	return s
}

func (s *shape) add(v any) {
	if v == nil {
		s.null = true
		return
	}
	s.types[jsonType(v)] = true
	switch x := v.(type) {
	case map[string]any:
		if s.fields == nil {
			s.fields = map[string]*shape{}
		}
		for k, e := range x {
			if s.fields[k] == nil {
				s.fields[k] = &shape{types: map[string]bool{}}
			}
			s.fields[k].add(e)
		}
	case []any:
		for _, e := range x {
			if s.elem == nil {
				s.elem = &shape{types: map[string]bool{}}
			}
			s.elem.add(e)
		}
	}
}

// diff checks the values gots found at path (one per array element above)
// against s.
func (s *shape) diff(path string, gots []any) []string {
	name := strings.TrimSuffix(path, ".")
	if len(s.types) == 0 { // the fixture has only null here
		return nil
	}
	var out []string
	var objs []map[string]any
	var elems []any
	for _, g := range gots {
		switch x := g.(type) {
		case nil:
			if !s.null && !slices.Contains(nullable, name) {
				out = append(out, fmt.Sprintf("%s is null, was %s", name, s.typeNames()))
			}
			continue
		case map[string]any:
			objs = append(objs, x)
		case []any:
			elems = append(elems, x...)
		}
		if !s.types[jsonType(g)] {
			out = append(out, fmt.Sprintf("%s is %s, was %s", name, jsonType(g), s.typeNames()))
		}
	}
	if s.types["an object"] && len(objs) > 0 {
		for _, k := range sortedKeys(s.fields) {
			var vs []any
			for _, o := range objs {
				if v, ok := o[k]; ok {
					vs = append(vs, v)
				}
			}
			if len(vs) == 0 {
				if !slices.Contains(mayBeZero, path+k) {
					out = append(out, fmt.Sprintf("%s%s is missing", path, k))
				}
				continue
			}
			f := s.fields[k]
			if slices.Contains(opaque, path+k) { // its type only
				f = &shape{types: f.types, null: f.null}
			}
			out = append(out, f.diff(path+k+".", vs)...)
		}
	}
	if s.elem != nil && len(elems) > 0 {
		out = append(out, s.elem.diff(name+"[].", elems)...)
	}
	return out
}

func (s *shape) typeNames() string {
	ts := sortedKeys(s.types)
	return strings.Join(ts, " or ")
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// anonymiser turns a test run's output into an example: paths of the
// test's directories, this machine's host and user names, ids, addresses,
// times and versions become the documentation's examples.
type anonymiser struct {
	paths []string // longest first
	subst map[string]string
	exact map[string]string
	res   []struct {
		re   *regexp.Regexp
		repl string
	}
}

func newAnonymiser(t *testing.T, paths map[string]string) *anonymiser {
	a := &anonymiser{subst: map[string]string{}, exact: map[string]string{}}
	for p, ex := range paths {
		p = filepath.ToSlash(filepath.Clean(p))
		a.paths = append(a.paths, p)
		a.subst[p] = ex
	}
	sort.Slice(a.paths, func(i, j int) bool { return len(a.paths[i]) > len(a.paths[j]) })
	if b, err := exec.Command(bins.longarm, "version", "--json").Output(); err == nil {
		var v struct{ Version string }
		if json.Unmarshal(b, &v) == nil && v.Version != "" {
			a.exact[v.Version] = "v1.9.0"
		}
	}
	var words []string
	if host, err := os.Hostname(); err == nil && len(host) > 2 {
		words = append(words, `(?i)\b`+regexp.QuoteMeta(host)+`\b=pc-1`)
	}
	if u, err := user.Current(); err == nil {
		name := u.Username
		if i := strings.LastIndexAny(name, `\/`); i >= 0 {
			name = name[i+1:]
		}
		if len(name) > 2 {
			words = append(words, `(?i)\b`+regexp.QuoteMeta(name)+`\b=alice`)
		}
	}
	for _, r := range append(words,
		`\b[0-9A-HJKMNP-TV-Z]{26}\b=01JC8ZK3T5W9QX4M2N6P7R8S0A`,                                             // ULIDs (job ids)
		`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}=3f2a9c1e-8b7d-4c5e-9f1a-2b3c4d5e6f70`, // node ids
		`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)=2026-11-03T09:00:00Z`,
		`127\.0\.0\.1:\d+=100.64.0.11:7701`,
		`\b[0-9a-fA-F]{40,}\b=5f3cbd0a9e2d4c71b8a6f0e3d2c1b0a99f8e7d6c5b4a39281706f5e4d3c2b1a0`,
	) {
		i := strings.LastIndex(r, "=")
		a.res = append(a.res, struct {
			re   *regexp.Regexp
			repl string
		}{regexp.MustCompile(r[:i]), r[i+1:]})
	}
	return a
}

func (a *anonymiser) value(v any) any {
	switch x := v.(type) {
	case string:
		return a.str(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = a.value(e)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = a.value(e)
			if procs, ok := out[k].([]any); ok && (k == "top_cpu" || k == "top_mem") {
				for i, p := range procs { // this machine's processes: examples
					if m, ok := p.(map[string]any); ok {
						m["name"] = exampleProcs[i%len(exampleProcs)]
					}
				}
			}
		}
		return out
	}
	return v
}

var exampleProcs = []string{"node", "claude", "longarmd", "git", "pwsh"}

func (a *anonymiser) str(s string) string {
	if ex, ok := a.exact[s]; ok {
		return ex
	}
	if strings.HasPrefix(s, "https://") && !strings.Contains(s, "example.") { // a hub's URL (default_hub)
		return "https://hub.example.com"
	}
	slash := filepath.ToSlash(s)
	for _, p := range a.paths {
		if i := strings.Index(strings.ToLower(slash), strings.ToLower(p)); i >= 0 {
			s = slash[:i] + a.subst[p] + slash[i+len(p):]
			s = strings.TrimSuffix(s, ".exe")
			break
		}
	}
	for _, r := range a.res {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return s
}

// The fixture check: a field removed or retyped fails, one added does not.
func TestSchemaDiff(t *testing.T) {
	var fixture any
	json.Unmarshal([]byte(`{"a": "x", "n": 1, "o": {"b": true}, "l": [{"c": "1"}, {"d": 2}], "z": null, "e": []}`), &fixture)
	for in, want := range map[string]string{
		`{"a": "y", "n": 2, "o": {"b": false, "new": 1}, "l": [{"c": "2", "d": 3}], "z": "now set", "e": [1], "added": 1}`: "",
		`{"a": "y", "n": 2, "o": {"b": false}, "l": [{"c": "2"}, {"d": 3}], "z": null, "e": []}`:                           "",
		`{"n": 2, "o": {"b": false}, "l": [{"c": "2", "d": 3}], "z": 1, "e": []}`:                                          "t: a is missing",
		`{"a": 1, "n": 2, "o": {"b": false}, "l": [{"c": "2", "d": 3}], "z": 1, "e": []}`:                                  "t: a is a number, was a string",
		`{"a": "y", "n": 2, "o": {}, "l": [{"c": "2", "d": 3}], "z": 1, "e": []}`:                                          "t: o.b is missing",
		`{"a": "y", "n": 2, "o": {"b": false}, "l": [{"c": 2, "d": 3}], "z": 1, "e": []}`:                                  "t: l[].c is a number, was a string",
		`{"a": "y", "n": 2, "o": {"b": false}, "l": [{"c": "2"}], "z": 1, "e": []}`:                                        "t: l[].d is missing",
	} {
		var got any
		if err := json.Unmarshal([]byte(in), &got); err != nil {
			t.Fatal(err)
		}
		if d := strings.Join(schemaDiff("t: ", fixture, got), "; "); d != want {
			t.Errorf("%s: %q, want %q", in, d, want)
		}
	}
}

// The fixture check sees every value: a null or a retyped value in any
// array element fails, the allowances (nullable, opaque, mayBeZero) still
// hold.
func TestSchemaDiffEveryValue(t *testing.T) {
	for _, c := range []struct{ name, fixture, in, want string }{
		{"version.json", `{"version": "v1"}`, `{"version": null}`, "version.json: version is null, was a string"},
		{"version.json", `{"features": ["a", "b"]}`, `{"features": ["one", 123]}`, "version.json: features[] is a number, was a string"},
		{"worker-ls.json", `{"nodes": [{"id": "x", "online": true}]}`,
			`{"nodes": [{"id": "y", "online": true}, {"id": 7, "online": "yes"}]}`,
			"worker-ls.json: nodes[].id is a number, was a string; worker-ls.json: nodes[].online is a string, was a boolean"},
		{"worker-ls.json", `{"nodes": [{"id": "x"}, {"id": "y"}]}`, `{"nodes": [{"id": "y"}, null]}`, "worker-ls.json: nodes[] is null, was an object"},
		// fine: a field in one element only, added fields, the allowances
		{"worker-ls.json", `{"nodes": [{"id": "x", "metrics": {"cpu_percent": 1, "agents": {"claude": 1}}}]}`,
			`{"nodes": [{"id": "x", "new": 1}, {"id": "y", "metrics": {"agents": {"codex": "z"}}}]}`, ""},
		{"worker-ls.json", `{"nodes": [{"metrics": {"agents": {"claude": 1}}}]}`,
			`{"nodes": [{"metrics": {"agents": []}}]}`, "worker-ls.json: nodes[].metrics.agents is an array, was an object"},
		{"sessions.json", `{"sessions": [{"lastInput": "2026-11-03T09:00:00Z", "endedAt": null, "argv": ["a"]}]}`,
			`{"sessions": [{"lastInput": null, "endedAt": "2026-11-03T09:00:00Z", "argv": ["b"]}]}`, ""},
		{"sessions.json", `{"sessions": [{"argv": ["a"]}]}`, `{"sessions": [{"argv": null}]}`, "sessions.json: sessions[].argv is null, was an array"},
	} {
		var fixture, got any
		if err := json.Unmarshal([]byte(c.fixture), &fixture); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(c.in), &got); err != nil {
			t.Fatal(err)
		}
		if d := strings.Join(schemaDiff(c.name+": ", fixture, got), "; "); d != c.want {
			t.Errorf("%s against %s: %q, want %q", c.in, c.fixture, d, c.want)
		}
	}
}
