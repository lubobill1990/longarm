package main

// Agent adapters of longarm handoff / agent-sessions: where Claude Code
// and Copilot CLI keep a conversation, how to resume it, and how to tell
// whether a process still has it open (docs/usage.md#hand-off-an-agent-session).
//
// Claude Code (verified with 2.1.x): ~/.claude (or $CLAUDE_CONFIG_DIR;
// `claude auth status --json` names it as configDirectory)
//   projects/<project dir name>/<id>.jsonl   the transcript (JSON lines, "cwd" on each message)
//   projects/<project dir name>/<id>/        subagents/, tool-results/ of the session
//   file-history/<id>/, tasks/<id>/, todos/<id>-*.json   related state, if present
//   sessions/<pid>.json                      {"pid","sessionId","cwd",...} of a running claude
//   resume: claude --resume <id> in the project directory
// Copilot CLI (verified layout with 1.0.x; resuming a copied session is
// experimental): ~/.copilot (or $COPILOT_HOME)
//   session-state/<id>/events.jsonl, workspace.yaml (cwd, git_root), checkpoints/, files/ ...
//   open-sessions-state.json                 {"<id>": {"openedAt","refreshedAt",...}}
//   resume: copilot --resume=<id>

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/platform"
)

// agentSession is one resumable conversation of an agent on a machine.
type agentSession struct {
	Agent      string       `json:"agent"`
	ID         string       `json:"id"`
	ProjectDir string       `json:"projectDir"`
	Modified   time.Time    `json:"modified"`
	Title      string       `json:"title"`
	Size       int64        `json:"size"`
	Version    string       `json:"version,omitempty"` // of the agent that last wrote it
	HandedOff  *handoffMark `json:"handedOff,omitempty"`

	cfg        string      // the agent's config directory on the machine
	transcript string      // the main file
	files      []agentFile // what a handoff moves
	marker     string      // where the hand-off marker goes
}

// agentFile is one file of a session: src on the source machine; on the
// target it goes to rel below the target's base ("project": Claude's
// project directory of the target worktree; "config": the agent's config
// directory).
type agentFile struct {
	src     string
	base    string
	rel     string // '/'-separated
	size    int64
	rewrite string // "json", "yaml" or "" (copied as is)
}

// handoffMark is the sidecar written next to a handed-off session.
type handoffMark struct {
	To             string    `json:"handedOffTo"`
	At             time.Time `json:"at"`
	TargetWorktree string    `json:"targetWorktree,omitempty"`
	TargetCwd      string    `json:"targetCwd,omitempty"`
	TargetSession  string    `json:"targetSession,omitempty"`
	Snapshot       string    `json:"snapshot,omitempty"`
	SnapshotRef    string    `json:"snapshotRef,omitempty"`
	Base           string    `json:"base,omitempty"`
	By             string    `json:"by,omitempty"`
}

type agentAdapter interface {
	name() string
	// configDir is the agent's state directory on ep.
	configDir(ctx context.Context, ep *hoEndpoint) string
	// list finds the sessions on ep (id "": all; with files: what a
	// handoff moves, else only the transcript and its sizes).
	list(ctx context.Context, ep *hoEndpoint, cfg, id string, withFiles bool) ([]*agentSession, error)
	// projectBase is the target directory of "project" files (Claude: the
	// project directory of cwd); "" when the agent has none.
	projectBase(ep *hoEndpoint, cfg, cwd string) (string, error)
	// targetTops are the files / directories of id a handoff writes on the
	// target (backed up first when they exist), relative to the bases.
	targetTops(id string) []agentFile
	resumeArgv(id string) []string
	// loggedIn: best effort (known false: could not tell).
	loggedIn(ctx context.Context, ep *hoEndpoint, cfg string) (in, known bool)
	// mcpServers are the names of the MCP servers configured for projectDir
	// (user-level and project-level).
	mcpServers(ctx context.Context, ep *hoEndpoint, cfg, projectDir string) []string
	// inUse names a process that has the session open ("": none found).
	inUse(ctx context.Context, ep *hoEndpoint, s *agentSession) string
	exitHint() string
}

func adapterFor(name string) (agentAdapter, error) {
	switch name {
	case "claude":
		return claudeAdapter{}, nil
	case "copilot":
		return copilotAdapter{}, nil
	}
	return nil, usagef("--agent %q: want claude or copilot", name)
}

