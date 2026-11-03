package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEnvUseScan(t *testing.T) {
	u := newEnvUse()
	u.scan("claude", []byte(strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":"<command-name>/review</command-name>"}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__github__create_issue","input":{"title":"x"}},` +
			`{"type":"tool_use","name":"mcp__plugin_tools_deploy-api__run","input":{}},{"type":"tool_use","name":"Skill","input":{"skill":"pdf"}},` +
			`{"type":"tool_use","name":"Skill","input":{"skill":"tools:deploy"}},{"type":"tool_use","name":"Bash","input":"not an object"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"mcp__fake__tool is only text"}]}}`,
		`{"partial line`,
	}, "\n")))
	if got := sortedKeys(u.mcp); !slices.Equal(got, []string{"github", "plugin_tools_deploy-api"}) {
		t.Errorf("claude MCP servers: %v", got)
	}
	if got := sortedKeys(u.skills); !slices.Equal(got, []string{"pdf", "review", "tools:deploy"}) {
		t.Errorf("claude skills: %v", got)
	}

	u = newEnvUse()
	u.scan("copilot", []byte(strings.Join([]string{
		`{"type":"tool.execution_start","data":{"toolName":"probe-get_color","mcpServerName":"probe","mcpConfigServerName":"probe","mcpConfigSource":"user"}}`,
		`{"type":"tool.execution_start","data":{"toolName":"icm-search_incidents","mcpServerName":"icm"}}`,
		`{"type":"tool.execution_start","data":{"toolName":"skill","arguments":{"skill":"icm-triage"}}}`,
		`{"type":"tool.execution_complete","data":{"toolName":"skill","arguments":{"skill":"not-a-start"}}}`,
		`{"type":"tool.execution_start","data":{"toolName":"bash","arguments":{"command":"ls"}}}`,
	}, "\n")))
	if got := sortedKeys(u.mcp); !slices.Equal(got, []string{"icm", "probe"}) {
		t.Errorf("copilot MCP servers: %v", got)
	}
	if got := sortedKeys(u.skills); !slices.Equal(got, []string{"icm-triage"}) {
		t.Errorf("copilot skills: %v", got)
	}
}

// envEPs are two local endpoints with homes of their own.
func envEPs(t *testing.T, root string) (src, dst *hoEndpoint) {
	src, dst = localEP(t), localEP(t)
	src.home, dst.home = filepath.Join(root, "src-home"), filepath.Join(root, "dst-home")
	return src, dst
}

func mustExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("missing: %v", err)
	}
}

func mustNotExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err == nil {
		t.Errorf("should not exist: %s", p)
	}
}

