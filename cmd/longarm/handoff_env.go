package main

// The session's environment in longarm handoff (docs/usage.md "Hand off an
// agent session"): of the MCP servers, skills and plugins of the source,
// those the session used (its transcripts say which) and the target lacks,
// and Claude Code's memory, go with it - for that session only. They land
// in the session's own directory on the target, <config>/longarm-handoff/<id>/
// (the project memory: in the new worktree's project directory), and the
// resumed agent loads them with per-run options (Claude Code: --mcp-config,
// --plugin-dir, --add-dir, --append-system-prompt-file; Copilot CLI:
// --additional-mcp-config, --plugin-dir, --add-dir); the target's own
// configuration, skills and plugins are not touched. MCP configurations go
// as they are, credentials in env and headers included, in files only the
// target account can read. What cannot be carried is a warning: the session
// moved already.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// envNameRE: skill and plugin names that may become a directory name on the target.
var envNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

// envUse is what of its agent's environment a session used, read from its
// transcripts while they are copied.
type envUse struct {
	mcp     map[string]bool // MCP servers, by the name the transcript has
	skills  map[string]bool // skills: "name", or Claude Code's "plugin:name"
	plugins map[string]bool // plugins asked for by name (run --plugin)
}

func newEnvUse() *envUse {
	return &envUse{mcp: map[string]bool{}, skills: map[string]bool{}, plugins: map[string]bool{}}
}

// claudeCommandRE: a slash command (a skill can be one) in a Claude Code transcript.
var claudeCommandRE = regexp.MustCompile(`<command-name>/?([A-Za-z0-9][A-Za-z0-9_.:-]*)</command-name>`)

// scan reads one transcript (JSON lines) of agent.
func (u *envUse) scan(agent string, b []byte) {
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			b = nil
		}
		if agent == "claude" {
			u.claudeLine(line)
		} else {
			u.copilotLine(line)
		}
	}
}

// claudeLine: tool calls mcp__<server>__<tool> (a plugin's server:
// mcp__plugin_<plugin>_<server>__<tool>), Skill calls and slash commands.
func (u *envUse) claudeLine(line []byte) {
	if bytes.Contains(line, []byte("<command-name>")) {
		for _, m := range claudeCommandRE.FindAllSubmatch(line, -1) {
			u.skills[string(m[1])] = true
		}
	}
	if !bytes.Contains(line, []byte(`"tool_use"`)) {
		return
	}
	var e struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	var cs []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(line, &e) != nil || json.Unmarshal(e.Message.Content, &cs) != nil {
		return
	}
	for _, c := range cs {
		switch {
		case c.Type != "tool_use":
		case c.Name == "Skill":
			var in struct {
				Skill string `json:"skill"`
			}
			if json.Unmarshal(c.Input, &in) == nil && in.Skill != "" {
				u.skills[strings.TrimPrefix(in.Skill, "/")] = true
			}
		case strings.HasPrefix(c.Name, "mcp__"):
			if srv, _, ok := strings.Cut(c.Name[len("mcp__"):], "__"); ok && srv != "" {
				u.mcp[srv] = true
			}
		}
	}
}

// copilotLine: tool.execution_start events of MCP tools and of the skill tool.
func (u *envUse) copilotLine(line []byte) {
	if !bytes.Contains(line, []byte(`"tool.execution_start"`)) {
		return
	}
	var e struct {
		Type string `json:"type"`
		Data struct {
			ToolName            string          `json:"toolName"`
			Arguments           json.RawMessage `json:"arguments"`
			MCPServerName       string          `json:"mcpServerName"`
			MCPConfigServerName string          `json:"mcpConfigServerName"`
		} `json:"data"`
	}
	if json.Unmarshal(line, &e) != nil || e.Type != "tool.execution_start" {
		return
	}
	if n := cmp.Or(e.Data.MCPConfigServerName, e.Data.MCPServerName); n != "" {
		u.mcp[n] = true
	}
	if e.Data.ToolName == "skill" {
		var a struct {
			Skill string `json:"skill"`
		}
		if json.Unmarshal(e.Data.Arguments, &a) == nil && a.Skill != "" {
			u.skills[a.Skill] = true
		}
	}
}

