package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSecurityModes(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "[security]\nstage = 1\n")
	h.mustLongarm(h.admin, "admin", "user", "add", "alice", "--no-code")
	if out := h.mustLongarm(h.client, "security", "status"); !strings.Contains(out, "security: legacy (hub stage 1)") ||
		!strings.Contains(out, "end-to-end trust is available: longarm security upgrade shadow (see docs)") {
		t.Fatalf("status: %s", out)
	}
	if out := h.mustLongarm(h.client, "security", "upgrade", "shadow"); !strings.Contains(out, "security: shadow") {
		t.Fatalf("upgrade: %s", out)
	}
	if out := h.mustLongarm(h.client, "whoami"); !strings.Contains(out, "security: shadow (hub stage 1)") {
		t.Fatalf("whoami: %s", out)
	}
	var who struct {
		SecurityMode  string `json:"security_mode"`
		SecurityStage int    `json:"security_stage"`
	}
	if out := h.mustLongarm(h.client, "whoami", "--json"); json.Unmarshal([]byte(out), &who) != nil ||
		who.SecurityMode != "shadow" || who.SecurityStage != 1 {
		t.Fatalf("whoami JSON: %s", out)
	}
	if out := h.mustLongarm(h.admin, "admin", "user", "ls"); !strings.Contains(out, "SECURITY") || !strings.Contains(out, "shadow") {
		t.Fatalf("user ls: %s", out)
	}
	h.mustLongarm(h.admin, "admin", "user", "set", "alice", "--security-mode", "e2e")
	if r := h.longarm(h.client, "security", "upgrade", "shadow"); r.code == 0 || !strings.Contains(r.stderr, "cannot be downgraded") {
		t.Fatalf("downgrade: %+v", r)
	}
	h.killHub()
	h.startHub(t)
	if out := h.mustLongarm(h.client, "whoami"); !strings.Contains(out, "security: e2e (hub stage 1)") {
		t.Fatalf("persisted whoami: %s", out)
	}
	if out := h.mustLongarm(h.admin, "admin", "audit", "--node", "hub", "--limit", "100"); !strings.Contains(out, "users/security-mode") {
		t.Fatalf("audit: %s", out)
	}
}