// TestCarryEnvClaude: of the source's MCP servers, skills and plugins the
// ones the session used and the target lacks go to the session's own
// directory there, with the project memory and CLAUDE.md; the target's own
// configuration is not touched; a second hop takes them from that directory.
func TestCarryEnvClaude(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src, dst := envEPs(t, root)
	srcCfg, dstCfg := filepath.Join(src.home, ".claude"), filepath.Join(dst.home, ".claude")
	proj, dstTop := filepath.Join(src.home, "src", "app"), filepath.Join(dst.home, "longarm-handoff", "app-1")
	id := "5e55a7c1-0000-4000-8000-00000000c1a0"
	srcProject := filepath.Join(srcCfg, "projects", "-src-app")
	dstProject := filepath.Join(dstCfg, "projects", "-dst-app")
	sess := &agentSession{ID: id, cfg: srcCfg, ProjectDir: proj, transcript: filepath.Join(srcProject, id+".jsonl")}

	writeFileT(t, filepath.Join(src.home, ".claude.json"), fmt.Sprintf(`{"mcpServers":{
		"github":{"command":%q,"env":{"TOKEN":"secret-value"}},
		"unused":{"command":"x"},
		"have-it":{"command":"y"}},
		"projects":{%q:{"mcpServers":{"local db":{"command":"node","args":[%q]}}}}}`,
		filepath.Join(src.home, "bin", "gh-mcp"), proj, filepath.Join(proj, "server.js")))
	writeFileT(t, filepath.Join(proj, ".mcp.json"), `{"mcpServers":{"jira":{"command":"jira-mcp"}}}`)
	writeFileT(t, filepath.Join(dst.home, ".claude.json"), `{"mcpServers":{"have-it":{"command":"z"}}}`)
	writeFileT(t, filepath.Join(srcCfg, "skills", "pdf", "SKILL.md"), "---\nname: pdf\n---\n")
	writeFileT(t, filepath.Join(srcCfg, "skills", "pdf", "scripts", "fill.py"), "print(1)\n")
	writeFileT(t, filepath.Join(srcCfg, "skills", "mine", "SKILL.md"), "source's\n")
	writeFileT(t, filepath.Join(dstCfg, "skills", "mine", "SKILL.md"), "target's own\n")
	writeFileT(t, filepath.Join(proj, ".claude", "skills", "projskill", "SKILL.md"), "travels with the code\n")
	toolsDir := filepath.Join(srcCfg, "plugins", "cache", "mkt", "tools", "1.0.0")
	writeFileT(t, filepath.Join(srcCfg, "plugins", "installed_plugins.json"), fmt.Sprintf(`{"version":2,"plugins":{
		"tools@mkt":[{"scope":"user","installPath":%q,"version":"1.0.0"}],
		"other@mkt":[{"scope":"user","installPath":%q}],
		"elsewhere@mkt":[{"scope":"user","installPath":%q}]}}`, toolsDir, filepath.Join(srcCfg, "plugins", "cache", "mkt", "other", "1"), filepath.Join(root, "outside")))
	writeFileT(t, filepath.Join(toolsDir, ".claude-plugin", "plugin.json"), `{"name":"tools"}`)
	writeFileT(t, filepath.Join(toolsDir, "skills", "deploy", "SKILL.md"), "deploy\n")
	writeFileT(t, filepath.Join(toolsDir, ".git", "HEAD"), "ref\n")
	writeFileT(t, filepath.Join(srcProject, "memory", "MEMORY.md"), "- [x](x.md)\n")
	writeFileT(t, filepath.Join(srcCfg, "CLAUDE.md"), "be terse\n")
	writeFileT(t, filepath.Join(dstProject, "memory", "MEMORY.md"), "the target's earlier memory\n")

	use := newEnvUse()
	for _, s := range []string{"github", "local_db", "jira", "have-it", "plugin_tools_deploy-api", "claude_ai_Gmail"} {
		use.mcp[s] = true
	}
	for _, s := range []string{"pdf", "mine", "projskill", "tools:deploy", "review", "../evil", "elsewhere:x"} {
		use.skills[s] = true
	}
	pms := []pathMap{{from: proj, to: dstTop, fromWin: src.windows(), toWin: dst.windows()}, {from: srcCfg, to: dstCfg, fromWin: src.windows(), toWin: dst.windows()}}
	var w warnings
	env := carryEnv(ctx, claudeAdapter{}, src, dst, envTarget{cfg: dstCfg, project: dstProject, cwd: dstTop, limit: 1 << 30}, sess, pms, use, &w)
	if env == nil {
		t.Fatalf("nothing carried; warnings %v", w)
	}
	pack := filepath.Join(dstCfg, "longarm-handoff", id)
	if env.Dir != pack || !slices.Equal(env.MCPServers, []string{"github", "local db"}) || !slices.Equal(env.Credentials, []string{"github"}) ||
		!slices.Equal(env.Skills, []string{"pdf"}) || !slices.Equal(env.Plugins, []string{"tools"}) ||
		!slices.Equal(env.Memory, []string{"project memory", "CLAUDE.md"}) {
		t.Fatalf("carried: %+v", env)
	}
	want := []string{"--mcp-config", filepath.Join(pack, "mcp.json"), "--plugin-dir", filepath.Join(pack, "plugins", "tools"),
		"--add-dir", pack, "--append-system-prompt-file", filepath.Join(pack, "CLAUDE.md")}
	if !slices.Equal(env.args, want) {
		t.Errorf("resume options:\n got %q\nwant %q", env.args, want)
	}
	mj := readFileT(t, filepath.Join(pack, "mcp.json"))
	if !strings.Contains(mj, "secret-value") || !strings.Contains(mj, jsonQ(filepath.Join(dstTop, "server.js"))) ||
		!strings.Contains(mj, jsonQ(filepath.Join(dst.home, "bin", "gh-mcp"))) || strings.Contains(mj, "unused") || strings.Contains(mj, "jira") {
		t.Errorf("mcp.json:\n%s", mj)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Join(pack, "mcp.json")); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("mcp.json mode: %v %v", fi, err)
		}
	}
	all := strings.Join(w, "\n")
	if !strings.Contains(all, "MCP server github runs") || !strings.Contains(all, "plugin elsewhere") || strings.Contains(all, "secret-value") {
		t.Errorf("warnings:\n%s", all)
	}
	mustExist(t, filepath.Join(pack, ".claude", "skills", "pdf", "scripts", "fill.py"))
	mustExist(t, filepath.Join(pack, "plugins", "tools", "skills", "deploy", "SKILL.md"))
	mustExist(t, filepath.Join(pack, "env.json"))
	mustNotExist(t, filepath.Join(pack, "plugins", "tools", ".git"))
	mustNotExist(t, filepath.Join(pack, ".claude", "skills", "mine"))
	mustNotExist(t, filepath.Join(pack, ".claude", "skills", "projskill"))
	mustNotExist(t, filepath.Join(dstCfg, "skills", "pdf")) // the target's own skills are not touched
	if got := readFileT(t, filepath.Join(dst.home, ".claude.json")); got != `{"mcpServers":{"have-it":{"command":"z"}}}` {
		t.Errorf("the target's configuration changed: %s", got)
	}
	if got := readFileT(t, filepath.Join(dstProject, "memory", "MEMORY.md")); got != "- [x](x.md)\n" {
		t.Errorf("project memory: %q", got)
	}
	bks, _ := filepath.Glob(filepath.Join(dstCfg, "longarm-handoff-backup", id+"-*", "memory", "MEMORY.md"))
	if len(bks) != 1 {
		t.Errorf("the target's earlier memory was not kept: %v", bks)
	}

	// the next hop: from the target, whose own configuration has none of it
	third := localEP(t)
	third.home = filepath.Join(root, "third-home")
	cfg3 := filepath.Join(third.home, ".claude")
	writeFileT(t, filepath.Join(dstProject, id+".jsonl"), "{}\n")
	sess2 := &agentSession{ID: id, cfg: dstCfg, ProjectDir: dstTop, transcript: filepath.Join(dstProject, id+".jsonl")}
	w = nil
	env2 := carryEnv(ctx, claudeAdapter{}, dst, third, envTarget{cfg: cfg3, project: filepath.Join(cfg3, "projects", "-app"), limit: 1 << 30}, sess2,
		[]pathMap{{from: dstCfg, to: cfg3, fromWin: dst.windows(), toWin: third.windows()}}, use, &w)
	// (have-it and mine are the second machine's own: the session used them there too)
	if env2 == nil || !slices.Equal(env2.MCPServers, []string{"github", "have-it", "local db"}) || !slices.Equal(env2.Skills, []string{"mine", "pdf"}) ||
		!slices.Equal(env2.Plugins, []string{"tools"}) || !slices.Contains(env2.Memory, "CLAUDE.md") {
		t.Fatalf("second hop: %+v %v", env2, w)
	}

	// again to the same target: the earlier directory is kept aside
	w = nil
	if env := carryEnv(ctx, claudeAdapter{}, src, dst, envTarget{cfg: dstCfg, project: dstProject, cwd: dstTop, shared: true, limit: 1 << 30}, sess, pms, use, &w); env == nil {
		t.Fatal("nothing carried the second time")
	}
	bks, _ = filepath.Glob(filepath.Join(dstCfg, "longarm-handoff-backup", id+"-*", "env", "mcp.json"))
	if len(bks) != 1 {
		t.Errorf("the earlier environment was not kept: %v", bks)
	}
	// shared isolation: the credentials are a warning
	if all := strings.Join(w, "\n"); !strings.Contains(all, "other users' jobs there can read them") {
		t.Errorf("shared target warnings:\n%s", all)
	}

	// nothing used: nothing carried, nothing written
	sess3 := *sess
	sess3.ID = "other"
	os.RemoveAll(filepath.Join(srcProject, "memory"))
	os.Remove(filepath.Join(srcCfg, "CLAUDE.md"))
	if env := carryEnv(ctx, claudeAdapter{}, src, dst, envTarget{cfg: dstCfg, project: dstProject, cwd: dstTop, limit: 1 << 30}, &sess3, pms, newEnvUse(), &w); env != nil {
		t.Errorf("carried for an unused environment: %+v", env)
	}
	mustNotExist(t, filepath.Join(dstCfg, "longarm-handoff", "other"))
}

