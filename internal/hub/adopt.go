package hub

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	"longarm/internal/hub/store"
	"longarm/internal/nodeid"
	"longarm/internal/pki"
	pb "longarm/proto/longarm/v1"
)

// Adoption is a durable assignment of the existing machines, keyed by node ID.
type Adoption struct {
	Devices map[string]string `json:"devices"`
	Workers map[string]string `json:"workers"`
}

// Adopt queues the host operator's one-time adoption, using the same WAL store
// as a running hub. The hub applies the plan under its own mutex.
func Adopt(st *store.Store, user string, mappings []string) (*Adoption, error) {
	if !pki.ValidUser(user) {
		return nil, fmt.Errorf("invalid user %q", user)
	}
	nodes, err := st.Nodes()
	if err != nil {
		return nil, err
	}
	plan := &Adoption{Devices: map[string]string{}, Workers: map[string]string{}}
	var candidates []nodeid.Candidate
	for _, n := range nodes {
		if n.State == stateRevoked {
			continue
		}
		if n.Role == string(pki.RoleClient) || n.Role == string(pki.RoleAdmin) {
			plan.Devices[n.ID] = user
		} else if n.Role == string(pki.RoleWorker) && n.Owner == "" && !n.Admin {
			plan.Workers[n.ID] = user
		} else {
			continue
		}
		candidates = append(candidates, nodeid.Candidate{ID: n.ID, Name: n.Name, State: n.State})
	}
	users := []string{user}
	seen := map[string]bool{}
	for _, m := range mappings {
		target, owner, ok := strings.Cut(m, "=")
		if !ok || !pki.ValidUser(owner) {
			return nil, fmt.Errorf("invalid mapping %q: want <device-or-worker>=<user>", m)
		}
		t, err := nodeid.ParseTarget(target)
		if err != nil {
			return nil, err
		}
		n, err := t.Resolve(candidates, "adoptable machine")
		if err != nil {
			return nil, err
		}
		if seen[n.ID] {
			return nil, fmt.Errorf("duplicate mapping for %s", target)
		}
		seen[n.ID] = true
		if _, ok := plan.Devices[n.ID]; ok {
			plan.Devices[n.ID] = owner
		} else {
			plan.Workers[n.ID] = owner
		}
		users = append(users, owner)
	}
	slices.Sort(users)
	var tenants []store.Tenant
	for _, name := range slices.Compact(users) {
		b, err := protojson.Marshal(&pb.TenantUser{Name: name})
		if err != nil {
			return nil, err
		}
		tenants = append(tenants, store.Tenant{Name: name, Data: b})
	}
	b, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	if err := st.QueueAdoption(tenants, string(b)); err != nil {
		return nil, err
	}
	return plan, nil
}

func (h *Hub) syncAdoptionLocked() error {
	if h.adoptionLoaded {
		return nil
	}
	raw, err := h.store.Meta("adoption")
	if err != nil || raw == "" {
		return err
	}
	var plan Adoption
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		return err
	}
	if err := h.loadTenants(); err != nil {
		return err
	}
	for _, assignments := range []map[string]string{plan.Devices, plan.Workers} {
		for id := range assignments {
			if h.adoptNodeLocked(id) == nil {
				return fmt.Errorf("adoption: cannot load node %s", id)
			}
		}
	}
	for id, n := range h.nodes {
		if n.rec.State == stateRevoked {
			continue
		}
		rec := *n.rec
		if user := plan.Devices[id]; user != "" && (rec.Role == string(pki.RoleClient) || rec.Role == string(pki.RoleAdmin)) {
			rec.User = user
		}
		if owner := plan.Workers[id]; owner != "" && isWorker(n) && rec.Owner == "" && !rec.Admin {
			rec.Owner = owner
			rec.AccessVersion++
		}
		if rec.User == n.rec.User && rec.Owner == n.rec.Owner {
			continue
		}
		if err := h.store.PutNode(&rec); err != nil {
			return err
		}
		n.rec = &rec
		if isWorker(n) {
			h.sendLocked(n, workerAccessCmd(workerAccessLocked(n)))
		}
		h.publishNodeLocked(n, "adopted by hub operator")
	}
	h.adoptionLoaded = true
	h.checkTenancyFlipLocked()
	h.recheckPortLinksLocked()
	h.kickQueue()
	return nil
}
