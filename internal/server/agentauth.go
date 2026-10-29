package server

import (
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// agentNames are the agents whose forwarded credentials a request may name
// (internal/agentauth on the client).
var agentNames = []string{"claude", "copilot", "gh"}

// agentAuthFromPB checks a request's agent_auth: the agents' names (sorted,
// for the spec and its comparison on a retried submit) and whether the
// process gets a private COPILOT_HOME. The credentials themselves came in
// secret_env and are handled like any secret: memory-only, redacted.
func agentAuthFromPB(a *pb.AgentAuth) ([]string, bool, error) {
	if a == nil {
		return nil, false, nil
	}
	var out []string
	for _, n := range a.Agents {
		if !slices.Contains(agentNames, n) {
			return nil, false, status.Errorf(codes.InvalidArgument, "agent_auth: unknown agent %q", n)
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, a.CopilotHome, nil
}
