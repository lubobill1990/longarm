package main

// --agent-env, --mcp, --skill, --plugin of run, exec, session start and
// attach (docs/usage.md "Agent environment (--agent-env)"): when longarm
// starts Claude Code or Copilot CLI on a worker, it can take this machine's
// MCP servers, skills, plugins, memory and settings along - only what is
// asked for, only what the worker's agent lacks - for that run only. They
// go to <agent dir>/longarm-env/<hash of the content>/ on the worker
// (shared by the runs that carry the same, written once) and the agent
// loads them with the per-run options handoff uses (handoff_env.go),
// added right after the command's program.

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"longarm/internal/client"
	"longarm/internal/steer"
	pb "longarm/proto/longarm/v1"
)

// agentEnvOpts are the flags.
type agentEnvOpts struct {
	kinds                []string // --agent-env
	mcp, skills, plugins []string // --mcp, --skill, --plugin
}

// agentEnvKinds are what --agent-env names; all is the first four (hooks
// run commands: settings only by name).
var agentEnvKinds = []string{"mcp", "skills", "plugins", "memory", "settings"}

func (o *agentEnvOpts) register(c *cobra.Command) {
	f := c.Flags()
	f.StringSliceVar(&o.kinds, "agent-env", nil, "take this machine's agent environment along for the agent the command runs (claude / copilot), for this run only: "+
		"all (= mcp,skills,plugins,memory), or a list of mcp, skills, plugins, memory, settings (hooks run commands: only by name); none")
	f.StringArrayVar(&o.mcp, "mcp", nil, "take this MCP server of this machine's agent configuration along (repeatable)")
	f.StringArrayVar(&o.skills, "skill", nil, "take this skill along: a personal one, or Claude Code's plugin:skill (repeatable)")
	f.StringArrayVar(&o.plugins, "plugin", nil, "take this installed plugin along (repeatable)")
}

func (o agentEnvOpts) asked() bool {
	return len(o.kinds)+len(o.mcp)+len(o.skills)+len(o.plugins) > 0
}

// agentInventory is what of an agent's environment a machine has.
type agentInventory struct {
	mcp       []string // MCP servers, as configured
	skills    []string // personal skills
	plugins   []string // the enabled plugins
	installed []string // all installed plugins
}

// inventory reads ep's agent environment (projectDir: for Claude Code's
// project-scoped MCP servers and plugins).
func inventory(ctx context.Context, ep *hoEndpoint, ad agentAdapter, cfg, projectDir string) agentInventory {
	var inv agentInventory
	skillsIn := func(dir string) {
		ls, _ := ep.list(ctx, dir)
		for _, f := range ls {
			if name, ok := strings.CutSuffix(f.rel, "/SKILL.md"); ok && !f.isDir && !strings.Contains(name, "/") && !slices.Contains(inv.skills, name) {
				inv.skills = append(inv.skills, name)
			}
		}
	}
	if ad.name() == "claude" {
		inv.mcp = ad.mcpServers(ctx, ep, cfg, projectDir)
		skillsIn(ep.join(cfg, "skills"))
		installed := claudePlugins(ctx, ep, cfg, projectDir)
		inv.installed = sortedKeys(installed)
		var enabled map[string]bool // nil: no enabledPlugins, all are
		if raw := jsonObject(ctx, ep, ep.join(cfg, "settings.json"))["enabledPlugins"]; raw != nil {
			enabled = map[string]bool{}
			var m map[string]bool
			if json.Unmarshal(raw, &m) == nil {
				for k, on := range m {
					name, _, _ := strings.Cut(k, "@")
					enabled[name] = enabled[name] || on
				}
			}
		}
		for _, n := range inv.installed {
			if enabled == nil || enabled[n] {
				inv.plugins = append(inv.plugins, n)
			}
		}
	} else {
		inv.mcp = ad.mcpServers(ctx, ep, cfg, "")
		skillsIn(ep.join(cfg, "skills"))
		skillsIn(ep.join(ep.home, ".agents", "skills"))
		var ps []struct {
			Name    string `json:"name"`
			Enabled *bool  `json:"enabled"`
		}
		json.Unmarshal(jsonObject(ctx, ep, ep.join(cfg, "config.json"))["installedPlugins"], &ps)
		for _, p := range ps {
			if p.Name == "" || slices.Contains(inv.installed, p.Name) {
				continue
			}
			inv.installed = append(inv.installed, p.Name)
			if p.Enabled == nil || *p.Enabled {
				inv.plugins = append(inv.plugins, p.Name)
			}
		}
	}
	slices.Sort(inv.skills)
	slices.Sort(inv.plugins)
	slices.Sort(inv.installed)
	return inv
}

