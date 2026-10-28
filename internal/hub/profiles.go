package hub

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Toolchain profiles (DESIGN §3.1 toolchain, §7.2): a named, declarative
// list of tools with versions. Workers reconcile their assigned profile
// idempotently when no job runs. A changed profile goes to one canary node
// first; only when the canary reports "ok" for the new revision do the other
// nodes get it ("rolling" -> "done"). A failing canary halts the rollout and
// the others stay on the previous revision.
//
// Stored per profile: the latest revision (pb.Profile) and the last revision
// that passed its canary ("stable"), which non-canary nodes get meanwhile.

// KnownTools are the tools the reconciler knows how to install.
var KnownTools = []string{"git", "gh", "node", "pnpm", "uv", "pwsh", "claude", "copilot", "go"}

type profileRecord struct {
	Latest json.RawMessage `json:"latest"`
	Stable json.RawMessage `json:"stable,omitempty"`
}

func (h *Hub) saveProfileLocked(p, stable *pb.Profile) error {
	rec := profileRecord{}
	var err error
	if rec.Latest, err = protojson.Marshal(p); err != nil {
		return err
	}
	if stable != nil {
		if rec.Stable, err = protojson.Marshal(stable); err != nil {
			return err
		}
	}
	b, _ := json.Marshal(rec)
	return h.store.PutProfile(p.Name, b)
}

// loadProfiles fills h.profiles / h.stable from the store (called by load).
func (h *Hub) decodeProfile(d []byte) (latest, stable *pb.Profile, err error) {
	var rec profileRecord
	if err := json.Unmarshal(d, &rec); err != nil {
		return nil, nil, err
	}
	latest = &pb.Profile{}
	if err := protojson.Unmarshal(rec.Latest, latest); err != nil {
		return nil, nil, err
	}
	if len(rec.Stable) > 0 {
		stable = &pb.Profile{}
		if err := protojson.Unmarshal(rec.Stable, stable); err != nil {
			return nil, nil, err
		}
	}
	return latest, stable, nil
}

func validateTools(tools []*pb.ToolSpec) error {
	seen := map[string]bool{}
	for _, t := range tools {
		if !slices.Contains(KnownTools, t.Name) {
			return fmt.Errorf("unknown tool %q (known: %s)", t.Name, strings.Join(KnownTools, ", "))
		}
		if seen[t.Name] {
			return fmt.Errorf("tool %q listed twice", t.Name)
		}
		seen[t.Name] = true
		if t.Version == "" {
			t.Version = "latest"
		}
		if strings.ContainsAny(t.Version, " \t\r\n;&|`$<>\"'") {
			return fmt.Errorf("tool %s: bad version %q", t.Name, t.Version)
		}
	}
	return nil
}

// PutProfile creates or updates a profile; a change of tools starts a new
// revision with a canary rollout.
func (h *Hub) PutProfile(by string, p *pb.Profile) (*pb.Profile, error) {
	if !pki.ValidName(p.Name) {
		return nil, status.Errorf(codes.InvalidArgument, "invalid profile name %q", p.Name)
	}
	if err := validateTools(p.Tools); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	old := h.profiles[p.Name]
	if old != nil && toolsEqual(old.Tools, p.Tools) {
		return proto.Clone(old).(*pb.Profile), nil
	}
	np := &pb.Profile{Name: p.Name, Tools: p.Tools, Revision: 1, UpdatedAt: tspb(time.Now()), Rollout: "canary"}
	if old != nil {
		np.Revision = old.Revision + 1
	}
	stable := h.stable[p.Name]
	if err := h.saveProfileLocked(np, stable); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	h.profiles[p.Name] = np
	h.log.Info("profile changed", "profile", p.Name, "revision", np.Revision, "by", by)
	h.bus.publish(&pb.FleetEvent{Kind: "node", Message: fmt.Sprintf("profile %s revision %d by %s: canary rollout", p.Name, np.Revision, by)})
	h.advanceRolloutsLocked()
	return proto.Clone(h.profiles[p.Name]).(*pb.Profile), nil
}

func toolsEqual(a, b []*pb.ToolSpec) bool {
	return slices.EqualFunc(a, b, func(x, y *pb.ToolSpec) bool { return proto.Equal(x, y) })
}

