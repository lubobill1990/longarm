package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestAgentEnvFlags: refused before anything is sent.
func TestAgentEnvFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg := filepath.Join(home, "claude-cfg")
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	writeFileT(t, filepath.Join(cfg, ".claude.json"), `{"mcpServers":{"github":{}}}`)
	ctx := context.Background()
	for _, c := range []struct {
		o         agentEnvOpts
		argv      []string
		steerable bool
		want      string
	}{
		{agentEnvOpts{kinds: []string{"bogus"}}, []string{"claude"}, false, `--agent-env "bogus"`},
		{agentEnvOpts{kinds: []string{"none", "mcp"}}, []string{"claude"}, false, "goes alone"},
		{agentEnvOpts{kinds: []string{"skills"}}, []string{"sh", "-c", "claude -p x"}, false, "not claude or copilot"},
		{agentEnvOpts{kinds: []string{"settings"}}, []string{"claude"}, true, "steerable"},
		{agentEnvOpts{mcp: []string{"nope"}}, []string{"claude", "-p", "x"}, false, "it has: github"},
		{agentEnvOpts{plugins: []string{"nope"}}, []string{"claude"}, false, "no plugin of that name"},
	} {
		_, _, err := forwardAgentEnv(ctx, nil, nil, "w1", c.argv, c.o, nil, nil, c.steerable)
		if !isUsage(err) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v %v: %v (want %q)", c.o, c.argv, err, c.want)
		}
	}
	// nothing asked, or none: the command as it is
	argv := []string{"claude", "-p", "x"}
	for _, o := range []agentEnvOpts{{}, {kinds: []string{"none"}}} {
		if got, _, err := forwardAgentEnv(ctx, nil, nil, "w1", argv, o, nil, nil, false); err != nil || !slices.Equal(got, argv) {
			t.Errorf("%+v: %v %v", o, got, err)
		}
	}
}

func TestInventory(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ep, _ := envEPs(t, root)
	cfg := filepath.Join(ep.home, ".claude")
	writeFileT(t, filepath.Join(ep.home, ".claude.json"), `{"mcpServers":{"github":{}},"projects":{"/p":{"mcpServers":{"db":{}}}}}`)
	writeFileT(t, filepath.Join(cfg, "skills", "pdf", "SKILL.md"), "x")
	writeFileT(t, filepath.Join(cfg, "skills", "notes.md"), "not a skill")
	writeFileT(t, filepath.Join(cfg, "plugins", "installed_plugins.json"), `{"plugins":{"a@m":[{"scope":"user","installPath":"/x"}],"b@m":[{"scope":"user","installPath":"/y"}]}}`)
	writeFileT(t, filepath.Join(cfg, "settings.json"), `{"enabledPlugins":{"a@m":true,"b@m":false}}`)
	inv := inventory(ctx, ep, claudeAdapter{}, cfg, "")
	if !slices.Equal(inv.mcp, []string{"github"}) || !slices.Equal(inv.skills, []string{"pdf"}) ||
		!slices.Equal(inv.plugins, []string{"a"}) || !slices.Equal(inv.installed, []string{"a", "b"}) {
		t.Errorf("claude: %+v", inv)
	}
	cop := filepath.Join(ep.home, ".copilot")
	writeFileT(t, filepath.Join(cop, "mcp-config.json"), `{"mcpServers":{"probe":{}}}`)
	writeFileT(t, filepath.Join(ep.home, ".agents", "skills", "notes", "SKILL.md"), "x")
	writeFileT(t, filepath.Join(cop, "config.json"), "// c\n{\"installedPlugins\":[{\"name\":\"icm\"},{\"name\":\"off\",\"enabled\":false}]}")
	inv = inventory(ctx, ep, copilotAdapter{}, cop, "")
	if !slices.Equal(inv.mcp, []string{"probe"}) || !slices.Equal(inv.skills, []string{"notes"}) ||
		!slices.Equal(inv.plugins, []string{"icm"}) || !slices.Equal(inv.installed, []string{"icm", "off"}) {
		t.Errorf("copilot: %+v", inv)
	}
}

