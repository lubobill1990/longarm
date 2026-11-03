package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
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
	env := carryEnv(ctx, claudeAdapter{}, src, dst, dstCfg, dstProject, sess, pms, use, false, 1<<30, &w)
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
	env2 := carryEnv(ctx, claudeAdapter{}, dst, third, cfg3, filepath.Join(cfg3, "projects", "-app"), sess2,
		[]pathMap{{from: dstCfg, to: cfg3, fromWin: dst.windows(), toWin: third.windows()}}, use, false, 1<<30, &w)
	// (have-it and mine are the second machine's own: the session used them there too)
	if env2 == nil || !slices.Equal(env2.MCPServers, []string{"github", "have-it", "local db"}) || !slices.Equal(env2.Skills, []string{"mine", "pdf"}) ||
		!slices.Equal(env2.Plugins, []string{"tools"}) || !slices.Contains(env2.Memory, "CLAUDE.md") {
		t.Fatalf("second hop: %+v %v", env2, w)
	}

	// again to the same target: the earlier directory is kept aside
	w = nil
	if env := carryEnv(ctx, claudeAdapter{}, src, dst, dstCfg, dstProject, sess, pms, use, true, 1<<30, &w); env == nil {
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
	if env := carryEnv(ctx, claudeAdapter{}, src, dst, dstCfg, dstProject, &sess3, pms, newEnvUse(), false, 1<<30, &w); env != nil {
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
	env := carryEnv(ctx, copilotAdapter{}, src, dst, dstCfg, "", sess, nil, use, false, 1<<30, &w)
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
	env := carryEnv(ctx, claudeAdapter{}, src, dst, dstCfg, "", sess, nil, use, false, 1024, &w)
	if env == nil || !slices.Equal(env.Skills, []string{"small"}) || !strings.Contains(strings.Join(w, "\n"), "skill big") {
		t.Errorf("carried %+v, warnings %v", env, w)
	}
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
