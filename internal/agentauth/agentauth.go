// Package agentauth forwards the credentials of coding agents (Claude Code,
// Copilot CLI, gh) from the client to a job, exec or session on a worker
// (longarm run / exec / session start / attach --agent-auth, docs/usage.md
// "Agent credentials"). Workers then need no agent logins of their own, and
// nothing is stored on them: the credentials travel like --secret-env
// (worker-direct, memory-only on the worker, injected into the process
// environment, redacted from logs and transcripts).
//
// This package is the client side: which agents a command needs (Selection,
// Detect), where their credentials come from (Sources: Claude Code's
// settings.json env block and environment, gh, the Copilot token pool), and
// to which workers they may go (Check). It reads files and runs gh, but
// never prints or logs a value.
package agentauth

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Agents and modes of --agent-auth.
const (
	Claude  = "claude"
	Copilot = "copilot"
	GH      = "gh"
	Auto    = "auto"
	None    = "none"
)

// Feature is the name of the capability, both in longarm version --json and
// in a worker's InfoResponse.features.
const Feature = "agent-auth"

// Agents are the agents credentials can be forwarded for, in order.
var Agents = []string{Claude, Copilot, GH}

// Config is the [agent_auth] section of workers.toml.
type Config struct {
	// Default is the mode without --agent-auth: "auto" (default) or "none"
	// (or an explicit list such as "claude,copilot").
	Default string `toml:"default"`
	// CopilotGHUser: the gh account whose token Copilot CLI gets
	// (gh auth token --user <login>) when the environment has none.
	CopilotGHUser string `toml:"copilot_gh_user"`
	// GHUser: the gh account of --agent-auth gh (default: gh's active one).
	GHUser string `toml:"gh_user"`
}

// Selection is a parsed --agent-auth.
type Selection struct {
	// Auto: forward the credentials of the agent the command runs (Detect).
	Auto bool
	// Explicit are agents named explicitly; always forwarded.
	Explicit []string
}

// ParseSelection parses --agent-auth values (repeatable, each
// comma-separated): auto, none, claude, copilot, gh. "none" cannot be
// combined with anything else. No values at all yield def (the configured
// default; "" = auto).
func ParseSelection(vals []string, def string) (Selection, error) {
	if len(vals) == 0 {
		if strings.TrimSpace(def) == "" {
			def = Auto
		}
		s, err := ParseSelection([]string{def}, "")
		if err != nil {
			return s, fmt.Errorf("[agent_auth] default: %w", err)
		}
		return s, nil
	}
	var s Selection
	none := false
	n := 0
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			p = strings.ToLower(strings.TrimSpace(p))
			if p == "" {
				continue
			}
			n++
			switch p {
			case Auto:
				s.Auto = true
			case None:
				none = true
			case Claude, Copilot, GH:
				if !slices.Contains(s.Explicit, p) {
					s.Explicit = append(s.Explicit, p)
				}
			default:
				return Selection{}, fmt.Errorf("--agent-auth %q: want auto, none, claude, copilot or gh", p)
			}
		}
	}
	if none && n > 1 {
		return Selection{}, errors.New("--agent-auth none cannot be combined with other values")
	}
	if n == 0 {
		return Selection{}, errors.New("--agent-auth: empty value")
	}
	return s, nil
}

// IsNone reports whether nothing would ever be forwarded.
func (s Selection) IsNone() bool { return !s.Auto && len(s.Explicit) == 0 }

// Resolve returns the agents to forward for a command: the explicit ones,
// plus with Auto the one Detect finds. explicit reports whether an agent was
// named (a missing credential is then an error, not a note).
func (s Selection) Resolve(argv []string) (agents []string, explicit func(string) bool) {
	set := map[string]bool{}
	for _, a := range s.Explicit {
		set[a] = true
	}
	auto := ""
	if s.Auto {
		auto = Detect(argv)
	}
	for _, a := range Agents {
		if set[a] || a == auto {
			agents = append(agents, a)
		}
	}
	return agents, func(a string) bool { return slices.Contains(s.Explicit, a) }
}

// Detect names the agent a command runs by its program alone (argv[0]'s
// base name, case-insensitive, without .exe / .cmd / .bat / .ps1 / .js):
// claude or copilot, else "". Commands wrapped in a shell (pwsh -c, sh -c,
// cmd /c, npx) are not looked into: name the agent with --agent-auth.
func Detect(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	// either separator: a Windows path from any client OS
	p := strings.ReplaceAll(argv[0], `\`, "/")
	b := strings.ToLower(p[strings.LastIndex(p, "/")+1:])
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1", ".js"} {
		b = strings.TrimSuffix(b, ext)
	}
	switch b {
	case Claude, Copilot:
		return b
	}
	return ""
}

// IsSecretName reports whether an environment variable of Claude Code's
// settings.json env block is a credential (forwarded as a secret: memory-only
// and redacted on the worker) rather than plain configuration (forwarded as
// job environment, visible in job.json and jobs --json).
func IsSecretName(name string) bool {
	u := strings.ToUpper(name)
	switch u {
	case "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN":
		return true
	}
	for _, s := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD"} {
		if strings.Contains(u, s) {
			return true
		}
	}
	return false
}
