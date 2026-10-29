package agentauth

import (
	"fmt"
)

// Target is what the client knows about the worker credentials would go to.
type Target struct {
	Worker string
	// Me is the caller's user on the worker (InfoResponse.caller_user).
	Me string
	// Owner is the worker's owner ("" = none recorded).
	Owner string
	// HubKnown: the hub lists the worker. A hub worker without an owner is
	// a fleet worker (other users' jobs run there); a worker the hub does
	// not know, with no owner, is a personal one from workers.toml.
	HubKnown bool
	// Isolation is the worker's [isolation] mode: "shared" | "per-user".
	Isolation string
	// Admin: an admin worker (its jobs run as SYSTEM).
	Admin bool
}

// Decision is the outcome of Check.
type Decision struct {
	// Skip: forward nothing (Note says why); the command runs without.
	Skip bool
	// Note is printed on stderr (a warning when forwarding anyway).
	Note string
}

// RefusedError: forwarding to this worker is refused.
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return e.Msg }

// Owned reports whether t is the caller's own worker.
func (t Target) Owned() bool {
	if t.Owner != "" {
		return t.Me != "" && t.Owner == t.Me
	}
	return !t.HubKnown
}

// Check applies the forwarding rules (docs/security.md "Agent credentials"):
//
//   - an admin worker only gets credentials an agent was explicitly named for
//     (--agent-auth claude, never auto);
//   - the caller's own worker: yes;
//   - a worker of somebody else (shared with the caller, or a fleet worker)
//     with per-user isolation: yes - the job runs as the caller's own OS
//     account there;
//   - such a worker with shared isolation: other users' jobs run as the same
//     OS account and could read the job's environment, so only with
//     sharedOK (--agent-auth-shared-ok), and with a warning.
//
// explicit: an agent was named explicitly (not only auto-detected).
func Check(t Target, explicit, sharedOK bool) (Decision, error) {
	if t.Admin {
		if !explicit {
			return Decision{Skip: true, Note: fmt.Sprintf("%s is an admin worker (runs as SYSTEM): agent credentials are not forwarded automatically; name the agent (--agent-auth claude|copilot|gh) to forward them", t.Worker)}, nil
		}
		if !t.Owned() {
			return Decision{}, &RefusedError{fmt.Sprintf("%s is an admin worker of %s: agent credentials go to your own admin workers only", t.Worker, t.Owner)}
		}
		return Decision{Note: fmt.Sprintf("forwarding agent credentials to admin worker %s: the agent runs as SYSTEM there", t.Worker)}, nil
	}
	if t.Owned() {
		return Decision{}, nil
	}
	if t.Isolation == "per-user" {
		return Decision{}, nil
	}
	whose := "a fleet worker"
	if t.Owner != "" {
		whose = t.Owner + "'s worker"
	}
	if !sharedOK {
		return Decision{}, &RefusedError{fmt.Sprintf("%s is %s with shared isolation: other users' jobs run there as the same OS account and could read "+
			"your agent credentials from the job's environment. Not forwarded; use a worker of yours or one with per-user isolation, "+
			"pass --agent-auth-shared-ok to forward anyway, or --agent-auth none", t.Worker, whose)}
	}
	return Decision{Note: fmt.Sprintf("WARNING: forwarding agent credentials to %s, %s with shared isolation (--agent-auth-shared-ok): "+
		"other users' jobs there run as the same OS account and can read them", t.Worker, whose)}, nil
}