// TestCarryEnvCopilot: MCP servers of mcp-config.json, personal skills, and
// the installed plugins the session's other skills and MCP servers came from.
func TestCarryEnvCopilot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src, dst := envEPs(t, root)
	srcCfg, dstCfg := filepath.Join(src.home, ".copilot"), filepath.Join(dst.home, ".copilot")
	proj := filepath.Join(src.home, "app")
	id := "c0p11075-0000-4000-8000-000000000001"
	sess := &agentSession{ID: id, cfg: srcCfg, ProjectDir: proj}

	writeFileT(t, filepath.Join(srcCfg, "mcp-config.json"), `{"mcpServers":{"probe":{"command":"node","headers":{"Authorization":"Bearer t"}},"unused":{}}}`)
	writeFileT(t, filepath.Join(dstCfg, "mcp-config.json"), `{"mcpServers":{"mine":{}}}`)
	ip := filepath.Join(srcCfg, "installed-plugins")
	writeFileT(t, filepath.Join(ip, "mkt", "icm", "plugin.json"), `{"name":"icm"}`)
	writeFileT(t, filepath.Join(ip, "mkt", "icm", ".mcp.json"), `{"mcpServers":{"icm":{"command":"icm-mcp"}}}`)
	writeFileT(t, filepath.Join(ip, "mkt", "twin", "plugin.json"), `{"name":"twin"}`)
	writeFileT(t, filepath.Join(ip, "mkt", "twin", "skills", "twin-devloop", "SKILL.md"), "twin\n")
	writeFileT(t, filepath.Join(ip, "mkt", "have", "skills", "have-skill", "SKILL.md"), "x\n")
	writeFileT(t, filepath.Join(dstCfg, "installed-plugins", "mkt", "have", "plugin.json"), `{}`)
	writeFileT(t, filepath.Join(src.home, ".agents", "skills", "notes", "SKILL.md"), "notes\n")
	writeFileT(t, filepath.Join(proj, ".github", "skills", "proj", "SKILL.md"), "travels\n")

	use := newEnvUse()
	for _, s := range []string{"probe", "icm", "mine", "github-mcp-server"} {
		use.mcp[s] = true
	}
	for _, s := range []string{"twin-devloop", "notes", "proj", "have-skill"} {
		use.skills[s] = true
	}
	var w warnings
	env := carryEnv(ctx, copilotAdapter{}, src, dst, envTarget{cfg: dstCfg, limit: 1 << 30}, sess, nil, use, &w)
	if env == nil {
		t.Fatalf("nothing carried; warnings %v", w)
	}
	pack := filepath.Join(dstCfg, "longarm-handoff", id)
	if !slices.Equal(env.MCPServers, []string{"probe"}) || !slices.Equal(env.Credentials, []string{"probe"}) || !slices.Equal(env.Skills, []string{"notes"}) ||
		!slices.Equal(env.Plugins, []string{"icm", "twin"}) || len(env.Memory) != 0 {
		t.Fatalf("carried: %+v (warnings %v)", env, w)
	}
	want := []string{"--additional-mcp-config", "@" + filepath.Join(pack, "mcp.json"), "--plugin-dir", filepath.Join(pack, "plugins", "icm"),
		"--plugin-dir", filepath.Join(pack, "plugins", "twin"), "--add-dir", pack}
	if !slices.Equal(env.args, want) {
		t.Errorf("resume options:\n got %q\nwant %q", env.args, want)
	}
	mustExist(t, filepath.Join(pack, ".github", "skills", "notes", "SKILL.md"))
	mustExist(t, filepath.Join(pack, "plugins", "icm", ".mcp.json"))
	mustExist(t, filepath.Join(pack, "plugins", "twin", "skills", "twin-devloop", "SKILL.md"))
	mustNotExist(t, filepath.Join(pack, "plugins", "have"))
	if got := readFileT(t, filepath.Join(dstCfg, "mcp-config.json")); got != `{"mcpServers":{"mine":{}}}` {
		t.Errorf("the target's configuration changed: %s", got)
	}
}

