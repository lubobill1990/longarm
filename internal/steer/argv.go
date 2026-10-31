// Package steer is the logic of steerable runs (docs/steerable-runs.md):
// an agent (Claude Code, Copilot CLI) runs interactively on a job's pseudo
// terminal; its hooks report to the job's shim, which answers its start-up
// screens and ends the run when the agent is done. Pure functions and a
// state machine with an explicit clock; the shim does the I/O.
package steer

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Agents.
const (
	Claude  = "claude"
	Copilot = "copilot"
)

// AgentOf names the agent argv runs ("" = neither).
func AgentOf(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(argv[0], `\`, "/")))
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		base = strings.TrimSuffix(base, ext)
	}
	switch base {
	case Claude, Copilot:
		return base
	}
	return ""
}

// Claude Code options that only mean something with --print (dropped),
// with whether they take a value.
var claudePrintOnly = map[string]bool{
	"--output-format": true, "--input-format": true,
	"--include-partial-messages": false, "--replay-user-messages": false,
}

// Interactive turns argv - a print-mode command (claude -p "task", copilot
// -p "task") or an interactive one - into the interactive command of a
// steerable run.
func Interactive(agent string, argv []string) ([]string, error) {
	if AgentOf(argv) != agent || agent == "" {
		return nil, fmt.Errorf("a steerable run runs claude or copilot (the command's first word), not %q", first(argv))
	}
	out := []string{argv[0]}
	args := argv[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" { // the rest are arguments
			out = append(out, args[i:]...)
			break
		}
		name, _, hasValue := strings.Cut(a, "=")
		switch agent {
		case Claude:
			switch {
			case a == "-p" || a == "--print":
				continue
			case name == "--settings":
				return nil, fmt.Errorf("--settings: a steerable run passes its own settings file to claude (for its hooks)")
			}
			if takes, ok := claudePrintOnly[name]; ok {
				if takes && !hasValue {
					i++
				}
				continue
			}
		case Copilot:
			switch {
			case a == "-s" || a == "--silent":
				continue
			case a == "-p" || a == "--prompt":
				if i+1 >= len(args) {
					return nil, fmt.Errorf("%s needs a prompt", a)
				}
				out = append(out, "-i", args[i+1])
				i++
				continue
			case name == "--prompt" && hasValue:
				out = append(out, "-i", a[len("--prompt="):])
				continue
			case strings.HasPrefix(a, "-p") && len(a) > 2 && !strings.HasPrefix(a, "--"):
				out = append(out, "-i", a[2:])
				continue
			}
		}
		out = append(out, a)
	}
	return out, nil
}

// Inject adds the run's hook configuration to the agent's command:
// Claude's --settings file (Copilot's hooks are files in its home).
func Inject(agent string, argv []string, claudeSettings string) []string {
	if agent != Claude || len(argv) == 0 {
		return argv
	}
	out := append([]string{argv[0], "--settings", claudeSettings}, argv[1:]...)
	return out
}

func first(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return argv[0]
}