// forwardAgentEnv carries what the flags ask for of this machine's agent
// environment to worker and returns the command with the options that load
// it (after argv[0]) and the environment with what it needs. steerable: a
// steerable run (it passes Claude Code its own --settings).
func forwardAgentEnv(ctx context.Context, s *session, conn *client.Conn, worker string, argv []string, o agentEnvOpts,
	agentAuth *pb.AgentAuth, env map[string]string, steerable bool) ([]string, map[string]string, error) {
	if !o.asked() {
		return argv, env, nil
	}
	kinds := map[string]bool{}
	none := false
	for _, k := range o.kinds {
		switch k = strings.ToLower(strings.TrimSpace(k)); {
		case k == "all":
			for _, a := range agentEnvKinds[:4] {
				kinds[a] = true
			}
		case k == "none":
			none = true
		case slices.Contains(agentEnvKinds, k):
			kinds[k] = true
		default:
			return nil, nil, usagef("--agent-env %q: want all, none, or a list of %s", k, strings.Join(agentEnvKinds, ", "))
		}
	}
	if none {
		if len(kinds)+len(o.mcp)+len(o.skills)+len(o.plugins) > 0 {
			return nil, nil, usagef("--agent-env none goes alone (not with other kinds, --mcp, --skill or --plugin)")
		}
		return argv, env, nil
	}
	// the agent: the command's program, or - a wrapper (a shell, a script) -
	// the one agent --agent-auth forwards; the wrapper then gets the options
	// in LONGARM_AGENT_ARGS (a JSON array) to add to the agent's command
	agent, wrapped := steer.AgentOf(argv), false
	if agent == "" {
		var named []string
		for _, a := range agentAuth.GetAgents() {
			if a == steer.Claude || a == steer.Copilot {
				named = append(named, a)
			}
		}
		if len(named) != 1 {
			prog := ""
			if len(argv) > 0 {
				prog = argv[0]
			}
			return nil, nil, usagef("--agent-env / --mcp / --skill / --plugin: the command runs %q, not claude or copilot: run the agent itself, or name it with --agent-auth claude|copilot (a wrapper then gets the options in %s)",
				prog, envAgentArgs)
		}
		agent, wrapped = named[0], true
	}
	if steerable && agent == steer.Claude && kinds["settings"] {
		return nil, nil, usagef("--agent-env settings: a steerable run passes Claude Code its own --settings (for its hooks)")
	}
	ad, err := adapterFor(agent)
	if err != nil {
		return nil, nil, err
	}
	src, err := openEndpoint(ctx, nil, localMachine)
	if err != nil {
		return nil, nil, err
	}
	cfg := ad.configDir(ctx, src)
	wd, _ := os.Getwd()
	inv := inventory(ctx, src, ad, cfg, wd)

	use := newEnvUse()
	mcpKey := func(n string) string { // the name the planner looks for
		if agent == steer.Claude {
			return claudeToolName(n)
		}
		return n
	}
	if kinds["mcp"] {
		for _, n := range inv.mcp {
			use.mcp[mcpKey(n)] = true
		}
	}
	for _, n := range o.mcp {
		if !slices.Contains(inv.mcp, n) {
			return nil, nil, usagef("--mcp %s: %s has no MCP server of that name here (it has: %s)", n, agent, listOrNone(inv.mcp))
		}
		use.mcp[mcpKey(n)] = true
	}
	if kinds["skills"] {
		for _, n := range inv.skills {
			use.skills[n] = true
		}
	}
	for _, n := range o.skills {
		use.skills[strings.TrimPrefix(n, "/")] = true
	}
	if kinds["plugins"] {
		for _, n := range inv.plugins {
			use.plugins[n] = true
		}
	}
	for _, n := range o.plugins {
		if !slices.Contains(inv.installed, n) {
			return nil, nil, usagef("--plugin %s: %s has no plugin of that name installed here (it has: %s)", n, agent, listOrNone(inv.installed))
		}
		use.plugins[n] = true
	}

	ep, err := endpointOf(ctx, conn, worker)
	if err != nil {
		return nil, nil, err
	}
	// MCP configurations and settings may hold credentials: not where other
	// users' jobs run as the same account (as for --agent-auth: the hub's
	// owner; no hub record, a worker of workers.toml, counts as yours)
	var node *pb.NodeInfo
	if nodes, ok := s.hubNodes(ctx, false); ok {
		for _, n := range nodes {
			if n.Name == worker {
				node = n
			}
		}
	}
	shared := !filesAreMine(ep, node)
	if shared && (len(use.mcp) > 0 || kinds["settings"]) {
		return nil, nil, usagef("%s is somebody else's worker with shared isolation: other users' jobs there could read MCP configurations and settings (their credentials); take skills, plugins or memory only", worker)
	}
	wcfg := ad.configDir(ctx, ep)
	has := wcfg // what the agent there has already
	switch {
	case agent == steer.Claude && env["CLAUDE_CONFIG_DIR"] != "":
		has = env["CLAUDE_CONFIG_DIR"]
	case agent == steer.Copilot && env["COPILOT_HOME"] != "":
		has = env["COPILOT_HOME"]
	case agent == steer.Copilot && agentAuth.GetCopilotHome():
		has = "" // a private, new COPILOT_HOME: it has nothing
	}
	only := map[string]bool{"memory": kinds["memory"], "settings": kinds["settings"]}
	t := envTarget{cfg: wcfg, has: has, shared: shared, settings: kinds["settings"], limit: 64 << 20, only: only, keyed: true}
	var warn warnings
	res := carryEnv(ctx, ad, src, ep, t, &agentSession{cfg: cfg, ProjectDir: wd}, nil, use, &warn)
	for _, n := range o.skills { // a skill named but not carried: not here, or the worker has it
		if res == nil || !slices.Contains(res.Skills, n) {
			if pl, _, ok := strings.Cut(n, ":"); !ok || res == nil || !slices.Contains(res.Plugins, pl) {
				notef("--skill %s: not carried (%s has it, or this machine has no such skill)", n, worker)
			}
		}
	}
	if res == nil || len(res.args)+len(res.env) == 0 {
		notef("agent environment: nothing to carry (%s has what was asked for)", worker)
		return argv, env, nil
	}
	if env == nil {
		env = map[string]string{}
	}
	for k, v := range res.env {
		if _, mine := env[k]; !mine { // what you give yourself wins
			env[k] = v
		}
	}
	if wrapped {
		if len(res.args) > 0 {
			b, _ := json.Marshal(res.args)
			env[envAgentArgs] = string(b)
		}
		return argv, env, nil
	}
	out := append([]string{argv[0]}, res.args...)
	out = append(out, argv[1:]...)
	return out, env, nil
}

// envAgentArgs carries the agent's options to a wrapper that starts it.
const envAgentArgs = "LONGARM_AGENT_ARGS"

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}
