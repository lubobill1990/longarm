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
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
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
	mcp    map[string]bool // MCP servers, by the name the transcript has
	skills map[string]bool // skills: "name", or Claude Code's "plugin:name"
}

func newEnvUse() *envUse { return &envUse{mcp: map[string]bool{}, skills: map[string]bool{}} }

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
	Memory      []string `json:"memory,omitempty"` // "project memory", "CLAUDE.md"
	args        []string // the resume command's options that load them
}

// envTree is a directory of the source copied to the target.
type envTree struct {
	what, name string // "skill" / "plugin" / "memory", for the output
	src, dst   string
	files      []hoFile // nil: listed when copied
}

// envPlan is what goes to the target.
type envPlan struct {
	mcp      map[string]json.RawMessage // MCP server configurations, by name
	trees    []envTree
	claudeMD []byte // Claude Code's user memory (CLAUDE.md)
}

// envPack is the session's own directory on the target.
func envPack(dst *hoEndpoint, dstCfg, id string) string {
	return dst.join(dstCfg, "longarm-handoff", id)
}

func epExists(ctx context.Context, ep *hoEndpoint, p string) bool {
	_, ok, err := ep.stat(ctx, p)
	return err == nil && ok
}

// mcpConfig reads the mcpServers of a JSON file (nil when missing).
func mcpConfig(ctx context.Context, ep *hoEndpoint, p string) map[string]json.RawMessage {
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
func planClaudeEnv(ctx context.Context, src, dst *hoEndpoint, dstCfg, dstProject, pack, srcPack string, sess *agentSession, use *envUse, warn *warnings) *envPlan {
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
	for _, n := range (claudeAdapter{}).mcpServers(ctx, dst, dstCfg, "") {
		skip[claudeToolName(n)] = true
	}
	plugins := claudePlugins(ctx, src, sess.cfg, sess.ProjectDir)
	if fs, err := src.list(ctx, src.join(srcPack, "plugins")); err == nil {
		for _, f := range fs {
			if f.isDir && !strings.Contains(f.rel, "/") {
				if _, ok := plugins[f.rel]; !ok {
					plugins[f.rel] = src.join(srcPack, "plugins", f.rel)
				}
			}
		}
	}
	dstPlugins := claudePlugins(ctx, dst, dstCfg, "")
	used := map[string]bool{} // plugins
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
			epExists(ctx, dst, dst.join(dstCfg, "skills", sk, "SKILL.md")) {
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
	if dstProject != "" {
		p.trees = append(p.trees, envTree{what: "memory", name: "project memory", src: src.join(src.dir(sess.transcript), "memory"), dst: dst.join(dstProject, "memory")})
	}
	for _, f := range []string{src.join(srcPack, "CLAUDE.md"), src.join(sess.cfg, "CLAUDE.md")} {
		b, err := src.readSmall(ctx, f, 1<<20)
		if err != nil || len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		if have, err := dst.readSmall(ctx, dst.join(dstCfg, "CLAUDE.md"), 1<<20); err != nil || !bytes.Equal(bytes.TrimSpace(have), bytes.TrimSpace(b)) {
			p.claudeMD = b
		}
		break
	}
	return p
}

// planCopilotEnv: the session's MCP servers of ~/.copilot/mcp-config.json,
// its personal skills (~/.copilot/skills, ~/.agents/skills), and the
// installed plugins (installed-plugins/<marketplace>/<plugin>) its other
// skills and MCP servers came from - what the target lacks of them.
func planCopilotEnv(ctx context.Context, src, dst *hoEndpoint, dstCfg, pack, srcPack string, sess *agentSession, use *envUse, warn *warnings) *envPlan {
	p := &envPlan{mcp: map[string]json.RawMessage{}}
	confs := map[string]json.RawMessage{}
	maps.Copy(confs, mcpConfig(ctx, src, src.join(srcPack, "mcp.json")))
	maps.Copy(confs, mcpConfig(ctx, src, src.join(sess.cfg, "mcp-config.json")))
	have := mcpConfig(ctx, dst, dst.join(dstCfg, "mcp-config.json"))

	// the plugins: installed-plugins/<marketplace>/<plugin>, and those of
	// an earlier handoff (<pack>/plugins/<plugin>); their skills by name
	type plugin struct {
		name, dir string // dir: on the source
		target    string // where the target would have it installed
	}
	var plugins []plugin
	skillOf := map[string]int{}
	scan := func(root string, depth int, mk func(parts []string) plugin) {
		fs, _ := src.list(ctx, root)
		at := map[string]int{}
		for _, f := range fs {
			parts := strings.Split(f.rel, "/")
			if f.isDir && len(parts) == depth {
				at[f.rel] = len(plugins)
				plugins = append(plugins, mk(parts))
			}
		}
		for _, f := range fs { // <plugin>/skills/<name>/SKILL.md
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
		return plugin{name: p[1], dir: src.join(root, p[0], p[1]), target: dst.join(dstCfg, "installed-plugins", p[0], p[1])}
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
	home := func(ep *hoEndpoint, cfg string) []string {
		return []string{ep.join(cfg, "skills"), ep.join(ep.home, ".agents", "skills")}
	}
	for _, sk := range sortedKeys(use.skills) {
		if !envNameRE.MatchString(sk) {
			continue
		}
		travels := false // a project skill goes with the code
		for _, d := range []string{".github", ".claude", ".agents"} {
			travels = travels || epExists(ctx, src, src.join(sess.ProjectDir, d, "skills", sk, "SKILL.md"))
		}
		own := false
		for _, d := range home(dst, dstCfg) {
			own = own || epExists(ctx, dst, dst.join(d, sk, "SKILL.md"))
		}
		if travels || own {
			continue
		}
		found := false
		for _, d := range append(home(src, sess.cfg), src.join(srcPack, ".github", "skills")) {
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
	seen := map[string]bool{}
	for _, i := range sortedKeys(used) {
		pl := plugins[i]
		if seen[pl.name] || (pl.target != "" && epExists(ctx, dst, pl.target)) {
			continue
		}
		seen[pl.name] = true
		if !envNameRE.MatchString(pl.name) {
			warn.add("plugin %q of the session: not a plugin name, not carried", pl.name)
			continue
		}
		p.trees = append(p.trees, envTree{what: "plugin", name: pl.name, src: pl.dir, dst: dst.join(pack, "plugins", pl.name)})
	}
	return p
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
func carryEnv(ctx context.Context, ad agentAdapter, src, dst *hoEndpoint, dstCfg, dstProject string, sess *agentSession,
	pms []pathMap, use *envUse, shared bool, limit int64, warn *warnings) *handoffEnv {
	pack := envPack(dst, dstCfg, sess.ID)
	srcPack := envPack(src, sess.cfg, sess.ID)
	var p *envPlan
	if ad.name() == "claude" {
		p = planClaudeEnv(ctx, src, dst, dstCfg, dstProject, pack, srcPack, sess, use, warn)
	} else {
		p = planCopilotEnv(ctx, src, dst, dstCfg, pack, srcPack, sess, use, warn)
	}
	// list the trees; an empty one (no project memory) is no tree
	trees := p.trees[:0]
	for _, t := range p.trees {
		fs, err := src.list(ctx, t.src)
		if err != nil {
			warn.add("%s %s: list %s on %s: %v", t.what, t.name, t.src, src.name, err)
			continue
		}
		for _, f := range fs {
			if !f.isDir && !slices.Contains(strings.Split(f.rel, "/"), ".git") {
				t.files = append(t.files, f)
			}
		}
		if len(t.files) > 0 {
			trees = append(trees, t)
		}
	}
	p.trees = trees
	if len(p.mcp) == 0 && len(p.trees) == 0 && p.claudeMD == nil {
		return nil
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
	if !backup(pack, "env") {
		return nil
	}
	env := &handoffEnv{Dir: pack}
	links := newLinkChecker(dst, dstCfg)
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
			if err = dst.upload(ctx, local, target); err != nil {
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
		// paths of the source (its home, the worktree, the agent's config) become the target's
		rw := newPathRewriter(append(slices.Clone(pms), pathMap{from: src.home, to: dst.home, fromWin: src.windows(), toWin: dst.windows()}))
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
	if p.claudeMD != nil {
		if err := put(dst.join(pack, "CLAUDE.md"), p.claudeMD); err != nil {
			warn.add("CLAUDE.md: %v; not carried", err)
		} else {
			env.Memory = append(env.Memory, "CLAUDE.md")
		}
	}
	if len(env.MCPServers)+len(env.Skills)+len(env.Plugins)+len(env.Memory) == 0 {
		return nil
	}
	rec, _ := json.MarshalIndent(env, "", "  ")
	if err := put(dst.join(pack, "env.json"), rec); err != nil {
		warn.add("record of the session's environment: %v", err)
	}
	env.args = envArgs(ad, dst, pack, env)
	notef("carried for this session only (%s on %s): %s", pack, dst.name, env.summary())
	if len(env.Credentials) > 0 {
		if shared {
			warn.add("MCP servers with credentials in their configuration went to %s as they are: %s - other users' jobs there can read them", dst.name, strings.Join(env.Credentials, ", "))
		} else {
			notef("MCP servers with credentials in their configuration went as they are (readable by your account on %s only): %s", dst.name, strings.Join(env.Credentials, ", "))
		}
	}
	return env
}

// envArgs are the resume command's options that load the session's environment.
func envArgs(ad agentAdapter, dst *hoEndpoint, pack string, env *handoffEnv) []string {
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
		return a
	}
	if len(env.MCPServers) > 0 {
		a = append(a, "--additional-mcp-config", "@"+mcp)
	}
	for _, pl := range env.Plugins {
		a = append(a, "--plugin-dir", dst.join(pack, "plugins", pl))
	}
	if len(env.Skills) > 0 { // <pack>/.github/skills
		a = append(a, "--add-dir", pack)
	}
	return a
}

func (e *handoffEnv) summary() string {
	var parts []string
	for _, x := range []struct {
		what  string
		names []string
	}{{"MCP servers", e.MCPServers}, {"skills", e.Skills}, {"plugins", e.Plugins}, {"memory", e.Memory}} {
		if len(x.names) > 0 {
			parts = append(parts, x.what+" "+strings.Join(x.names, ", "))
		}
	}
	return strings.Join(parts, "; ")
}

// isAbsAny: an absolute path of any OS.
func isAbsAny(p string) bool {
	return strings.HasPrefix(p, "/") || driveRE.MatchString(p) || strings.HasPrefix(p, `\\`)
}