// handoffEnv is what went with the session (handoff --json "environment").
type handoffEnv struct {
	Dir        string   `json:"dir"` // the session's own directory on the target
	MCPServers []string `json:"mcpServers,omitempty"`
	// of those, the ones whose configuration has credentials (env, headers)
	Credentials []string `json:"mcpCredentials,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	Plugins     []string `json:"plugins,omitempty"`
	// "project memory", "CLAUDE.md", Copilot's "copilot-instructions.md" / "<name>.instructions.md"
	Memory []string `json:"memory,omitempty"`
	// with --carry-settings: "settings.json", "settings.local.json" (Claude Code), "hooks"
	Settings []string `json:"settings,omitempty"`
	// what --carry-settings would have carried
	NotCarried []string `json:"notCarried,omitempty"`
	// a run's directory (longarm-env/<key>): when a run last used it
	Used time.Time         `json:"used,omitzero"`
	args []string          // the resume command's options that load them
	env  map[string]string // and its environment
}

// envTree is a directory of the source copied to the target.
type envTree struct {
	what, name string // "skill" / "plugin" / "memory", for the output
	src, dst   string
	files      []hoFile // nil: listed when copied
}

// envFile is a file made for the target (a configuration, merged or
// converted from the source's).
type envFile struct {
	what, name string // "memory" / "settings" / "hooks", for the output
	dst        string
	data       []byte
	rewrite    bool   // the source's paths become the target's
	root       string // the directory it may land in (default: the agent's)
	optIn      bool   // only with --carry-settings: hooks and settings
	detail     string // for "not carried": what is in it
}

// envPlan is what goes to the target.
type envPlan struct {
	mcp   map[string]json.RawMessage // MCP server configurations, by name
	trees []envTree
	files []envFile
}

// envRunDir holds the agent environments of runs (run --agent-env) in the
// agent's directory: <key>/, named by a hash of the content.
const envRunDir = "longarm-env"

// key names the plan's content (as it will be on dst, in pack): what it
// carries - the files by name, size and time, the configurations made -
// and for which agent and home.
func (p *envPlan) key(ad agentAdapter, dst *hoEndpoint, pack string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", ad.name(), dst.home)
	rel := func(p string) string { return strings.TrimPrefix(p, pack) }
	for _, t := range p.trees {
		fmt.Fprintf(h, "tree\x00%s\x00%s\x00%s\x00", t.what, t.name, rel(t.dst))
		for _, f := range t.files {
			fmt.Fprintf(h, "%s\x00%d\x00%d\x00%o\x00", f.rel, f.size, f.mtime.UnixNano(), f.mode)
		}
	}
	b, _ := json.Marshal(p.mcp)
	h.Write(b)
	for _, f := range p.files {
		fmt.Fprintf(h, "file\x00%s\x00%s\x00%d\x00", f.what, rel(f.dst), len(f.data))
		h.Write(f.data)
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// envPack is the session's own directory on the target.
func envPack(dst *hoEndpoint, dstCfg, id string) string {
	return dst.join(dstCfg, "longarm-handoff", id)
}

func epExists(ctx context.Context, ep *hoEndpoint, p string) bool {
	if p == "" {
		return false
	}
	_, ok, err := ep.stat(ctx, p)
	return err == nil && ok
}

// mcpConfig reads the mcpServers of a JSON file (nil when missing or p is "").
func mcpConfig(ctx context.Context, ep *hoEndpoint, p string) map[string]json.RawMessage {
	if p == "" {
		return nil
	}
	b, err := ep.readSmall(ctx, p, 8<<20)
	if err != nil {
		return nil
	}
	var mc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	json.Unmarshal(b, &mc)
	clear(b)
	return mc.MCPServers
}

// claudeToolName is Claude Code's spelling of an MCP server in tool names.
func claudeToolName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

// claudePlugins are the plugins Claude Code has installed (plugins/installed_plugins.json):
// name -> install directory; the project-scoped ones of projectDir only.
func claudePlugins(ctx context.Context, ep *hoEndpoint, cfg, projectDir string) map[string]string {
	out := map[string]string{}
	b, err := ep.readSmall(ctx, ep.join(cfg, "plugins", "installed_plugins.json"), 8<<20)
	if err != nil {
		return out
	}
	var ip struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	if json.Unmarshal(b, &ip) != nil {
		return out
	}
	type entry struct {
		Scope       string `json:"scope"`
		InstallPath string `json:"installPath"`
		ProjectPath string `json:"projectPath"`
	}
	for key, raw := range ip.Plugins {
		name, _, _ := strings.Cut(key, "@")
		var es []entry
		if json.Unmarshal(raw, &es) != nil {
			var e entry
			if json.Unmarshal(raw, &e) != nil {
				continue
			}
			es = []entry{e}
		}
		for _, e := range es {
			if e.InstallPath == "" || (e.Scope == "project" || e.Scope == "local") && (projectDir == "" || !ep.samePath(e.ProjectPath, projectDir)) {
				continue
			}
			out[name] = e.InstallPath
		}
	}
	return out
}

// planClaudeEnv: the session's MCP servers of ~/.claude.json (a project's
// .mcp.json travels with the code), its personal skills, the plugins of
// its plugin skills and plugin MCP servers, its project memory and the
// user's CLAUDE.md - what the target lacks of them. srcPack is the
// session's own directory on the source, from an earlier handoff.
func planClaudeEnv(ctx context.Context, src, dst *hoEndpoint, t envTarget, pack, srcPack string, sess *agentSession, use *envUse, warn *warnings) *envPlan {
	dstProject, dstCwd := t.project, t.cwd
	in := t.in(dst)
	p := &envPlan{mcp: map[string]json.RawMessage{}}
	type conf struct {
		name string
		raw  json.RawMessage
	}
	confs := map[string]conf{} // by the name in tool names
	for n, c := range mcpConfig(ctx, src, src.join(srcPack, "mcp.json")) {
		confs[claudeToolName(n)] = conf{n, c}
	}
	if b, err := src.readSmall(ctx, claudeJSONPath(src, sess.cfg), 32<<20); err == nil {
		var cj struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
			Projects   map[string]struct {
				MCPServers map[string]json.RawMessage `json:"mcpServers"`
			} `json:"projects"`
		}
		json.Unmarshal(b, &cj)
		clear(b)
		for n, c := range cj.MCPServers {
			confs[claudeToolName(n)] = conf{n, c}
		}
		for dir, pr := range cj.Projects {
			if src.samePath(dir, sess.ProjectDir) {
				for n, c := range pr.MCPServers {
					confs[claudeToolName(n)] = conf{n, c}
				}
			}
		}
	}
	skip := map[string]bool{} // the code's .mcp.json, and the target's own
	for n := range mcpConfig(ctx, src, src.join(sess.ProjectDir, ".mcp.json")) {
		skip[claudeToolName(n)] = true
	}
	if t.has != "" {
		for _, n := range (claudeAdapter{}).mcpServers(ctx, dst, t.has, "") {
			skip[claudeToolName(n)] = true
		}
	}
	plugins := claudePlugins(ctx, src, sess.cfg, sess.ProjectDir)
	if ls, err := src.list(ctx, src.join(srcPack, "plugins")); err == nil {
		for _, f := range ls {
			if f.isDir && !strings.Contains(f.rel, "/") {
				if _, ok := plugins[f.rel]; !ok {
					plugins[f.rel] = src.join(srcPack, "plugins", f.rel)
				}
			}
		}
	}
	dstPlugins := map[string]string{}
	if t.has != "" {
		dstPlugins = claudePlugins(ctx, dst, t.has, "")
	}
	used := map[string]bool{} // plugins
	for pl := range use.plugins {
		if _, ok := plugins[pl]; ok {
			used[pl] = true
		}
	}
	for _, srv := range sortedKeys(use.mcp) {
		if rest, ok := strings.CutPrefix(srv, "plugin_"); ok {
			best := ""
			for pl := range plugins {
				if strings.HasPrefix(rest, claudeToolName(pl)+"_") && len(pl) > len(best) {
					best = pl
				}
			}
			if best != "" {
				used[best] = true
			}
			continue
		}
		if c, ok := confs[srv]; ok && !skip[srv] {
			p.mcp[c.name] = c.raw
		}
	}
	for _, sk := range sortedKeys(use.skills) {
		if pl, _, ok := strings.Cut(sk, ":"); ok {
			if _, ok := plugins[pl]; ok {
				used[pl] = true
			}
			continue
		}
		if !envNameRE.MatchString(sk) || epExists(ctx, src, src.join(sess.ProjectDir, ".claude", "skills", sk, "SKILL.md")) ||
			epExists(ctx, dst, in("skills", sk, "SKILL.md")) {
			continue // not a skill name, a project skill (it travels), or the target's own
		}
		for _, dir := range []string{src.join(sess.cfg, "skills", sk), src.join(srcPack, ".claude", "skills", sk)} {
			if epExists(ctx, src, src.join(dir, "SKILL.md")) {
				p.trees = append(p.trees, envTree{what: "skill", name: sk, src: dir, dst: dst.join(pack, ".claude", "skills", sk)})
				break
			}
		}
	}
	for _, pl := range sortedKeys(used) {
		if _, ok := dstPlugins[pl]; ok {
			continue
		}
		dir := plugins[pl]
		if !envNameRE.MatchString(pl) || !(src.within(dir, src.join(sess.cfg, "plugins")) || src.within(dir, srcPack)) {
			warn.add("plugin %s of the session is installed outside %s: not carried", pl, src.join(sess.cfg, "plugins"))
			continue
		}
		p.trees = append(p.trees, envTree{what: "plugin", name: pl, src: dir, dst: dst.join(pack, "plugins", pl)})
	}
	if dstProject != "" && sess.transcript != "" && t.want("memory") {
		p.trees = append(p.trees, envTree{what: "memory", name: "project memory", src: src.join(src.dir(sess.transcript), "memory"), dst: dst.join(dstProject, "memory")})
	}
	for _, f := range []string{src.join(srcPack, "CLAUDE.md"), src.join(sess.cfg, "CLAUDE.md")} {
		if !t.want("memory") {
			break
		}
		b, err := src.readSmall(ctx, f, 1<<20)
		if err != nil || len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		if have, err := readTarget(ctx, dst, in("CLAUDE.md")); err != nil || !bytes.Equal(bytes.TrimSpace(have), bytes.TrimSpace(b)) {
			p.files = append(p.files, envFile{what: "memory", name: "CLAUDE.md", dst: dst.join(pack, "CLAUDE.md"), data: b})
		}
		break
	}

	if !t.want("settings") {
		return p
	}
	// settings.json (hooks, permissions, env, ...): what differs from the
	// target's, loaded with --settings (over the target's own)
	set := jsonObject(ctx, src, src.join(sess.cfg, "settings.json"))
	for k, v := range jsonObject(ctx, src, src.join(srcPack, "settings.json")) { // an earlier hop's
		if k == "hooks" {
			v = mergeHooks(set[k], v)
		}
		set[k] = v
	}
	delete(set, "enabledPlugins") // plugins go as above
	delete(set, "extraKnownMarketplaces")
	if diff := settingsDiff(set, jsonObject(ctx, dst, in("settings.json"))); len(diff) > 0 {
		b, _ := json.MarshalIndent(diff, "", "  ")
		p.files = append(p.files, envFile{what: "settings", name: "settings.json", dst: dst.join(pack, "settings.json"), data: b,
			rewrite: true, optIn: true, detail: settingsDetail(diff)})
	}
	// the project's settings.local.json (your answers to its permission
	// prompts): git ignores it, so it does not travel with the code
	local := src.join(sess.ProjectDir, ".claude", "settings.local.json")
	if b, err := src.readSmall(ctx, local, 1<<20); err == nil && dstCwd != "" {
		if target := dst.join(dstCwd, ".claude", "settings.local.json"); !epExists(ctx, dst, target) {
			var m map[string]json.RawMessage
			json.Unmarshal(b, &m)
			p.files = append(p.files, envFile{what: "settings", name: "settings.local.json", dst: target, data: b, rewrite: true,
				root: dstCwd, optIn: true, detail: settingsDetail(m)})
		}
	}
	return p
}

// readTarget reads a small file of the target's configuration ("": none).
func readTarget(ctx context.Context, ep *hoEndpoint, p string) ([]byte, error) {
	if p == "" {
		return nil, os.ErrNotExist
	}
	return ep.readSmall(ctx, p, 1<<20)
}

// jsonObject reads a JSON object (empty when missing, not one, or p is "").
func jsonObject(ctx context.Context, ep *hoEndpoint, p string) map[string]json.RawMessage {
	m := map[string]json.RawMessage{}
	if p == "" {
		return m
	}
	if b, err := ep.readSmall(ctx, p, 4<<20); err == nil {
		json.Unmarshal(stripJSONComments(b), &m)
		clear(b)
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m
}

// stripJSONComments drops whole-line // comments (Copilot's config.json has them).
func stripJSONComments(b []byte) []byte {
	if !bytes.Contains(b, []byte("//")) {
		return b
	}
	var out [][]byte
	for _, l := range bytes.Split(b, []byte("\n")) {
		if !bytes.HasPrefix(bytes.TrimSpace(l), []byte("//")) {
			out = append(out, l)
		}
	}
	return bytes.Join(out, []byte("\n"))
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(x, y)
}

// hookEntries are the entries of a hooks object (event -> entries).
func hookEntries(raw json.RawMessage) map[string][]json.RawMessage {
	m := map[string][]json.RawMessage{}
	json.Unmarshal(raw, &m)
	return m
}

// mergeHooks is the entries of a and those of b that a lacks.
func mergeHooks(a, b json.RawMessage) json.RawMessage {
	m := hookEntries(a)
	for ev, es := range hookEntries(b) {
		for _, e := range es {
			if !slices.ContainsFunc(m[ev], func(x json.RawMessage) bool { return jsonEqual(x, e) }) {
				m[ev] = append(m[ev], e)
			}
		}
	}
	out, _ := json.Marshal(m)
	return out
}

// hooksDiff is the entries of src the target's hooks lack.
func hooksDiff(src, dst json.RawMessage) map[string][]json.RawMessage {
	have := hookEntries(dst)
	out := map[string][]json.RawMessage{}
	for ev, es := range hookEntries(src) {
		for _, e := range es {
			if !slices.ContainsFunc(have[ev], func(x json.RawMessage) bool { return jsonEqual(x, e) }) {
				out[ev] = append(out[ev], e)
			}
		}
	}
	return out
}

// settingsDiff is what of the source's settings the target's lack or has
// otherwise: the keys with another value, and the hooks it doesn't have.
func settingsDiff(src, dst map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range src {
		if k == "hooks" {
			if h := hooksDiff(v, dst[k]); len(h) > 0 {
				out[k], _ = json.Marshal(h)
			}
		} else if d, ok := dst[k]; !ok || !jsonEqual(v, d) {
			out[k] = v
		}
	}
	return out
}

// settingsDetail names what settings hold: "hooks: PreToolUse, Stop; permissions, env".
func settingsDetail(m map[string]json.RawMessage) string {
	var parts []string
	if h := hookEntries(m["hooks"]); len(h) > 0 {
		parts = append(parts, "hooks: "+strings.Join(sortedKeys(h), ", "))
	}
	var keys []string
	for _, k := range sortedKeys(m) {
		if k != "hooks" && k != "$schema" {
			keys = append(keys, k)
		}
	}
	if len(keys) > 0 {
		parts = append(parts, strings.Join(keys, ", "))
	}
	return strings.Join(parts, "; ")
}

// planCopilotEnv: the session's MCP servers of ~/.copilot/mcp-config.json,
// its personal skills (~/.copilot/skills, ~/.agents/skills), and the
// installed plugins (installed-plugins/<marketplace>/<plugin>) its other
// skills and MCP servers came from - what the target lacks of them.
func planCopilotEnv(ctx context.Context, src, dst *hoEndpoint, t envTarget, pack, srcPack string, sess *agentSession, use *envUse, warn *warnings) *envPlan {
	in := t.in(dst)
	p := &envPlan{mcp: map[string]json.RawMessage{}}
	confs := map[string]json.RawMessage{}
	maps.Copy(confs, mcpConfig(ctx, src, src.join(srcPack, "mcp.json")))
	maps.Copy(confs, mcpConfig(ctx, src, src.join(sess.cfg, "mcp-config.json")))
	have := mcpConfig(ctx, dst, in("mcp-config.json"))

	// the plugins: installed-plugins/<marketplace>/<plugin>, and those of
	// an earlier handoff (<pack>/plugins/<plugin>); their skills by name
	type plugin struct {
		name, dir string // dir: on the source
		target    string // where the target would have it installed
	}
	var plugins []plugin
	skillOf := map[string]int{}
	scan := func(root string, depth int, mk func(parts []string) plugin) {
		ls, _ := src.list(ctx, root)
		at := map[string]int{}
		for _, f := range ls {
			parts := strings.Split(f.rel, "/")
			if f.isDir && len(parts) == depth {
				at[f.rel] = len(plugins)
				plugins = append(plugins, mk(parts))
			}
		}
		for _, f := range ls { // <plugin>/skills/<name>/SKILL.md
			parts := strings.Split(f.rel, "/")
			if n := len(parts); !f.isDir && n == depth+3 && parts[depth] == "skills" && parts[n-1] == "SKILL.md" {
				if i, ok := at[strings.Join(parts[:depth], "/")]; ok {
					if _, dup := skillOf[parts[depth+1]]; !dup {
						skillOf[parts[depth+1]] = i
					}
				}
			}
		}
	}
	root := src.join(sess.cfg, "installed-plugins")
	scan(root, 2, func(p []string) plugin {
		return plugin{name: p[1], dir: src.join(root, p[0], p[1]), target: in("installed-plugins", p[0], p[1])}
	})
	scan(src.join(srcPack, "plugins"), 1, func(p []string) plugin {
		return plugin{name: p[0], dir: src.join(srcPack, "plugins", p[0])}
	})
	used := map[int]bool{}
	var unresolved []string
	for _, srv := range sortedKeys(use.mcp) {
		if _, ok := have[srv]; ok {
			continue
		}
		if c, ok := confs[srv]; ok {
			p.mcp[srv] = c
			continue
		}
		unresolved = append(unresolved, srv)
	}
	if len(unresolved) > 0 { // a plugin's server (built-in ones are in no plugin)
		for i, pl := range plugins {
			ms := map[string]json.RawMessage{}
			maps.Copy(ms, mcpConfig(ctx, src, src.join(pl.dir, ".mcp.json")))
			maps.Copy(ms, mcpConfig(ctx, src, src.join(pl.dir, "plugin.json")))
			for _, srv := range unresolved {
				if _, ok := ms[srv]; ok {
					used[i] = true
				}
			}
		}
	}
	for _, sk := range sortedKeys(use.skills) {
		if !envNameRE.MatchString(sk) {
			continue
		}
		travels := false // a project skill goes with the code
		for _, d := range []string{".github", ".claude", ".agents"} {
			travels = travels || epExists(ctx, src, src.join(sess.ProjectDir, d, "skills", sk, "SKILL.md"))
		}
		// the target's own: in its Copilot home, or ~/.agents/skills (read from HOME, a private COPILOT_HOME or not)
		own := epExists(ctx, dst, in("skills", sk, "SKILL.md")) || epExists(ctx, dst, dst.join(dst.home, ".agents", "skills", sk, "SKILL.md"))
		if travels || own {
			continue
		}
		found := false
		for _, d := range []string{src.join(sess.cfg, "skills"), src.join(src.home, ".agents", "skills"), src.join(srcPack, ".github", "skills")} {
			if dir := src.join(d, sk); epExists(ctx, src, src.join(dir, "SKILL.md")) {
				p.trees = append(p.trees, envTree{what: "skill", name: sk, src: dir, dst: dst.join(pack, ".github", "skills", sk)})
				found = true
				break
			}
		}
		if i, ok := skillOf[sk]; ok && !found {
			used[i] = true
		}
	}
	for i, pl := range plugins {
		if use.plugins[pl.name] {
			used[i] = true
		}
	}
	seen := map[string]bool{}
	dstPlugins := map[string]bool{}
	if t.has != "" {
		dstPlugins = copilotPlugins(ctx, dst, t.has) // from any marketplace
	}
	for _, i := range sortedKeys(used) {
		pl := plugins[i]
		if seen[pl.name] || dstPlugins[pl.name] || epExists(ctx, dst, pl.target) {
			continue
		}
		seen[pl.name] = true
		if !envNameRE.MatchString(pl.name) {
			warn.add("plugin %q of the session: not a plugin name, not carried", pl.name)
			continue
		}
		p.trees = append(p.trees, envTree{what: "plugin", name: pl.name, src: pl.dir, dst: dst.join(pack, "plugins", pl.name)})
	}

	// the user's instructions (copilot-instructions.md, instructions/**/*.instructions.md)
	// the target has otherwise: Copilot loads .github/instructions/ of the
	// directories in COPILOT_CUSTOM_INSTRUCTIONS_DIRS
	instr := map[string][]byte{} // .instructions.md name -> content
	pi := src.join(srcPack, "instructions", ".github", "instructions")
	var ls []hoFile
	if t.want("memory") {
		ls, _ = src.list(ctx, pi)
	}
	for _, f := range ls {
		if !f.isDir && strings.HasSuffix(f.rel, ".instructions.md") {
			if b, err := src.readSmall(ctx, src.join(pi, f.rel), 1<<20); err == nil {
				instr[f.rel] = b
			}
		}
	}
	if t.want("memory") {
		ls, _ = src.list(ctx, src.join(sess.cfg, "instructions"))
		for _, f := range ls {
			if !f.isDir && strings.HasSuffix(f.rel, ".instructions.md") {
				if b, err := src.readSmall(ctx, src.join(sess.cfg, "instructions", f.rel), 1<<20); err == nil {
					instr[f.rel] = b
				}
			}
		}
		if b, err := src.readSmall(ctx, src.join(sess.cfg, "copilot-instructions.md"), 1<<20); err == nil {
			instr[copilotHomeInstructions] = append([]byte(copilotApplyAll), b...)
		}
	}
	for _, rel := range sortedKeys(instr) {
		b := instr[rel]
		name, have := rel, in("instructions", rel)
		body := b
		if rel == copilotHomeInstructions {
			name, have = "copilot-instructions.md", in("copilot-instructions.md")
			body = bytes.TrimPrefix(b, []byte(copilotApplyAll))
		}
		if len(bytes.TrimSpace(body)) == 0 || checkRel(rel, dst.windows()) != nil {
			continue
		}
		if h, err := readTarget(ctx, dst, have); err == nil && bytes.Equal(bytes.TrimSpace(h), bytes.TrimSpace(body)) {
			continue
		}
		p.files = append(p.files, envFile{what: "memory", name: name, dst: dst.join(pack, "instructions", ".github", "instructions", rel), data: b})
	}

	// the user's hooks (hooks/*.json) the target lacks, as a plugin of their own
	if !t.want("settings") {
		return p
	}
	hooks := jsonObject(ctx, src, src.join(srcPack, "plugins", copilotHooksPlugin, "hooks.json"))["hooks"]
	hookFiles := func(ep *hoEndpoint, cfg string) (all json.RawMessage) {
		if cfg == "" {
			return nil
		}
		ls, _ := ep.list(ctx, ep.join(cfg, "hooks"))
		for _, f := range ls {
			if !f.isDir && !strings.Contains(f.rel, "/") && strings.HasSuffix(f.rel, ".json") {
				all = mergeHooks(all, jsonObject(ctx, ep, ep.join(cfg, "hooks", f.rel))["hooks"])
			}
		}
		return all
	}
	hooks = mergeHooks(hooks, hookFiles(src, sess.cfg))
	if d := hooksDiff(hooks, hookFiles(dst, t.has)); len(d) > 0 {
		dir := dst.join(pack, "plugins", copilotHooksPlugin)
		pj, _ := json.MarshalIndent(map[string]any{"name": copilotHooksPlugin, "version": "1.0.0",
			"description": "the hooks of a session handed off by longarm", "hooks": "hooks.json"}, "", "  ")
		hb, _ := json.MarshalIndent(map[string]any{"version": 1, "hooks": d}, "", "  ")
		p.files = append(p.files,
			envFile{what: "hooks", name: "plugin.json", dst: dst.join(dir, "plugin.json"), data: pj, optIn: true},
			envFile{what: "hooks", name: "hooks.json", dst: dst.join(dir, "hooks.json"), data: hb, rewrite: true, optIn: true,
				detail: "hooks: " + strings.Join(sortedKeys(d), ", ")})
	}
	return p
}

const (
	// copilotHooksPlugin is the plugin of the user's hooks carried for a Copilot session.
	copilotHooksPlugin = "longarm-handoff-hooks"
	// copilotHomeInstructions is ~/.copilot/copilot-instructions.md as an
	// instructions file for every path.
	copilotHomeInstructions = "copilot-instructions.instructions.md"
	copilotApplyAll         = "---\napplyTo: \"**\"\n---\n"
)

// copilotPlugins are the names of the plugins Copilot CLI has installed
// (config.json installedPlugins), from whichever marketplace.
func copilotPlugins(ctx context.Context, ep *hoEndpoint, cfg string) map[string]bool {
	var ps []struct {
		Name string `json:"name"`
	}
	json.Unmarshal(jsonObject(ctx, ep, ep.join(cfg, "config.json"))["installedPlugins"], &ps)
	out := map[string]bool{}
	for _, p := range ps {
		out[p.Name] = true
	}
	return out
}

func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	ks := make([]K, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// carryEnv puts what the session used of its agent's environment on the
// target (see the top of this file) and returns it with the resume
// command's options that load it; nil when there is nothing to carry.
// t.keyed (a run): the directory is named by its content and reused.
func carryEnv(ctx context.Context, ad agentAdapter, src, dst *hoEndpoint, t envTarget, sess *agentSession,
	pms []pathMap, use *envUse, warn *warnings) *handoffEnv {
	dstCfg, limit := t.cfg, t.limit
	if !t.keyed && t.has == "" { // a handoff: the agent resumes with the target's own configuration
		t.has = dstCfg
	}
	pack := envPack(dst, dstCfg, sess.ID)
	srcPack := envPack(src, sess.cfg, sess.ID)
	if t.keyed { // written aside, then renamed to its key
		pack = dst.join(dstCfg, envRunDir, fmt.Sprintf(".staging-%d-%d", time.Now().UnixNano(), os.Getpid()))
	}
	if sess.ID == "" { // a run: no earlier handoff to take things from
		srcPack = src.join(sess.cfg, "longarm-handoff", ".none")
	}
	var p *envPlan
	if ad.name() == "claude" {
		p = planClaudeEnv(ctx, src, dst, t, pack, srcPack, sess, use, warn)
	} else {
		p = planCopilotEnv(ctx, src, dst, t, pack, srcPack, sess, use, warn)
	}
	// hooks and settings only when asked for: hooks run commands there
	var notCarried []string
	p.files = slices.DeleteFunc(p.files, func(f envFile) bool {
		if f.optIn && !t.settings {
			if label := f.name; f.detail != "" {
				if f.what == "hooks" {
					label = "hooks"
				}
				notCarried = append(notCarried, fmt.Sprintf("%s (%s)", label, f.detail))
			}
			return true
		}
		return false
	})
	if len(notCarried) > 0 {
		notef("not carried: %s - --carry-settings carries them for this session (hooks run commands on %s)", strings.Join(notCarried, "; "), dst.name)
	}
	// list the trees; an empty one (no project memory) is no tree
	trees := p.trees[:0]
	for _, t := range p.trees {
		ls, err := src.list(ctx, t.src)
		if err != nil {
			warn.add("%s %s: list %s on %s: %v", t.what, t.name, t.src, src.name, err)
			continue
		}
		for _, f := range ls {
			if !f.isDir && !slices.Contains(strings.Split(f.rel, "/"), ".git") {
				t.files = append(t.files, f)
			}
		}
		if len(t.files) > 0 {
			trees = append(trees, t)
		}
	}
	p.trees = trees
	if len(p.mcp) == 0 && len(p.trees) == 0 && len(p.files) == 0 {
		if len(notCarried) > 0 {
			return &handoffEnv{NotCarried: notCarried}
		}
		return nil
	}

	final := pack
	if t.keyed {
		final = dst.join(dstCfg, envRunDir, p.key(ad, dst, pack))
		if b, err := readTarget(ctx, dst, dst.join(final, "env.json")); err == nil { // carried by an earlier run
			env := &handoffEnv{}
			if json.Unmarshal(b, env) == nil {
				env.Dir, env.Used = final, time.Now().UTC()
				rec, _ := json.MarshalIndent(env, "", "  ")
				dst.writeFile(ctx, dst.join(final, "env.json"), rec) // when it was last used: handoff --cleanup
				env.args, env.env = envArgs(ad, dst, final, env)
				notef("agent environment for this run: %s (on %s already, %s)", env.summary(), dst.name, final)
				return env
			}
		}
	}

	// keep what the target has there already (an earlier handoff of this session)
	stamp := time.Now().UTC().Format("20060102T150405Z")
	backup := func(path, as string) bool {
		if _, ok, err := dst.stat(ctx, path); err != nil || !ok {
			return err == nil
		}
		bk := dst.join(dstCfg, "longarm-handoff-backup", sess.ID+"-"+stamp, as)
		if err := dst.rename(ctx, path, bk); err != nil {
			warn.add("could not move the earlier %s on %s aside: %v; the session's environment was not carried", path, dst.name, err)
			return false
		}
		notef("kept the earlier %s on %s as %s", path, dst.name, bk)
		return true
	}
	if !t.keyed && !backup(pack, "env") {
		return nil
	}
	env := &handoffEnv{Dir: pack, NotCarried: notCarried}
	links := newLinkChecker(dst, dstCfg)
	// paths of the source (its home, the worktree, the agent's config) become the target's
	rw := newPathRewriter(append(slices.Clone(pms), pathMap{from: src.home, to: dst.home, fromWin: src.windows(), toWin: dst.windows()}))
	tmp, err := os.MkdirTemp("", "longarm-handoff-env-")
	if err != nil {
		warn.add("the session's environment: %v", err)
		return nil
	}
	defer os.RemoveAll(tmp) // MCP configurations may hold credentials: no copy stays here
	budget, nfiles, idx := limit, 0, 0
	put := func(target string, data []byte) error {
		if err := links.check(ctx, target); err != nil {
			return err
		}
		return dst.writeFile(ctx, target, data)
	}

	for _, t := range p.trees {
		var size int64
		bad := ""
		for _, f := range t.files {
			size += f.size
			if err := checkRel(f.rel, dst.windows()); err != nil {
				bad = err.Error()
			}
		}
		switch {
		case bad != "":
			warn.add("%s %s on %s: %s; not carried", t.what, t.name, src.name, bad)
			continue
		case size > budget || nfiles+len(t.files) > maxHandoffFiles:
			warn.add("%s %s (%s, %d files) is over what handoff carries (--max-size %s): not carried", t.what, t.name, humanSize(size), len(t.files), humanSize(limit))
			continue
		}
		if t.what == "memory" && !backup(t.dst, "memory") {
			continue
		}
		var err error
		for _, f := range t.files {
			target := dst.join(t.dst, f.rel)
			if err = links.check(ctx, target); err != nil {
				break
			}
			// named by a counter in the private directory: a source's names never reach a path here
			idx++
			local := filepath.Join(tmp, strconv.Itoa(idx))
			var n int64
			if n, err = src.download(ctx, src.join(t.src, f.rel), local, min(f.size, budget)); err != nil {
				break
			}
			budget -= n
			if err = dst.uploadMode(ctx, local, target, envFileMode(src, f, local)); err != nil {
				break
			}
			os.Remove(local)
		}
		nfiles += len(t.files)
		if err != nil {
			warn.add("%s %s: %v; carried in part (%s)", t.what, t.name, err, t.dst)
			continue
		}
		switch t.what {
		case "skill":
			env.Skills = append(env.Skills, t.name)
		case "plugin":
			env.Plugins = append(env.Plugins, t.name)
		default:
			env.Memory = append(env.Memory, t.name)
		}
	}

	if len(p.mcp) > 0 {
		b, _ := json.MarshalIndent(map[string]any{"mcpServers": p.mcp}, "", "  ")
		nb, _ := rw.rewriteJSON(b)
		clear(b)
		if err := put(dst.join(pack, "mcp.json"), nb); err != nil {
			warn.add("MCP servers %s: %v; not carried", strings.Join(sortedKeys(p.mcp), ", "), err)
		} else {
			var mc struct {
				MCPServers map[string]struct {
					Command string            `json:"command"`
					Env     map[string]string `json:"env"`
					Headers map[string]string `json:"headers"`
				} `json:"mcpServers"`
			}
			json.Unmarshal(nb, &mc)
			for _, n := range sortedKeys(p.mcp) {
				env.MCPServers = append(env.MCPServers, n)
				s := mc.MCPServers[n]
				if len(s.Env) > 0 || len(s.Headers) > 0 {
					env.Credentials = append(env.Credentials, n)
				}
				if c := s.Command; isAbsAny(c) && !epExists(ctx, dst, c) {
					warn.add("MCP server %s runs %s, which %s does not have: install it there", n, c, dst.name)
				}
			}
		}
		clear(nb)
	}
	hooksOK := true // the hooks plugin is two files: both or none
	for _, f := range p.files {
		if f.what == "hooks" && !hooksOK {
			continue
		}
		data := f.data
		if f.rewrite {
			data, _ = rw.rewriteJSON(data)
		}
		lc := links
		if f.root != "" {
			lc = newLinkChecker(dst, f.root)
		}
		err := lc.check(ctx, f.dst)
		if err == nil {
			err = dst.writeFile(ctx, f.dst, data)
		}
		if err != nil {
			warn.add("%s %s: %v; not carried", f.what, f.name, err)
			hooksOK = hooksOK && f.what != "hooks"
			continue
		}
		switch {
		case f.what == "memory":
			env.Memory = append(env.Memory, f.name)
		case f.what == "settings":
			env.Settings = append(env.Settings, f.name)
		case f.name == "hooks.json":
			env.Settings = append(env.Settings, "hooks")
		}
		if f.what != "memory" {
			for _, m := range missingPaths(ctx, dst, data) {
				warn.add("%s %s runs %s, which %s does not have", f.what, f.name, m, dst.name)
			}
		}
	}
	if len(env.MCPServers)+len(env.Skills)+len(env.Plugins)+len(env.Memory)+len(env.Settings) == 0 {
		if t.keyed {
			dst.removeTree(ctx, pack)
		}
		if len(notCarried) > 0 {
			return &handoffEnv{NotCarried: notCarried}
		}
		return nil
	}
	if t.keyed {
		env.Used = time.Now().UTC()
	}
	rec, _ := json.MarshalIndent(env, "", "  ")
	if err := put(dst.join(pack, "env.json"), rec); err != nil {
		warn.add("record of the session's environment: %v", err)
	}
	if t.keyed {
		// another run may have put the same there meanwhile: then that one is it
		if err := dst.rename(ctx, pack, final); err != nil {
			dst.removeTree(ctx, pack)
			if !epExists(ctx, dst, dst.join(final, "env.json")) {
				warn.add("the agent environment on %s: %v; not carried", dst.name, err)
				return nil
			}
		}
		env.Dir = final
		env.args, env.env = envArgs(ad, dst, final, env)
		notef("agent environment for this run (%s on %s): %s", final, dst.name, env.summary())
	} else {
		env.args, env.env = envArgs(ad, dst, pack, env)
		notef("carried for this session only (%s on %s): %s", pack, dst.name, env.summary())
	}
	if len(env.Credentials) > 0 {
		if t.shared {
			warn.add("MCP servers with credentials in their configuration went to %s as they are: %s - other users' jobs there can read them", dst.name, strings.Join(env.Credentials, ", "))
		} else {
			notef("MCP servers with credentials in their configuration went as they are (readable by your account on %s only): %s", dst.name, strings.Join(env.Credentials, ", "))
		}
	}
	if t.shared && len(env.Settings) > 0 {
		warn.add("your %s (their env may hold credentials) went to %s - other users' jobs there can read them", strings.Join(env.Settings, ", "), dst.name)
	}
	return env
}

// envTarget is where the session goes on the target, and how.
type envTarget struct {
	cfg, project, cwd string // the agent's config and project directories, the session's working directory
	// has is the configuration the agent uses there, what it has already:
	// cfg, or "" for a fresh one (a run's private COPILOT_HOME)
	has      string
	shared   bool            // the target keeps every user's agent state in one account
	settings bool            // --carry-settings
	limit    int64           // --max-size, for each part
	only     map[string]bool // nil: all; else of memory, settings (a run's --agent-env)
	// keyed: the directory is <cfg>/longarm-env/<a hash of its content>,
	// shared by the runs that carry the same (run --agent-env)
	keyed bool
}

// in is a path in the configuration the agent uses on the target ("" for a fresh one).
func (t envTarget) in(dst *hoEndpoint) func(elem ...string) string {
	return func(elem ...string) string {
		if t.has == "" {
			return ""
		}
		return dst.join(append([]string{t.has}, elem...)...)
	}
}

func (t envTarget) want(kind string) bool { return t.only == nil || t.only[kind] }

// envArgs are the resume command's options and environment that load the
// session's environment.
func envArgs(ad agentAdapter, dst *hoEndpoint, pack string, env *handoffEnv) ([]string, map[string]string) {
	var a []string
	mcp := dst.join(pack, "mcp.json")
	if ad.name() == "claude" {
		if len(env.MCPServers) > 0 {
			a = append(a, "--mcp-config", mcp)
		}
		for _, pl := range env.Plugins {
			a = append(a, "--plugin-dir", dst.join(pack, "plugins", pl))
		}
		if len(env.Skills) > 0 { // <pack>/.claude/skills
			a = append(a, "--add-dir", pack)
		}
		if slices.Contains(env.Memory, "CLAUDE.md") {
			a = append(a, "--append-system-prompt-file", dst.join(pack, "CLAUDE.md"))
		}
		if slices.Contains(env.Settings, "settings.json") {
			a = append(a, "--settings", dst.join(pack, "settings.json"))
		}
		return a, nil
	}
	if len(env.MCPServers) > 0 {
		a = append(a, "--additional-mcp-config", "@"+mcp)
	}
	for _, pl := range env.Plugins {
		a = append(a, "--plugin-dir", dst.join(pack, "plugins", pl))
	}
	if slices.Contains(env.Settings, "hooks") {
		a = append(a, "--plugin-dir", dst.join(pack, "plugins", copilotHooksPlugin))
	}
	if len(env.Skills) > 0 { // <pack>/.github/skills
		a = append(a, "--add-dir", pack)
	}
	if len(env.Memory) > 0 { // <pack>/instructions/.github/instructions
		return a, map[string]string{"COPILOT_CUSTOM_INSTRUCTIONS_DIRS": dst.join(pack, "instructions")}
	}
	return a, nil
}

// missingPaths are the absolute paths in the home directory of dst that a
// carried configuration names (hook commands, a status line) and dst lacks.
func missingPaths(ctx context.Context, dst *hoEndpoint, data []byte) []string {
	var v any
	if json.Unmarshal(data, &v) != nil || dst.home == "" {
		return nil
	}
	homes := []string{dst.home, strings.ReplaceAll(dst.home, `\`, "/")}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for _, k := range sortedKeys(x) {
				walk(x[k])
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case string:
			for _, h := range homes {
				for rest := x; len(out) < 10; {
					i := strings.Index(rest, h)
					if i < 0 {
						break
					}
					p := rest[i:]
					if j := strings.IndexAny(p, " \t\"'`;|&<>"); j >= 0 {
						p = p[:j]
					}
					rest = rest[i+len(h):]
					if p != h && !slices.Contains(out, p) && !epExists(ctx, dst, p) {
						out = append(out, p)
					}
				}
			}
		}
	}
	walk(v)
	return out
}