// TestCarryEnvLimits: a part over the size limit or with an unsafe name
// is a warning, the rest goes.
func TestCarryEnvLimits(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src, dst := envEPs(t, root)
	srcCfg, dstCfg := filepath.Join(src.home, ".claude"), filepath.Join(dst.home, ".claude")
	sess := &agentSession{ID: "s1", cfg: srcCfg, ProjectDir: filepath.Join(src.home, "app")}
	writeFileT(t, filepath.Join(srcCfg, "skills", "big", "SKILL.md"), strings.Repeat("x", 4096))
	writeFileT(t, filepath.Join(srcCfg, "skills", "small", "SKILL.md"), "s\n")
	use := newEnvUse()
	use.skills["big"], use.skills["small"] = true, true
	var w warnings
	env := carryEnv(ctx, claudeAdapter{}, src, dst, envTarget{cfg: dstCfg, limit: 1024}, sess, nil, use, &w)
	if env == nil || !slices.Equal(env.Skills, []string{"small"}) || !strings.Contains(strings.Join(w, "\n"), "skill big") {
		t.Errorf("carried %+v, warnings %v", env, w)
	}
}

// TestCarryEnvClaudeSettings: hooks and settings only with --carry-settings
// (named otherwise): what differs from the target's, loaded with --settings;
// the project's settings.local.json into the new worktree.
func TestCarryEnvClaudeSettings(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src, dst := envEPs(t, root)
	srcCfg, dstCfg := filepath.Join(src.home, ".claude"), filepath.Join(dst.home, ".claude")
	proj, dstTop := filepath.Join(src.home, "app"), filepath.Join(dst.home, "wt")
	os.MkdirAll(dstTop, 0o755)
	sess := &agentSession{ID: "s1", cfg: srcCfg, ProjectDir: proj}
	shared := `{"matcher":"Bash","hooks":[{"type":"command","command":"guard"}]}`
	writeFileT(t, filepath.Join(srcCfg, "settings.json"), fmt.Sprintf(`{"theme":"dark","enabledPlugins":{"x@m":true},
		"permissions":{"allow":["Bash(make)"]},"env":{"X":"1"},
		"hooks":{"PreToolUse":[%s],"Stop":[{"hooks":[{"type":"command","command":%s}]}]}}`, shared, jsonQ(filepath.Join(src.home, "bin", "notify.sh"))))
	writeFileT(t, filepath.Join(dstCfg, "settings.json"), fmt.Sprintf(`{"theme":"dark","hooks":{"PreToolUse":[%s]}}`, shared))
	writeFileT(t, filepath.Join(proj, ".claude", "settings.local.json"), `{"permissions":{"allow":["Bash(go test:*)"]}}`)
	tgt := envTarget{cfg: dstCfg, cwd: dstTop, limit: 1 << 30}
	pms := []pathMap{{from: proj, to: dstTop, fromWin: src.windows(), toWin: dst.windows()}}

	var w warnings
	env := carryEnv(ctx, claudeAdapter{}, src, dst, tgt, sess, pms, newEnvUse(), &w)
	if env == nil || len(env.args) != 0 || !slices.Equal(env.NotCarried, []string{"settings.json (hooks: Stop; env, permissions)", "settings.local.json (permissions)"}) {
		t.Fatalf("without --carry-settings: %+v", env)
	}
	mustNotExist(t, filepath.Join(dstCfg, "longarm-handoff", "s1"))
	mustNotExist(t, filepath.Join(dstTop, ".claude", "settings.local.json"))

	tgt.settings = true
	env = carryEnv(ctx, claudeAdapter{}, src, dst, tgt, sess, pms, newEnvUse(), &w)
	pack := filepath.Join(dstCfg, "longarm-handoff", "s1")
	if env == nil || !slices.Equal(env.Settings, []string{"settings.json", "settings.local.json"}) ||
		!slices.Equal(env.args, []string{"--settings", filepath.Join(pack, "settings.json")}) {
		t.Fatalf("with --carry-settings: %+v", env)
	}
	var got map[string]json.RawMessage
	json.Unmarshal([]byte(readFileT(t, filepath.Join(pack, "settings.json"))), &got)
	if keys := sortedKeys(got); !slices.Equal(keys, []string{"env", "hooks", "permissions"}) {
		t.Errorf("settings.json keys %v (the target's theme and hooks, and the plugins, are not repeated)", keys)
	}
	if h := hookEntries(got["hooks"]); len(h) != 1 || len(h["Stop"]) != 1 || !strings.Contains(string(h["Stop"][0]), jsonQ(filepath.Join(dst.home, "bin", "notify.sh"))[1:]) {
		t.Errorf("hooks: %s", got["hooks"])
	}
	if !strings.Contains(strings.Join(w, "\n"), "notify.sh, which") {
		t.Errorf("no warning for the hook's missing script: %v", w)
	}
	if got := readFileT(t, filepath.Join(dstTop, ".claude", "settings.local.json")); !strings.Contains(got, "go test") {
		t.Errorf("settings.local.json: %s", got)
	}
}