// TestCarryEnvKeyed: a run's environment goes to longarm-env/<key>, once;
// another content is another key; a fresh agent home (has "") gets all of it;
// --cleanup removes the unused ones and what was left half-written.
func TestCarryEnvKeyed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	src, dst := envEPs(t, root)
	srcCfg, dstCfg := filepath.Join(src.home, ".copilot"), filepath.Join(dst.home, ".copilot")
	writeFileT(t, filepath.Join(srcCfg, "mcp-config.json"), `{"mcpServers":{"probe":{"command":"node"}}}`)
	writeFileT(t, filepath.Join(dstCfg, "mcp-config.json"), `{"mcpServers":{"probe":{"command":"node"}}}`) // the worker's own
	writeFileT(t, filepath.Join(srcCfg, "skills", "notes", "SKILL.md"), "notes\n")
	sess := &agentSession{cfg: srcCfg, ProjectDir: filepath.Join(src.home, "app")}
	use := newEnvUse()
	use.mcp["probe"], use.skills["notes"] = true, true
	runs := filepath.Join(dstCfg, envRunDir)

	var w warnings
	// the worker's own Copilot home: probe is there already
	tgt := envTarget{cfg: dstCfg, has: dstCfg, limit: 1 << 20, only: map[string]bool{}, keyed: true}
	env := carryEnv(ctx, copilotAdapter{}, src, dst, tgt, sess, nil, use, &w)
	dirs, _ := filepath.Glob(filepath.Join(runs, "*"))
	if env == nil || len(dirs) != 1 || env.Dir != dirs[0] || len(env.MCPServers) != 0 || !slices.Equal(env.Skills, []string{"notes"}) ||
		!slices.Equal(env.args, []string{"--add-dir", dirs[0]}) {
		t.Fatalf("carried %+v, dirs %v", env, dirs)
	}
	// again: the same directory, not written again
	os.Remove(filepath.Join(dirs[0], ".github", "skills", "notes", "SKILL.md"))
	if again := carryEnv(ctx, copilotAdapter{}, src, dst, tgt, sess, nil, use, &w); again == nil || again.Dir != dirs[0] || !slices.Equal(again.args, env.args) {
		t.Fatalf("again: %+v", again)
	}
	mustNotExist(t, filepath.Join(dirs[0], ".github", "skills", "notes", "SKILL.md"))
	// a private, new COPILOT_HOME has nothing: probe goes too, to another key
	tgt.has = ""
	fresh := carryEnv(ctx, copilotAdapter{}, src, dst, tgt, sess, nil, use, &w)
	if fresh == nil || fresh.Dir == dirs[0] || !slices.Equal(fresh.MCPServers, []string{"probe"}) {
		t.Fatalf("fresh: %+v", fresh)
	}
	if left, _ := filepath.Glob(filepath.Join(runs, ".staging-*")); len(left) != 0 {
		t.Errorf("staging left: %v", left)
	}

	// cleanup: the first unused for 8 days goes, the fresh one stays; a half-written one of 2 days ago goes
	var rec handoffEnv
	json.Unmarshal([]byte(readFileT(t, filepath.Join(dirs[0], "env.json"))), &rec)
	rec.Used = time.Now().Add(-8 * 24 * time.Hour)
	b, _ := json.Marshal(rec)
	os.WriteFile(filepath.Join(dirs[0], "env.json"), b, 0o600)
	stale := filepath.Join(runs, ".staging-1-2")
	os.MkdirAll(stale, 0o700)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(stale, old, old)
	got := map[string]bool{}
	for _, s := range pruneRunEnvs(ctx, dst, copilotAdapter{}, dstCfg) {
		got[s.Dir] = s.Removed
	}
	if !got[dirs[0]] || got[fresh.Dir] || !got[stale] {
		t.Errorf("pruned: %v", got)
	}
	mustExist(t, fresh.Dir)
}
