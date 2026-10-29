package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestHubEntraLogin: `longarm login` with a fake Azure CLI against a hub
// whose Entra authority is a fake (JWKS): a user's first sign-in creates
// the user and enrolls the device; a service principal (CI) signs in as
// its configured user; with --az, a missing az login tells the user what
// to run.
func TestHubEntraLogin(t *testing.T) {
	const (
		tenant, client = entraTenant, entraClient
		ciApp          = "c1c1c1c1-0000-4000-8000-000000000001"
	)
	a := newFakeAuthority(t)
	sign := a.sign
	h := newHubEnv(t, a.hubConfig(fmt.Sprintf("admins = [\"boss@example.com\"]\napps = [\"%s=ci\"]", ciApp)))

	// the fake az: account show prints type.txt, get-access-token token.txt
	// (or fail.txt to stderr, exit 1)
	azDir := t.TempDir()
	azPath, script := filepath.Join(azDir, "az"), "#!/bin/sh\nd=$(dirname \"$0\")\n"+
		"if [ \"$2\" = show ]; then cat \"$d/type.txt\"; exit 0; fi\n"+
		"if [ -f \"$d/fail.txt\" ]; then cat \"$d/fail.txt\" >&2; exit 1; fi\ncat \"$d/token.txt\"\n"
	if runtime.GOOS == "windows" {
		azPath, script = filepath.Join(azDir, "az.cmd"), "@echo off\r\n"+
			"if \"%2\"==\"show\" (type \"%~dp0type.txt\" & exit /b 0)\r\n"+
			"if exist \"%~dp0fail.txt\" (type \"%~dp0fail.txt\" 1>&2 & exit /b 1)\r\n"+
			"type \"%~dp0token.txt\"\r\n"
	}
	set := func(name, content string) {
		if err := os.WriteFile(filepath.Join(azDir, name), []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeExe(t, azPath, []byte(script))
	azEnv := []string{"LONGARM_AZ=" + azPath}

	// a user
	userTok := sign(map[string]any{"oid": "eeeeeeee-0000-4000-8000-000000000001", "scp": "longarm.connect",
		"preferred_username": "erin@example.com", "azp": "04b07795-8ddb-461a-bbee-02f9e1bf7b46"})
	set("type.txt", "user")
	set("token.txt", userTok)
	erin := filepath.Join(h.root, "erin")
	r := h.longarmWith(azEnv, erin, "login", h.addr(), "--ca-fingerprint", h.fp)
	if r.code != 0 || !strings.Contains(r.stdout, "signed in to") || !strings.Contains(r.stdout, "client/erin-") {
		t.Fatalf("longarm login: %+v", r)
	}
	if r := h.longarmWith(nil, erin, "whoami"); r.code != 0 || !strings.Contains(r.stdout, "user erin ") {
		t.Fatalf("whoami: %+v", r)
	}
	// again on the same machine: no --force needed, same device
	if r := h.longarmWith(azEnv, erin, "login", h.addr(), "--ca-fingerprint", h.fp); r.code != 0 || !strings.Contains(r.stdout, "client/erin-") {
		t.Fatalf("second login: %+v", r)
	}

	// CI: a service principal's app-only token
	set("type.txt", "servicePrincipal")
	set("token.txt", sign(map[string]any{"oid": "5e5e5e5e-0000-4000-8000-000000000003", "idtyp": "app", "azp": ciApp}))
	ci := filepath.Join(h.root, "ci")
	if r := h.longarmWith(azEnv, ci, "login", h.addr(), "--ca-fingerprint", h.fp, "--name", "runner-1"); r.code != 0 || !strings.Contains(r.stdout, "client/runner-1") {
		t.Fatalf("service principal login: %+v", r)
	}
	if r := h.longarmWith(nil, ci, "whoami"); r.code != 0 || !strings.Contains(r.stdout, "user ci ") {
		t.Fatalf("ci whoami: %+v", r)
	}

	// not signed in to az, with --az (without, login falls back to pairing: TestHubPairing)
	set("type.txt", "user")
	set("fail.txt", "ERROR: AADSTS70043: The refresh token has expired. Please run 'az login' to setup account.")
	r = h.longarmWith(azEnv, filepath.Join(h.root, "frank"), "login", h.addr(), "--ca-fingerprint", h.fp, "--az")
	if want := "az login --tenant " + tenant + " --scope api://" + client + "/longarm.connect"; r.code == 0 || !strings.Contains(r.stderr, want) {
		t.Fatalf("not signed in: %+v, want %q", r, want)
	}
	// the token never shows up in output or the hub's log
	logb, _ := os.ReadFile(filepath.Join(h.root, "hub.log"))
	if strings.Contains(string(logb), userTok) {
		t.Fatal("the access token is in the hub log")
	}
	if !strings.Contains(string(logb), "node joined by entra sign-in") {
		t.Fatalf("hub log without the sign-in:\n%s", logb)
	}
}