func (e *handoffEnv) summary() string {
	var parts []string
	for _, x := range []struct {
		what  string
		names []string
	}{{"MCP servers", e.MCPServers}, {"skills", e.Skills}, {"plugins", e.Plugins}, {"memory", e.Memory}, {"settings", e.Settings}} {
		if len(x.names) > 0 {
			parts = append(parts, x.what+" "+strings.Join(x.names, ", "))
		}
	}
	return strings.Join(parts, "; ")
}

// envDirState is a session's own directory handoff --cleanup found.
type envDirState struct {
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Dir     string `json:"dir"`
	Removed bool   `json:"removed"`
	Reason  string `json:"reason"` // why removed or kept, or what failed
}

// pruneEnvDirs removes the session directories (<agent dir>/longarm-handoff/<id>/)
// of the sessions that moved on from ep (handed off) or are gone; those of
// sessions that are here stay.
func pruneEnvDirs(ctx context.Context, ep *hoEndpoint) []envDirState {
	var out []envDirState
	for _, ad := range allAdapters {
		cfg := ad.configDir(ctx, ep)
		root := ep.join(cfg, "longarm-handoff")
		ls, err := ep.list(ctx, root)
		if err != nil {
			out = append(out, envDirState{Agent: ad.name(), Dir: root, Reason: oneLine(err)})
			continue
		}
		for _, f := range ls {
			if !f.isDir || strings.Contains(f.rel, "/") || !envNameRE.MatchString(f.rel) {
				continue
			}
			st := envDirState{Agent: ad.name(), Session: f.rel, Dir: ep.join(root, f.rel)}
			sessions, err := ad.list(ctx, ep, cfg, f.rel, false, nil)
			switch {
			case err != nil:
				st.Reason = oneLine(err)
			case slices.ContainsFunc(sessions, func(s *agentSession) bool { return s.HandedOff == nil }):
				st.Reason = "the session is here"
			default:
				st.Reason = "no such session here"
				if len(sessions) > 0 {
					st.Reason = "handed off to " + sessions[0].HandedOff.To
				}
				if err := ep.removeTree(ctx, st.Dir); err != nil {
					st.Reason = oneLine(err)
				} else {
					st.Removed = true
				}
			}
			out = append(out, st)
		}
		out = append(out, pruneRunEnvs(ctx, ep, ad, cfg)...)
	}
	return out
}