// Profiles lists profiles.
func (h *Hub) Profiles() []*pb.Profile {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*pb.Profile
	for _, p := range h.profiles {
		out = append(out, proto.Clone(p).(*pb.Profile))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// AssignProfile sets a node's profile ("" = none).
func (h *Hub) AssignProfile(by, nodeName, profile string) (*pb.NodeInfo, error) {
	return h.nodeOp(nodeName, "profile "+profile+" assigned by "+by, func(n *node) error {
		if profile != "" {
			if _, ok := h.profiles[profile]; !ok {
				return status.Errorf(codes.NotFound, "no profile %q", profile)
			}
		}
		if n.rec.Role != string(pki.RoleWorker) {
			return status.Errorf(codes.FailedPrecondition, "%s is not a worker", nodeName)
		}
		if n.rec.Admin && profile != "" {
			// a toolchain profile is package operations as SYSTEM, started by the hub
			return status.Errorf(codes.PermissionDenied, "%s is an admin worker: it takes no toolchain profile (its owner installs software on it with explicit commands)", nodeName)
		}
		n.rec.Profile = profile
		h.sendLocked(n, &pb.HubCommand{Msg: &pb.HubCommand_Toolchain{Toolchain: h.desiredToolchainLocked(n)}})
		h.advanceRolloutsLocked()
		return nil
	})
}

// desiredToolchainLocked is what node n should run: the latest revision
// when it is the canary or the rollout passed the canary, else the stable
// revision (or nothing yet).
func (h *Hub) desiredToolchainLocked(n *node) *pb.ToolchainDesired {
	if n.rec.Admin {
		return &pb.ToolchainDesired{} // never: see AssignProfile
	}
	name := n.rec.Profile
	p := h.profiles[name]
	if name == "" || p == nil {
		return &pb.ToolchainDesired{}
	}
	if p.Rollout == "rolling" || p.Rollout == "done" || p.Canary == n.rec.Name {
		return &pb.ToolchainDesired{Profile: name, Revision: p.Revision, Tools: p.Tools}
	}
	if s := h.stable[name]; s != nil {
		return &pb.ToolchainDesired{Profile: name, Revision: s.Revision, Tools: s.Tools}
	}
	return &pb.ToolchainDesired{Profile: name} // revision 0: wait for the canary
}

func reportOK(n *node, p *pb.Profile) (ok, failed bool, msg string) {
	tc := n.toolchain
	if tc == nil || tc.Profile != p.Name || tc.Revision != p.Revision {
		return false, false, ""
	}
	return tc.State == "ok", tc.State == "failed", tc.Error
}

// advanceRolloutsLocked moves every profile's rollout forward.
func (h *Hub) advanceRolloutsLocked() {
	for _, p := range h.profiles {
		var assigned []*node
		for _, n := range h.nodes {
			if n.rec.Profile == p.Name && n.rec.Role == string(pki.RoleWorker) && n.rec.State == stateApproved && !n.rec.Admin {
				assigned = append(assigned, n)
			}
		}
		sort.Slice(assigned, func(i, j int) bool { return assigned[i].rec.Name < assigned[j].rec.Name })
		before := proto.Clone(p).(*pb.Profile)
		switch p.Rollout {
		case "canary":
			var canary *node
			for _, n := range assigned {
				if n.rec.Name == p.Canary {
					canary = n
				}
			}
			if canary == nil { // pick one (prefer online)
				p.Canary = ""
				for _, n := range assigned {
					if n.sess != nil {
						canary = n
						break
					}
				}
				if canary == nil && len(assigned) == 0 {
					p.Rollout, p.Message = "done", "no nodes assigned"
					h.stable[p.Name] = proto.Clone(p).(*pb.Profile)
					break
				}
				if canary != nil {
					p.Canary, p.Message = canary.rec.Name, "canary "+canary.rec.Name+" applying"
					h.sendLocked(canary, &pb.HubCommand{Msg: &pb.HubCommand_Toolchain{Toolchain: h.desiredToolchainLocked(canary)}})
				}
			}
			if canary == nil {
				break
			}
			if ok, failed, msg := reportOK(canary, p); ok {
				p.Rollout, p.Message = "rolling", "canary "+canary.rec.Name+" ok"
				h.stable[p.Name] = proto.Clone(p).(*pb.Profile)
				for _, n := range assigned {
					h.sendLocked(n, &pb.HubCommand{Msg: &pb.HubCommand_Toolchain{Toolchain: h.desiredToolchainLocked(n)}})
				}
			} else if failed {
				p.Rollout, p.Message = "halted", "canary "+canary.rec.Name+" failed: "+msg
			}
		case "rolling":
			all := true
			for _, n := range assigned {
				if ok, _, _ := reportOK(n, p); !ok {
					all = false
				}
			}
			if all {
				p.Rollout, p.Message = "done", fmt.Sprintf("%d nodes ok", len(assigned))
			}
		}
		if !proto.Equal(before, p) {
			if err := h.saveProfileLocked(p, h.stable[p.Name]); err != nil {
				h.log.Warn("save profile", "err", err)
			}
			h.log.Info("profile rollout", "profile", p.Name, "revision", p.Revision, "rollout", p.Rollout, "msg", p.Message)
			h.bus.publish(&pb.FleetEvent{Kind: "node", Message: fmt.Sprintf("profile %s revision %d: %s (%s)", p.Name, p.Revision, p.Rollout, p.Message)})
		}
	}
}