var allAdapters = []agentAdapter{claudeAdapter{}, copilotAdapter{}}

var sessionIDRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_-]{5,127}$`)

// maxHeadRead / maxTailRead bound what agent-sessions reads of a transcript.
const (
	maxHeadRead = 256 << 10
	maxTailRead = 128 << 10
)

// ---------------------------------------------------------------- Claude Code

type claudeAdapter struct{}

func (claudeAdapter) name() string { return "claude" }

func (claudeAdapter) exitHint() string {
	return "type /exit (or press Ctrl-C twice) in that Claude Code"
}

func (claudeAdapter) resumeArgv(id string) []string { return []string{"claude", "--resume", id} }

func (claudeAdapter) configDir(ctx context.Context, ep *hoEndpoint) string {
	if ep.local() {
		if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
			return d
		}
		return ep.join(ep.home, ".claude")
	}
	// the worker's claude knows its own (CLAUDE_CONFIG_DIR in longarmd's environment)
	if out, err := ep.run(ctx, "", nil, nil, 45*time.Second, "claude", "auth", "status", "--json"); err == nil {
		var st struct {
			ConfigDirectory string `json:"configDirectory"`
		}
		if json.Unmarshal(out, &st) == nil && st.ConfigDirectory != "" {
			return st.ConfigDirectory
		}
	}
	return ep.join(ep.home, ".claude")
}

func (claudeAdapter) projectBase(ep *hoEndpoint, cfg, cwd string) (string, error) {
	n, ok := claudeProjectDirName(cwd)
	if !ok {
		return "", usagef("the target directory %s is too long or not ASCII for Claude Code's project directory name; use a shorter --worktree-dir", cwd)
	}
	return ep.join(cfg, "projects", n), nil
}

func (claudeAdapter) targetTops(id string) []agentFile {
	return []agentFile{{base: "project", rel: id + ".jsonl"}, {base: "project", rel: id},
		{base: "config", rel: "file-history/" + id}, {base: "config", rel: "tasks/" + id}}
}

// claudeLine is what longarm reads of a transcript line.
type claudeLine struct {
	Type        string          `json:"type"`
	Cwd         string          `json:"cwd"`
	Version     string          `json:"version"`
	IsMeta      bool            `json:"isMeta"`
	IsSidechain bool            `json:"isSidechain"`
	AITitle     string          `json:"aiTitle"`
	CustomTitle string          `json:"customTitle"`
	Summary     string          `json:"summary"`
	Message     json.RawMessage `json:"message"`
}

// userText is the text a user typed in a message ("" for tool results
// and command output).
func userText(raw json.RawMessage) string {
	var m struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Role != "user" {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) == nil {
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				return p.Text
			}
		}
	}
	return ""
}

// oneLineTitle shortens a prompt / title for a table column.
func oneLineTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if strings.HasPrefix(s, "<") { // command / system markup, not what the user typed
		return ""
	}
	if r := []rune(s); len(r) > 80 {
		s = string(r[:77]) + "..."
	}
	return s
}

// scanLines calls f for each complete JSON line of b.
func scanLines(b []byte, f func([]byte)) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), maxHeadRead+1)
	for sc.Scan() {
		if l := bytes.TrimSpace(sc.Bytes()); len(l) > 0 && l[0] == '{' {
			f(l)
		}
	}
}

func (a claudeAdapter) list(ctx context.Context, ep *hoEndpoint, cfg, id string, withFiles bool) ([]*agentSession, error) {
	projects := ep.join(cfg, "projects")
	ents, err := ep.list(ctx, projects)
	if err != nil {
		return nil, err
	}
	type acc struct {
		s       *agentSession
		dir     string
		related []hoFile
	}
	byKey := map[string]*acc{}
	var order []string
	for _, f := range ents {
		parts := strings.Split(f.rel, "/")
		if len(parts) != 2 || f.isDir || !strings.HasSuffix(parts[1], ".jsonl") {
			continue
		}
		sid := strings.TrimSuffix(parts[1], ".jsonl")
		if !sessionIDRE.MatchString(sid) || (id != "" && sid != id) || checkRel(parts[0], true) != nil {
			continue // (a project directory name the marker could not safely be written next to)
		}
		k := parts[0] + "/" + sid
		byKey[k] = &acc{dir: parts[0], s: &agentSession{Agent: a.name(), ID: sid, cfg: cfg, Modified: f.mtime, Size: f.size,
			transcript: ep.join(projects, parts[0], parts[1]), marker: ep.join(projects, parts[0], sid+".handoff.json")}}
		byKey[k].s.files = append(byKey[k].s.files, agentFile{src: byKey[k].s.transcript, base: "project", rel: sid + ".jsonl", size: f.size, rewrite: "json"})
		order = append(order, k)
	}
	markers := map[string]string{}
	for _, f := range ents {
		parts := strings.Split(f.rel, "/")
		if len(parts) == 2 && strings.HasSuffix(parts[1], ".handoff.json") {
			markers[parts[0]+"/"+strings.TrimSuffix(parts[1], ".handoff.json")] = f.rel
		}
		if len(parts) < 3 || f.isDir {
			continue
		}
		if ac := byKey[parts[0]+"/"+parts[1]]; ac != nil {
			ac.s.Size += f.size
			rel := strings.Join(parts[1:], "/")
			ac.s.files = append(ac.s.files, agentFile{src: ep.join(projects, parts[0], filepath2native(ep, rel)), base: "project",
				rel: rel, size: f.size, rewrite: rewriteKind(rel)})
		}
	}
	out := make([]*agentSession, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k].s)
	}
	// read heads and tails in parallel (8 at a time)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, k := range order {
		ac := byKey[k]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			a.describe(ctx, ep, ac.s)
			if m, ok := markers[k]; ok {
				if b, err := ep.readSmall(ctx, ep.join(projects, filepath2native(ep, m)), 64<<10); err == nil {
					var hm handoffMark
					if json.Unmarshal(b, &hm) == nil && !ac.s.Modified.After(hm.At.Add(2*time.Second)) {
						ac.s.HandedOff = &hm
					}
				}
			}
		}()
	}
	wg.Wait()
	if withFiles {
		for _, s := range out {
			if err := a.relatedFiles(ctx, ep, s); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// describe fills in the project directory, title and version from the
// transcript's first and last bytes.
func (claudeAdapter) describe(ctx context.Context, ep *hoEndpoint, s *agentSession) {
	head, err := ep.readRange(ctx, s.transcript, 0, maxHeadRead)
	if err != nil {
		return
	}
	prompt := ""
	scanLines(head, func(l []byte) {
		var cl claudeLine
		if json.Unmarshal(l, &cl) != nil {
			return
		}
		if s.ProjectDir == "" && cl.Cwd != "" && !cl.IsSidechain {
			s.ProjectDir = cl.Cwd
		}
		if cl.Version != "" {
			s.Version = cl.Version
		}
		if prompt == "" && cl.Type == "user" && !cl.IsMeta && !cl.IsSidechain {
			prompt = oneLineTitle(userText(cl.Message))
		}
		switch {
		case cl.CustomTitle != "":
			s.Title = oneLineTitle(cl.CustomTitle)
		case cl.AITitle != "" && s.Title == "":
			s.Title = oneLineTitle(cl.AITitle)
		}
	})
	if s.Size > int64(len(head)) {
		if tail, err := ep.readRange(ctx, s.transcript, -maxTailRead, maxTailRead); err == nil {
			custom := ""
			scanLines(tail, func(l []byte) {
				var cl claudeLine
				if json.Unmarshal(l, &cl) != nil {
					return
				}
				if cl.Version != "" {
					s.Version = cl.Version
				}
				if cl.CustomTitle != "" {
					custom = oneLineTitle(cl.CustomTitle)
				} else if cl.AITitle != "" && custom == "" {
					s.Title = oneLineTitle(cl.AITitle)
				}
			})
			if custom != "" {
				s.Title = custom
			}
		}
	}
	if s.Title == "" {
		s.Title = prompt
	}
}

// relatedFiles adds file-history/<id>/, tasks/<id>/ and todos/<id>-*.json.
func (claudeAdapter) relatedFiles(ctx context.Context, ep *hoEndpoint, s *agentSession) error {
	for _, d := range []string{"file-history", "tasks"} {
		ents, err := ep.list(ctx, ep.join(s.cfg, d, s.ID))
		if err != nil {
			return err
		}
		for _, f := range ents {
			if f.isDir {
				continue
			}
			rel := d + "/" + s.ID + "/" + f.rel
			s.files = append(s.files, agentFile{src: ep.join(s.cfg, filepath2native(ep, rel)), base: "config", rel: rel, size: f.size,
				rewrite: map[bool]string{true: rewriteKind(rel)}[d == "tasks"]})
		}
	}
	todos := ep.join(s.cfg, "todos")
	if _, ok, err := ep.stat(ctx, todos); err == nil && ok {
		ents, err := ep.list(ctx, todos)
		if err != nil {
			return err
		}
		for _, f := range ents {
			if !f.isDir && !strings.Contains(f.rel, "/") && strings.HasPrefix(f.rel, s.ID+"-") && strings.HasSuffix(f.rel, ".json") {
				s.files = append(s.files, agentFile{src: ep.join(todos, f.rel), base: "config", rel: "todos/" + f.rel, size: f.size, rewrite: "json"})
			}
		}
	}
	return nil
}

func (claudeAdapter) loggedIn(ctx context.Context, ep *hoEndpoint, cfg string) (bool, bool) {
	out, err := ep.run(ctx, "", nil, nil, 45*time.Second, "claude", "auth", "status", "--json")
	if err != nil && len(out) == 0 {
		return false, false
	}
	var st struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if json.Unmarshal(out, &st) != nil || st.LoggedIn == nil {
		return false, false
	}
	return *st.LoggedIn, true
}

// claudeJSONPath is Claude Code's global config (~/.claude.json, or
// .claude.json in $CLAUDE_CONFIG_DIR).
func claudeJSONPath(ep *hoEndpoint, cfg string) string {
	if ep.samePath(cfg, ep.join(ep.home, ".claude")) {
		return ep.join(ep.home, ".claude.json")
	}
	return ep.join(cfg, ".claude.json")
}

func (claudeAdapter) mcpServers(ctx context.Context, ep *hoEndpoint, cfg, projectDir string) []string {
	// read in memory for the server names only (the file may hold
	// credentials in servers' env; nothing of it is printed or kept)
	b, err := ep.readSmall(ctx, claudeJSONPath(ep, cfg), 32<<20)
	if err != nil {
		return nil
	}
	var cj struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &cj) != nil {
		return nil
	}
	clear(b)
	var names []string
	for n := range cj.MCPServers {
		names = append(names, n)
	}
	if projectDir != "" {
		for p, pr := range cj.Projects {
			if ep.samePath(p, projectDir) {
				for n := range pr.MCPServers {
					names = append(names, n)
				}
			}
		}
	}
	sort.Strings(names)
	return slicesCompact(names)
}

// agentProcNames are process names that can be a running agent (Claude
// Code runs as claude or under node / bun).
var agentProcNames = []string{"claude", "node", "bun", "copilot"}

func agentProcName(n string) bool {
	n = strings.ToLower(strings.TrimSuffix(strings.ToLower(n), ".exe"))
	for _, a := range agentProcNames {
		if strings.Contains(n, a) {
			return true
		}
	}
	return false
}

func (a claudeAdapter) inUse(ctx context.Context, ep *hoEndpoint, s *agentSession) string {
	dir := ep.join(s.cfg, "sessions")
	ents, err := ep.list(ctx, dir)
	if err != nil {
		return ""
	}
	for _, f := range ents {
		if f.isDir || strings.Contains(f.rel, "/") || !strings.HasSuffix(f.rel, ".json") {
			continue
		}
		b, err := ep.readSmall(ctx, ep.join(dir, f.rel), 256<<10)
		if err != nil {
			continue
		}
		var ps struct {
			PID       int    `json:"pid"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(b, &ps) != nil || ps.SessionID != s.ID || ps.PID <= 0 {
			continue
		}
		if name, alive := processAlive(ctx, ep, ps.PID); alive && agentProcName(name) {
			return fmt.Sprintf("%s (pid %d)", name, ps.PID)
		}
	}
	return ""
}