// TestCarryEnvCopilotExtras: the user's instructions (by default) through
// COPILOT_CUSTOM_INSTRUCTIONS_DIRS, the hooks (with --carry-settings) as a
// plugin, plugins the target has from another marketplace, executable bits.
func TestCarryEnvCopilotExtras(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src, dst := envEPs(t, root)
	srcCfg, dstCfg := filepath.Join(src.home, ".copilot"), filepath.Join(dst.home, ".copilot")
	sess := &agentSession{ID: "c1", cfg: srcCfg, ProjectDir: filepath.Join(src.home, "app")}
	writeFileT(t, filepath.Join(srcCfg, "copilot-instructions.md"), "Be brief.\n")
	writeFileT(t, filepath.Join(srcCfg, "instructions", "go.instructions.md"), "---\napplyTo: \"**/*.go\"\n---\ngofmt\n")
	writeFileT(t, filepath.Join(dstCfg, "instructions", "go.instructions.md"), "---\napplyTo: \"**/*.go\"\n---\ngofmt\n")
	writeFileT(t, filepath.Join(srcCfg, "hooks", "a.json"), `{"version":1,"hooks":{"sessionStart":[{"type":"command","bash":"echo hi"}]}}`)
	ip := filepath.Join(srcCfg, "installed-plugins")
	writeFileT(t, filepath.Join(ip, "mkt", "icm", "skills", "triage", "SKILL.md"), "x\n")
	writeFileT(t, filepath.Join(ip, "mkt", "tools", "skills", "deploy", "SKILL.md"), "x\n")
	writeFileT(t, filepath.Join(ip, "mkt", "tools", "bin", "run.sh"), "#!/bin/sh\necho\n")
	os.Chmod(filepath.Join(ip, "mkt", "tools", "bin", "run.sh"), 0o755)
	writeFileT(t, filepath.Join(dstCfg, "config.json"), "// User settings\n{\"installedPlugins\":[{\"name\":\"icm\",\"marketplace\":\"renamed\"}]}\n")
	use := newEnvUse()
	use.skills["triage"], use.skills["deploy"] = true, true

	var w warnings
	tgt := envTarget{cfg: dstCfg, limit: 1 << 30}
	env := carryEnv(ctx, copilotAdapter{}, src, dst, tgt, sess, nil, use, &w)
	pack := filepath.Join(dstCfg, "longarm-handoff", "c1")
	instr := filepath.Join(pack, "instructions")
	if env == nil || !slices.Equal(env.Plugins, []string{"tools"}) || !slices.Equal(env.Memory, []string{"copilot-instructions.md"}) ||
		len(env.Settings) != 0 || !slices.Equal(env.NotCarried, []string{"hooks (hooks: sessionStart)"}) ||
		env.env["COPILOT_CUSTOM_INSTRUCTIONS_DIRS"] != instr {
		t.Fatalf("carried: %+v env %v (warnings %v)", env, env.env, w)
	}
	if got := readFileT(t, filepath.Join(instr, ".github", "instructions", "copilot-instructions.instructions.md")); got != copilotApplyAll+"Be brief.\n" {
		t.Errorf("instructions: %q", got)
	}
	mustNotExist(t, filepath.Join(instr, ".github", "instructions", "go.instructions.md"))
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Join(pack, "plugins", "tools", "bin", "run.sh")); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("run.sh mode: %v %v", fi, err)
		}
		if fi, err := os.Stat(filepath.Join(pack, "plugins", "tools", "skills", "deploy", "SKILL.md")); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("SKILL.md mode: %v %v", fi, err)
		}
	}

	tgt.settings = true
	env = carryEnv(ctx, copilotAdapter{}, src, dst, tgt, sess, nil, use, &w)
	hp := filepath.Join(pack, "plugins", copilotHooksPlugin)
	if env == nil || !slices.Equal(env.Settings, []string{"hooks"}) || !slices.Contains(env.args, hp) {
		t.Fatalf("with --carry-settings: %+v", env)
	}
	if got := readFileT(t, filepath.Join(hp, "hooks.json")); !strings.Contains(got, "sessionStart") || !strings.Contains(readFileT(t, filepath.Join(hp, "plugin.json")), `"hooks": "hooks.json"`) {
		t.Errorf("hooks plugin: %s", got)
	}
}

