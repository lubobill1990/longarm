package agentauth

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"fmt"

	"github.com/lubobill1990/longarm/internal/pki"
)

// Target is what the client knows about the worker credentials would go to.
//
// Whose word it is matters (docs/security.md "Forwarded agent
// credentials"): Owner, HubKnown and Admin come from the hub, or from the
// worker that got them from the hub; Isolation is the worker's own
// configuration, from Node.Info over the verified mTLS connection. Without
// end-to-end trust the hub is trusted by design (it runs the CA), so its
// word on ownership counts. With end-to-end trust (E2E) it does not:
// "mine" must be proven by the user's own endorsement of the worker's node
// key (EndorsedByMe), and the caller takes Admin and Isolation from the
// endorsed worker's own report only.
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
	// E2E: this client requires end-to-end trust (require_workers); the
	// hub's (and hub-pushed) ownership claims don't count.
	E2E bool
	// EndorsedByMe: the worker's certificate carries an endorsement of its
	// node key by this device's own user key (EndorsedBy).
	EndorsedByMe bool
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

// Owned reports whether t is the caller's own worker. With end-to-end trust
// only the user's own endorsement proves it.
func (t Target) Owned() bool {
	if t.E2E {
		return t.EndorsedByMe
	}
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
	switch {
	case t.E2E:
		whose = "not endorsed by your user key (end-to-end trust: ownership is proven by your endorsement only)"
		if t.Owner != "" {
			whose = "said to be " + t.Owner + "'s, but " + whose
		}
		whose = "a worker " + whose + ","
	case t.Owner != "":
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

// EndorsedBy reports whether worker certificate c is accepted by trust
// policy pol (as in the TLS handshake) with an endorsement signed by the
// user key mine - this device's own: the user's proof that the worker is
// theirs, which no hub can forge.
func EndorsedBy(c *x509.Certificate, pol *pki.TrustPolicy, mine ed25519.PublicKey) bool {
	if c == nil || !pol.Active() || len(mine) != ed25519.PublicKeySize {
		return false
	}
	if _, err := pol.Check(c); err != nil {
		return false
	}
	t, err := pki.TrustOf(c)
	if err != nil || t == nil || t.Endorsement == nil {
		return false
	}
	return bytes.Equal(t.Endorsement.UserKey, mine)
}