// envRunKeep: a run's agent environment (longarm-env/<key>) not used by a
// run for this long goes.
const envRunKeep = 7 * 24 * time.Hour

// pruneRunEnvs removes the agent environments of runs not used for
// envRunKeep, and what a run left half-written (.staging-*) a day ago.
func pruneRunEnvs(ctx context.Context, ep *hoEndpoint, ad agentAdapter, cfg string) []envDirState {
	root := ep.join(cfg, envRunDir)
	ls, err := ep.list(ctx, root)
	if err != nil {
		return []envDirState{{Agent: ad.name(), Dir: root, Reason: oneLine(err)}}
	}
	var out []envDirState
	for _, f := range ls {
		if !f.isDir || strings.Contains(f.rel, "/") {
			continue
		}
		st := envDirState{Agent: ad.name(), Dir: ep.join(root, f.rel)}
		last, what := f.mtime, "written"
		if b, err := readTarget(ctx, ep, ep.join(st.Dir, "env.json")); err == nil {
			var e handoffEnv
			if json.Unmarshal(b, &e) == nil && !e.Used.IsZero() {
				last, what = e.Used, "used"
			}
		}
		keep := envRunKeep
		if strings.HasPrefix(f.rel, ".staging-") {
			keep, what = 24*time.Hour, "left half-written"
		}
		age := time.Since(last).Round(time.Hour)
		if age < keep {
			st.Reason = fmt.Sprintf("a run's, %s %s ago", what, age)
		} else if err := ep.removeTree(ctx, st.Dir); err != nil {
			st.Reason = oneLine(err)
		} else {
			st.Removed, st.Reason = true, fmt.Sprintf("a run's, %s %s ago", what, age)
		}
		out = append(out, st)
	}
	return out
}

// envFileMode is the mode of a carried file on the target: 0700 when it is
// executable on the source - or, from Windows (no executable bit), starts
// with "#!" - so that a plugin's hook or server script still runs on a
// Unix target; else 0600.
func envFileMode(src *hoEndpoint, f hoFile, local string) fs.FileMode {
	if f.mode&0o111 != 0 {
		return 0o700
	}
	if src.windows() {
		if h, err := os.Open(local); err == nil {
			b := make([]byte, 2)
			n, _ := h.Read(b)
			h.Close()
			if n == 2 && string(b) == "#!" {
				return 0o700
			}
		}
	}
	return 0o600
}

// isAbsAny: an absolute path of any OS.
func isAbsAny(p string) bool {
	return strings.HasPrefix(p, "/") || driveRE.MatchString(p) || strings.HasPrefix(p, `\\`)
}