// processAlive reports whether pid runs on ep, and its name.
func processAlive(ctx context.Context, ep *hoEndpoint, pid int) (string, bool) {
	if ep.local() {
		if !platform.ProcessAlive(pid, 0) {
			return "", false
		}
		procs, err := platform.ListProcesses()
		if err == nil {
			for _, p := range procs {
				if p.PID == pid {
					return p.Name, true
				}
			}
		}
		return "", runtime.GOOS == "windows" && err != nil // alive, name unknown
	}
	if ep.windows() {
		out, err := ep.run(ctx, "", nil, nil, 30*time.Second, "tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH")
		if err != nil {
			return "", false
		}
		for _, l := range strings.Split(string(out), "\n") {
			f := strings.Split(strings.TrimSpace(l), ",")
			if len(f) >= 2 && strings.Trim(f[1], `"`) == strconv.Itoa(pid) {
				return strings.Trim(f[0], `"`), true
			}
		}
		return "", false
	}
	out, err := ep.run(ctx, "", nil, nil, 30*time.Second, "ps", "-p", strconv.Itoa(pid), "-o", "comm=")
	if err != nil {
		return "", false
	}
	n := strings.TrimSpace(string(out))
	return path.Base(n), n != ""
}

// ---------------------------------------------------------------- Copilot CLI

