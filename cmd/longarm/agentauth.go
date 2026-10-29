package main

// Agent credentials (--agent-auth): Claude Code / Copilot CLI / gh
// credentials of this machine, forwarded per job, exec or session to the
// worker like --secret-env (docs/usage.md#agent-credentials). The readers
// and the forwarding rules live in internal/agentauth; this is the glue for
// the commands that start a program on a worker: run, exec, session start,
// attach, the MCP job tools - and handoff (forwardAgentAuth is its hook).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/agentauth"
	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/machine"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// agentAuthOpts are the --agent-auth flags of a command.
type agentAuthOpts struct {
	modes          []string
	sharedOK       bool
	copilotAccount string
	// keepCopilotHome: no private COPILOT_HOME on the worker even when
	// Copilot credentials go along (handoff resumes a Copilot session
	// copied into the account's ~/.copilot).
	keepCopilotHome bool
	// allowed limits the agents (nil = any): the MCP server forwards only
	// what the operator allowed (LONGARM_MCP_AGENT_AUTH).
	allowed []string
}

const agentAuthFlagHelp = "forward coding-agent credentials of this machine to the job (memory-only on the worker, redacted): " +
	"auto (when the command is claude or copilot), claude, copilot, gh, none; repeatable or comma-separated " +
	"(default: workers.toml [agent_auth] default, else auto)"

// register adds --agent-auth, --agent-auth-shared-ok and --copilot-account.
func (o *agentAuthOpts) register(c *cobra.Command) {
	c.Flags().StringArrayVar(&o.modes, "agent-auth", nil, agentAuthFlagHelp)
	c.Flags().BoolVar(&o.sharedOK, "agent-auth-shared-ok", false,
		"forward agent credentials to a worker of somebody else with shared isolation too, where other users' jobs could read them")
	c.Flags().StringVar(&o.copilotAccount, "copilot-account", "",
		"forward this account of the Copilot token pool (~/.copilot/accounts/tokens.json) as Copilot's credential")
}

// agentAuthStderr is where notes go (a variable for tests).
var agentAuthStderr = os.Stderr

// forwardAgentAuth resolves --agent-auth for a command about to start argv
// on worker (over conn) and adds the credentials to its env and secrets:
// what the user gave with -e / --secret-env wins over a forwarded value. It
// returns the request's agent_auth (nil = nothing forwarded) and the maps.
//
// This is the hook for every command that starts an agent on a worker: call
// it after the worker is known and before the request is sent; the secrets
// travel in secret_env, worker-direct, like --secret-env. Errors are usage
// errors (exit 2): an agent named explicitly without credentials here, a
// worker the rules refuse (agentauth.Check), an older worker.
func forwardAgentAuth(ctx context.Context, s *session, conn *client.Conn, worker string, argv []string, o agentAuthOpts,
	env, secrets map[string]string) (*pb.AgentAuth, map[string]string, map[string]string, error) {
	sel, err := agentauth.ParseSelection(o.modes, s.cfg.AgentAuth.Default)
	if err != nil {
		return nil, env, secrets, usageErr(err)
	}
	agents, explicit := sel.Resolve(argv)
	if o.allowed != nil {
		for _, a := range agents {
			if explicit(a) && !slices.Contains(o.allowed, a) {
				return nil, env, secrets, usagef("agent_auth %s is not allowed: list it in LONGARM_MCP_AGENT_AUTH when registering longarm mcp "+
					"(allowed now: %s)", a, strings.Join(o.allowed, ","))
			}
		}
		agents = slices.DeleteFunc(agents, func(a string) bool { return !slices.Contains(o.allowed, a) })
	}
	if o.copilotAccount != "" && !slices.Contains(agents, agentauth.Copilot) {
		return nil, env, secrets, usagef("--copilot-account goes with forwarded Copilot credentials (--agent-auth copilot, or a copilot command)")
	}
	if len(agents) == 0 {
		return nil, env, secrets, nil
	}
	anyExplicit := slices.ContainsFunc(agents, explicit)
	note := func(msg string) { fmt.Fprintln(agentAuthStderr, "longarm: "+msg) }

	ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
	info, err := conn.Node().Info(ictx, &pb.InfoRequest{})
	cancel()
	if err != nil {
		return nil, env, secrets, err
	}
	if !slices.Contains(info.Features, agentauth.Feature) {
		if anyExplicit {
			return nil, env, secrets, usagef("--agent-auth: worker %s (longarmd %s) does not support it; update longarmd", worker, info.Version)
		}
		note(fmt.Sprintf("worker %s (longarmd %s) predates agent credential forwarding: %s runs without forwarded credentials",
			worker, info.Version, strings.Join(agents, ", ")))
		return nil, env, secrets, nil
	}
	t := agentauth.Target{Worker: worker, Me: info.CallerUser, Owner: info.Owner, Isolation: info.Isolation, Admin: info.AdminWorker}
	if n := s.hubNode(worker); n != nil {
		t.HubKnown = true
		if n.Owner != "" {
			t.Owner = n.Owner // the hub's word
		}
		t.Admin = t.Admin || machine.FromNode(n).Admin
	}
	dec, err := agentauth.Check(t, anyExplicit, o.sharedOK)
	if err != nil {
		return nil, env, secrets, usageErr(err)
	}
	if dec.Note != "" {
		note(dec.Note)
	}
	if dec.Skip {
		return nil, env, secrets, nil
	}

	src := agentauth.System(s.cfg.Dir, s.cfg.AgentAuth, o.copilotAccount)
	b, err := src.Collect(ctx, agents, explicit)
	if err != nil {
		var miss *agentauth.MissingError
		if errors.As(err, &miss) {
			return nil, env, secrets, usagef("--agent-auth %s: %s", miss.Agent, miss.Why)
		}
		return nil, env, secrets, usageErr(err)
	}
	for _, n := range b.Notes {
		note(n)
	}
	if len(b.Agents) == 0 {
		return nil, env, secrets, nil
	}
	note(fmt.Sprintf("forwarding agent credentials to %s: %s", worker, strings.Join(b.Sources, "; ")))
	env, secrets = b.Merge(env, secrets)
	b.Clear()
	return &pb.AgentAuth{Agents: b.Agents, CopilotHome: b.Has(agentauth.Copilot) && !o.keepCopilotHome}, env, secrets, nil
}

// hubNode is the hub's record of worker (nil = not hub-known, or no hub).
func (s *session) hubNode(worker string) *pb.NodeInfo {
	if !s.cfg.Hub.Enabled() {
		return nil
	}
	s.refreshNodes(false)
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	if s.placed != nil && s.placed.Name == worker {
		return s.placed
	}
	for _, n := range s.nodes {
		if n.Name == worker {
			return n
		}
	}
	return nil
}
