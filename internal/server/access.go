package server

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// What admin certificates of non-owners may do on a worker ([access] admins).
const (
	AdminsFull    = "full"    // everything (the default)
	AdminsOwn     = "own"     // what a client certificate of their user may do
	AdminsObserve = "observe" // Node.Info and metrics without process lists
	AdminsNone    = "none"    // Node.Info only
)

// Access is the worker owner's local access policy ([access] in
// longarmd.toml, docs/security.md). It is never taken from the hub: it
// limits what the hub's admins and users may do on this machine. The zero
// value (and nil) changes nothing.
type Access struct {
	// Users may use the worker (empty = everybody the hub admits).
	Users []string
	// Owners keep full rights for their role whatever Admins says, and are
	// users implicitly.
	Owners []string
	// Admins is what other admin certificates may do (Admins* constants;
	// "" = full).
	Admins string
}

// Validate checks the user names and the admins mode.
func (a *Access) Validate() error {
	switch a.Admins {
	case "", AdminsFull, AdminsOwn, AdminsObserve, AdminsNone:
	default:
		return fmt.Errorf("admins %q: want %q, %q, %q or %q", a.Admins, AdminsFull, AdminsOwn, AdminsObserve, AdminsNone)
	}
	for _, list := range []struct {
		key   string
		names []string
	}{{"users", a.Users}, {"owners", a.Owners}} {
		for _, u := range list.names {
			if !pki.ValidUser(u) {
				return fmt.Errorf("%s: invalid user name %q", list.key, u)
			}
		}
	}
	return nil
}

// Active reports whether the policy restricts anybody (it then turns on
// ownership checks: without them everybody acts as an admin).
func (a *Access) Active() bool {
	return a != nil && (len(a.Users) > 0 || len(a.Owners) > 0 || a.mode() != AdminsFull)
}

func (a *Access) mode() string {
	if a == nil || a.Admins == "" {
		return AdminsFull
	}
	return a.Admins
}

// IsOwner reports whether user is one of the worker's owners.
func (a *Access) IsOwner(user string) bool {
	return a != nil && slices.Contains(a.Owners, user)
}

// AllowsUser reports whether user may use the worker at all.
func (a *Access) AllowsUser(user string) bool {
	return a == nil || len(a.Users) == 0 || slices.Contains(a.Users, user) || a.IsOwner(user)
}

// limitsAdmin reports whether id is an admin certificate whose admin rights
// the policy takes away.
func (a *Access) limitsAdmin(id pki.Identity) bool {
	return id.Role == pki.RoleAdmin && a.mode() != AdminsFull && !a.IsOwner(id.Owner())
}

// AdminRights reports whether id acts with admin rights here: an admin
// certificate of an owner, or of anybody while admins = "full".
func (a *Access) AdminRights(id pki.Identity) bool {
	return id.Role == pki.RoleAdmin && !a.limitsAdmin(id)
}

// observeMethods are what admins = "observe" leaves (metrics lose their
// process lists: callers without admin rights never see them).
var observeMethods = []string{pb.Node_Info_FullMethodName, pb.Metrics_Snapshot_FullMethodName, pb.Metrics_Stream_FullMethodName}

// Deny returns why the policy refuses id calling method ("" = allowed). It
// runs after the role table: it only ever takes rights away.
func (a *Access) Deny(id pki.Identity, method string) string {
	if a == nil || method == pb.Node_Info_FullMethodName {
		return "" // clients find out who they are and why they're refused
	}
	if !a.AllowsUser(id.Owner()) {
		return "not a user of this worker"
	}
	if !a.limitsAdmin(id) {
		return ""
	}
	switch a.mode() {
	case AdminsOwn:
		if !slices.Contains(MethodRoles[method], pki.RoleClient) {
			return "admins = own: admin-only call"
		}
	case AdminsObserve:
		if !slices.Contains(observeMethods, method) {
			return "admins = observe"
		}
	default: // none (and anything Validate would reject)
		return "admins = none"
	}
	return ""
}

// String summarizes the effective policy (the start log line).
func (a *Access) String() string {
	users := "any"
	if a != nil && len(a.Users) > 0 {
		users = strings.Join(a.Users, ",")
	}
	owners := "-"
	if a != nil && len(a.Owners) > 0 {
		owners = strings.Join(a.Owners, ",")
	}
	return fmt.Sprintf("users=%s owners=%s admins=%s", users, owners, a.mode())
}