type copilotAdapter struct{}

func (copilotAdapter) name() string { return "copilot" }

func (copilotAdapter) exitHint() string { return "type /exit (or press Ctrl-C) in that Copilot CLI" }

func (copilotAdapter) resumeArgv(id string) []string { return []string{"copilot", "--resume=" + id} }

func (copilotAdapter) configDir(ctx context.Context, ep *hoEndpoint) string {
	if ep.local() {
		if d := os.Getenv("COPILOT_HOME"); d != "" {
			return d
		}
	}
	return ep.join(ep.home, ".copilot")
}

func (copilotAdapter) projectBase(*hoEndpoint, string, string) (string, error) { return "", nil }

func (copilotAdapter) targetTops(id string) []agentFile {
	return []agentFile{{base: "config", rel: "session-state/" + id}}
}

// copilotMarker is the hand-off marker's name inside a session directory.
const copilotMarker = "longarm-handoff.json"

func (a copilotAdapter) list(ctx context.Context, ep *hoEndpoint, cfg, id string, withFiles bool) ([]*agentSession, error) {
	root := ep.join(cfg, "session-state")
	var ents []hoFile
	var err error
	if id != "" {
		ents, err = ep.list(ctx, ep.join(root, id))
		for i := range ents {
			ents[i].rel = id + "/" + ents[i].rel
		}
	} else {
		ents, err = ep.list(ctx, root)
	}
	if err != nil {
		return nil, err
	}
	byID := map[string]*agentSession{}
	var order []string
	for _, f := range ents {
		sid, rel, ok := strings.Cut(f.rel, "/")
		if !ok || !sessionIDRE.MatchString(sid) || f.isDir {
			continue
		}
		s := byID[sid]
		if s == nil {
			s = &agentSession{Agent: a.name(), ID: sid, cfg: cfg, transcript: ep.join(root, sid, "events.jsonl"),
				marker: ep.join(root, sid, copilotMarker)}
			byID[sid] = s
			order = append(order, sid)
		}
		if rel == copilotMarker {
			continue
		}
		s.Size += f.size
		if rel == "events.jsonl" || s.Modified.IsZero() {
			if f.mtime.After(s.Modified) || rel == "events.jsonl" {
				s.Modified = f.mtime
			}
		}
		kind := rewriteKind(rel)
		if rel == "workspace.yaml" {
			kind = "yaml"
		}
		s.files = append(s.files, agentFile{src: ep.join(root, sid, filepath2native(ep, rel)), base: "config", rel: "session-state/" + sid + "/" + rel,
			size: f.size, rewrite: kind})
	}
	var out []*agentSession
	for _, sid := range order {
		if _, ok := hasFile(byID[sid].files, "events.jsonl"); ok {
			out = append(out, byID[sid])
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, s := range out {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			a.describe(ctx, ep, s)
		}()
	}
	wg.Wait()
	if !withFiles {
		for _, s := range out {
			s.files = nil
		}
	}
	return out, nil
}

func hasFile(fs []agentFile, suffix string) (agentFile, bool) {
	for _, f := range fs {
		if strings.HasSuffix(f.rel, "/"+suffix) {
			return f, true
		}
	}
	return agentFile{}, false
}

func (copilotAdapter) describe(ctx context.Context, ep *hoEndpoint, s *agentSession) {
	if b, err := ep.readSmall(ctx, ep.dir(s.transcript)+ep.sep()+"workspace.yaml", 64<<10); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			k, v, ok := strings.Cut(strings.TrimRight(l, "\r"), ":")
			if !ok {
				continue
			}
			v = yamlScalar(v)
			switch k {
			case "cwd":
				s.ProjectDir = v
			case "name":
				s.Title = oneLineTitle(v)
			}
		}
	}
	head, err := ep.readRange(ctx, s.transcript, 0, maxHeadRead)
	if err != nil {
		return
	}
	prompt := ""
	scanLines(head, func(l []byte) {
		var ev struct {
			Type string `json:"type"`
			Data struct {
				CopilotVersion string `json:"copilotVersion"`
				Context        struct {
					Cwd string `json:"cwd"`
				} `json:"context"`
				Content json.RawMessage `json:"content"`
			} `json:"data"`
		}
		if json.Unmarshal(l, &ev) != nil {
			return
		}
		switch ev.Type {
		case "session.start":
			if s.ProjectDir == "" {
				s.ProjectDir = ev.Data.Context.Cwd
			}
			if ev.Data.CopilotVersion != "" {
				s.Version = ev.Data.CopilotVersion
			}
		case "user.message":
			var c string
			if prompt == "" && json.Unmarshal(ev.Data.Content, &c) == nil {
				prompt = oneLineTitle(c)
			}
		}
	})
	if s.Title == "" {
		s.Title = prompt
	}
	if b, err := ep.readSmall(ctx, s.marker, 64<<10); err == nil {
		var hm handoffMark
		if json.Unmarshal(b, &hm) == nil && !s.Modified.After(hm.At.Add(2*time.Second)) {
			s.HandedOff = &hm
		}
	}
}

