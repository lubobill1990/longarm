package agentskill

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Agent is a coding agent whose user-global skills directory can hold the
// Longarm skill. Paths are templates: "~" is the user's home and
// ${VAR:-default} an environment variable with a default.
type Agent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Bins: executables on PATH that mean the agent is installed.
	Bins []string `json:"-"`
	// Homes: directories whose existence means the agent was used here.
	Homes []string `json:"-"`
	// Skills: the user-global skills directory (the skill goes into
	// <Skills>/longarm/SKILL.md).
	Skills string `json:"-"`
	// Source documents Skills (docs or source code).
	Source string `json:"-"`
}

// Agents is the table of supported agents; add a row to support another.
// Each Skills path is verified against the agent's documentation or source
// (Source).
var Agents = []Agent{
	{
		ID: "claude", Name: "Claude Code",
		Bins:   []string{"claude"},
		Homes:  []string{"${CLAUDE_CONFIG_DIR:-~/.claude}"},
		Skills: "${CLAUDE_CONFIG_DIR:-~/.claude}/skills",
		// $CLAUDE_CONFIG_DIR replaces ~/.claude for all of its configuration
		// (https://code.claude.com/docs/en/env-vars)
		Source: "https://code.claude.com/docs/en/skills",
	},
	{
		ID: "copilot", Name: "GitHub Copilot CLI",
		Bins:   []string{"copilot"},
		Homes:  []string{"${COPILOT_HOME:-~/.copilot}"},
		Skills: "${COPILOT_HOME:-~/.copilot}/skills",
		// personal: ~/.copilot/skills or ~/.agents/skills (`copilot skill
		// --help`); $COPILOT_HOME moves ~/.copilot. ~/.github/skills is not
		// read: .github/skills is a project directory.
		Source: "https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/create-skills",
	},
	{
		ID: "codex", Name: "OpenAI Codex CLI",
		Bins:   []string{"codex"},
		Homes:  []string{"${CODEX_HOME:-~/.codex}"},
		Skills: "~/.agents/skills",
		// $HOME/.agents/skills; $CODEX_HOME/skills is the deprecated location
		// (codex-rs/ext/skills/src/host_roots.rs)
		Source: "https://developers.openai.com/codex/skills",
	},
	{
		ID: "gemini", Name: "Gemini CLI",
		Bins:   []string{"gemini"},
		Homes:  []string{"~/.gemini"},
		Skills: "~/.gemini/skills",
		Source: "https://geminicli.com/docs/cli/skills/",
	},
}

// Env is what the installer reads from the machine (tests replace it).
type Env struct {
	Home     string
	Getenv   func(string) string
	LookPath func(string) (string, error)
}

// DefaultEnv is this process's user and environment.
func DefaultEnv() Env {
	home, _ := os.UserHomeDir()
	return Env{Home: home, Getenv: os.Getenv, LookPath: exec.LookPath}
}

// Lookup finds an agent by its ID.
func Lookup(id string) (Agent, bool) {
	for _, a := range Agents {
		if a.ID == id {
			return a, true
		}
	}
	return Agent{}, false
}

// AgentIDs lists the supported agents' IDs.
func AgentIDs() []string {
	ids := make([]string, len(Agents))
	for i, a := range Agents {
		ids[i] = a.ID
	}
	return ids
}

// expand resolves a path template; "" when it is not an absolute path
// (no home, or a relative value in the variable).
func (e Env) expand(t string) string {
	for {
		i := strings.Index(t, "${")
		if i < 0 {
			break
		}
		j := strings.IndexByte(t[i:], '}')
		if j < 0 {
			return ""
		}
		name, def, _ := strings.Cut(t[i+2:i+j], ":-")
		v := ""
		if e.Getenv != nil {
			v = e.Getenv(name)
		}
		if v == "" {
			v = def
		}
		t = t[:i] + v + t[i+j+1:]
	}
	if t == "~" || strings.HasPrefix(t, "~/") || strings.HasPrefix(t, `~\`) {
		if e.Home == "" {
			return ""
		}
		t = e.Home + t[1:]
	}
	p := filepath.Clean(filepath.FromSlash(t))
	if !filepath.IsAbs(p) {
		return ""
	}
	return p
}

// SkillsDir is the agent's user-global skills directory ("" if unknown).
func (e Env) SkillsDir(a Agent) string { return e.expand(a.Skills) }

// detect says whether the agent looks installed, and why.
func (e Env) detect(a Agent) (bool, string) {
	for _, b := range a.Bins {
		if e.LookPath == nil {
			break
		}
		if _, err := e.LookPath(b); err == nil {
			return true, b + " on PATH"
		}
	}
	for _, h := range a.Homes {
		if p := e.expand(h); p != "" {
			if fi, err := os.Stat(p); err == nil && fi.IsDir() {
				return true, p
			}
		}
	}
	return false, ""
}