// TestEnvFileMode: executable on the source, or a script from Windows.
func TestEnvFileMode(t *testing.T) {
	dir := t.TempDir()
	script, text := filepath.Join(dir, "s"), filepath.Join(dir, "t")
	os.WriteFile(script, []byte("#!/bin/sh\n"), 0o600)
	os.WriteFile(text, []byte("hello"), 0o600)
	unix, win := &hoEndpoint{goos: "linux"}, &hoEndpoint{goos: "windows"}
	for _, c := range []struct {
		ep    *hoEndpoint
		mode  fs.FileMode
		local string
		want  fs.FileMode
	}{
		{unix, 0o755, text, 0o700},
		{unix, 0o644, script, 0o600}, // not executable there: kept so
		{win, 0, script, 0o700},
		{win, 0, text, 0o600},
	} {
		if got := envFileMode(c.ep, hoFile{mode: c.mode}, c.local); got != c.want {
			t.Errorf("%s %o %s: %o, want %o", c.ep.goos, c.mode, filepath.Base(c.local), got, c.want)
		}
	}
}

// TestPruneEnvDirs: handoff --cleanup removes the directories of sessions
// handed off from here or gone, and keeps those of sessions here.
func TestPruneEnvDirs(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	cfg := filepath.Join(home, "claude-cfg")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("COPILOT_HOME", filepath.Join(home, "copilot-home"))
	here, moved, gone := "11111111-2222-3333-4444-555555555555", "aaaaaaaa-2222-3333-4444-555555555555", "bbbbbbbb-2222-3333-4444-555555555555"
	claudeFixture(t, cfg, filepath.Join(home, "app"), here)
	claudeFixture(t, cfg, filepath.Join(home, "other"), moved)
	ep := localEP(t)
	list, err := claudeAdapter{}.list(ctx, ep, cfg, moved, false, nil)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	mark, _ := json.Marshal(handoffMark{To: "w1", At: time.Now().Add(time.Minute)})
	writeFileT(t, list[0].marker, string(mark))
	for _, id := range []string{here, moved, gone} {
		writeFileT(t, filepath.Join(cfg, "longarm-handoff", id, "mcp.json"), "{}")
	}
	got := map[string]envDirState{}
	for _, s := range pruneEnvDirs(ctx, ep) {
		got[s.Session] = s
	}
	if s := got[here]; s.Removed || s.Reason != "the session is here" {
		t.Errorf("here: %+v", s)
	}
	if s := got[moved]; !s.Removed || s.Reason != "handed off to w1" {
		t.Errorf("moved: %+v", s)
	}
	if s := got[gone]; !s.Removed || s.Reason != "no such session here" {
		t.Errorf("gone: %+v", s)
	}
	mustExist(t, filepath.Join(cfg, "longarm-handoff", here, "mcp.json"))
	mustNotExist(t, filepath.Join(cfg, "longarm-handoff", moved))
	mustNotExist(t, filepath.Join(cfg, "longarm-handoff", gone))
}

func jsonQ(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func readFileT(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