// yamlScalar is the value of a simple "key: value" YAML line.
func yamlScalar(v string) string {
	v = strings.TrimSpace(v)
	switch {
	case len(v) >= 2 && v[0] == '"':
		var s string
		if json.Unmarshal([]byte(v), &s) == nil {
			return s
		}
	case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	return v
}

func (copilotAdapter) loggedIn(ctx context.Context, ep *hoEndpoint, cfg string) (bool, bool) {
	b, err := ep.readSmall(ctx, ep.join(cfg, "config.json"), 4<<20)
	if errors.Is(err, fs.ErrNotExist) {
		return false, true
	}
	if err != nil {
		return false, false
	}
	in := bytes.Contains(b, []byte(`"lastLoggedInUser"`)) || bytes.Contains(b, []byte(`"loggedInUsers"`))
	clear(b)
	return in, in // not logged in there may still mean a token in the environment
}

func (copilotAdapter) mcpServers(ctx context.Context, ep *hoEndpoint, cfg, _ string) []string {
	b, err := ep.readSmall(ctx, ep.join(cfg, "mcp-config.json"), 8<<20)
	if err != nil {
		return nil
	}
	var mc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	json.Unmarshal(b, &mc)
	clear(b)
	var names []string
	for n := range mc.MCPServers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// copilotOpenWindow: an open-sessions-state.json entry refreshed this
// recently counts as a running Copilot CLI (it refreshes while open).
const copilotOpenWindow = 3 * time.Minute

func (copilotAdapter) inUse(ctx context.Context, ep *hoEndpoint, s *agentSession) string {
	b, err := ep.readSmall(ctx, ep.join(s.cfg, "open-sessions-state.json"), 4<<20)
	if err != nil {
		return ""
	}
	var open map[string]struct {
		RefreshedAt time.Time `json:"refreshedAt"`
	}
	if json.Unmarshal(b, &open) != nil {
		return ""
	}
	if e, ok := open[s.ID]; ok && time.Since(e.RefreshedAt) < copilotOpenWindow {
		return "a Copilot CLI (open-sessions-state.json refreshed " + humanDuration(time.Since(e.RefreshedAt)) + " ago)"
	}
	return ""
}

// ---------------------------------------------------------------- helpers

// rewriteKind is how a session file is rewritten: JSON (lines) or as is.
func rewriteKind(rel string) string {
	if strings.HasSuffix(rel, ".jsonl") || strings.HasSuffix(rel, ".json") {
		return "json"
	}
	return ""
}

// filepath2native turns a '/'-separated relative path into ep's form.
func filepath2native(ep *hoEndpoint, rel string) string { return ep.native(rel) }

func slicesCompact(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// versionRE finds a version number in `<agent> --version` output.
var versionRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// olderVersion reports whether version a is older than b (unknown: false).
func olderVersion(a, b string) bool {
	ma, mb := versionRE.FindStringSubmatch(a), versionRE.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return x < y
		}
	}
	return false
}
