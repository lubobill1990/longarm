package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/agentskill"
	"github.com/lubobill1990/longarm/internal/version"
)

// skillEnv is the machine the skill commands act on (tests replace it).
var skillEnv = agentskill.DefaultEnv

const skillLong = `The Longarm agent skill (SKILL.md and reference files) ships inside longarm.
These commands copy it into a coding agent's user-global skills directory
(opt-in: nothing is written until you install), report it, and remove it.

  longarm skill status [--agent ID] [--json]
  longarm skill install [--agent ID]...   default: every detected agent
  longarm skill update                    refresh the copies Longarm installed (after an update)
  longarm skill remove [--agent ID]...    default: every copy Longarm installed

Agents: %s.
A copy carries a manifest (.longarm-skill.json: version, sha256 per file).
Longarm updates or removes only copies it installed that nobody changed
since; a changed copy, or a "longarm" skill Longarm did not install, is left
alone and the command exits 2 naming the files. Files you added are kept.`

func skillCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "skill",
		Short: "Install the Longarm skill into Claude Code, Copilot CLI, Codex or Gemini CLI (opt-in), show or remove it",
		Long:  fmt.Sprintf(skillLong, skillAgentList()),
	}
	var agents []string
	var asJSON bool
	status := &cobra.Command{
		Use: "status", Short: "Show each agent: detected, skill installed (version), bundled version", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sts, err := skillStatuses(agents)
			if err != nil {
				return err
			}
			return printSkillStatus(cmd.OutOrStdout(), sts, asJSON)
		},
	}
	status.Flags().StringSliceVar(&agents, "agent", nil, "only these agents ("+strings.Join(agentskill.AgentIDs(), ", ")+")")
	status.Flags().BoolVar(&asJSON, "json", false, `{"agents": [{"agent","name","detected","dir","state","installed_version","bundled_version","modified","extra","note"}]}`)

	var instAgents []string
	install := &cobra.Command{
		Use: "install", Short: "Install (or update) the skill for the given agents, default: every detected agent", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env := skillEnv()
			ids := instAgents
			if len(ids) == 0 {
				all, err := env.StatusAll()
				if err != nil {
					return err
				}
				for _, s := range all {
					if s.Detected {
						ids = append(ids, s.Agent)
					}
				}
				if len(ids) == 0 {
					return usagef("no coding agent detected; name one with --agent (%s)", strings.Join(agentskill.AgentIDs(), ", "))
				}
			}
			return skillEach(cmd.OutOrStdout(), ids, env.Install, "installed")
		},
	}
	install.Flags().StringSliceVar(&instAgents, "agent", nil, "agents to install for (repeatable or comma-separated)")

	update := &cobra.Command{
		Use: "update", Short: "Update the copies Longarm installed that are older than this longarm's and unmodified", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			up, err := skillEnv().Refresh()
			for _, s := range up {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: updated to %s (%s)\n", s.Name, s.Installed, s.Dir)
			}
			if len(up) == 0 && err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "nothing to update")
			}
			return err
		},
	}

	var rmAgents []string
	remove := &cobra.Command{
		Use: "remove", Aliases: []string{"rm"}, Short: "Remove the copies Longarm installed (only its own, unmodified files)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env := skillEnv()
			ids := rmAgents
			if len(ids) == 0 {
				all, err := env.StatusAll()
				if err != nil {
					return err
				}
				for _, s := range all {
					if s.State != agentskill.Absent && s.State != agentskill.Foreign {
						ids = append(ids, s.Agent)
					}
				}
				if len(ids) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "no copy installed by Longarm")
					return nil
				}
			}
			return skillEach(cmd.OutOrStdout(), ids, env.Remove, "removed")
		},
	}
	remove.Flags().StringSliceVar(&rmAgents, "agent", nil, "agents to remove it from (repeatable or comma-separated)")

	c.AddCommand(status, install, update, remove)
	return c
}

func skillAgentList() string {
	var parts []string
	for _, a := range agentskill.Agents {
		parts = append(parts, a.ID+" ("+a.Name+": "+a.Skills+")")
	}
	return strings.Join(parts, ", ")
}

func skillStatuses(ids []string) ([]agentskill.Status, error) {
	env := skillEnv()
	if len(ids) == 0 {
		return env.StatusAll()
	}
	var out []agentskill.Status
	for _, id := range ids {
		st, err := env.Status(id)
		if err != nil {
			return nil, skillErr(err)
		}
		out = append(out, st)
	}
	return out, nil
}

// skillEach applies op to each agent; refusals and unknown agents are
// usage errors (exit 2) after the others were done.
func skillEach(w io.Writer, ids []string, op func(string) (agentskill.Status, error), done string) error {
	var errs []error
	for _, id := range ids {
		st, err := op(id)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", skillName(id), err))
			continue
		}
		switch {
		case done == "removed" && st.State == agentskill.Absent:
			fmt.Fprintf(w, "%s: removed (%s)\n", st.Name, st.Dir)
		case done == "removed":
			fmt.Fprintf(w, "%s: %s\n", st.Name, st.Note)
		default:
			fmt.Fprintf(w, "%s: %s %s (%s)\n", st.Name, done, st.Installed, st.Dir)
		}
	}
	return skillErr(errors.Join(errs...))
}

func skillName(id string) string {
	if a, ok := agentskill.Lookup(id); ok {
		return a.Name
	}
	return id
}

func skillErr(err error) error {
	var re *agentskill.RefusedError
	if errors.Is(err, agentskill.ErrUnknownAgent) || errors.As(err, &re) {
		return usageErr(err)
	}
	return err
}

func printSkillStatus(w io.Writer, sts []agentskill.Status, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"agents": sts})
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tDETECTED\tSKILL\tINSTALLED\tBUNDLED\tDIR")
	for _, s := range sts {
		det := "no"
		if s.Detected {
			det = "yes"
		}
		state := string(s.State)
		if s.State == agentskill.Absent {
			state = "-"
		}
		inst := s.Installed
		if inst == "" {
			inst = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Agent, det, state, inst, s.Bundled, s.Dir)
	}
	tw.Flush()
	for _, s := range sts {
		if s.Note != "" {
			fmt.Fprintf(w, "%s: %s\n", s.Name, s.Note)
		}
		if len(s.Modified) > 0 {
			fmt.Fprintf(w, "%s: changed since Longarm installed it: %s\n", s.Name, strings.Join(s.Modified, ", "))
		}
	}
	return nil
}

// refreshSkills updates the copies of the skill Longarm installed after
// longarm itself was updated (best effort; called by longarm daemon).
func refreshSkills() {
	if version.Version == "dev" { // a local build: leave the installed copies to releases
		return
	}
	up, err := skillEnv().Refresh()
	for _, s := range up {
		fmt.Fprintf(os.Stderr, "longarm: updated the Longarm skill for %s to %s\n", s.Name, s.Installed)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "longarm: updating the Longarm skill:", err)
	}
}
