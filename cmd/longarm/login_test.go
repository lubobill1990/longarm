package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// fakeAz installs a fake Azure CLI ($LONGARM_AZ): `account show` prints
// the account type, `account get-access-token` the token or (with fail
// set) an error; every call's arguments are appended to args.txt.
type fakeAz struct{ dir string }

func newFakeAz(t *testing.T) *fakeAz {
	t.Helper()
	dir := t.TempDir()
	path, script := filepath.Join(dir, "az"), `#!/bin/sh
d=$(dirname "$0")
echo "$*" >> "$d/args.txt"
if [ "$2" = show ]; then cat "$d/type.txt"; exit 0; fi
if [ -f "$d/fail.txt" ]; then cat "$d/fail.txt" >&2; exit 1; fi
cat "$d/token.txt"
`
	if runtime.GOOS == "windows" {
		path, script = filepath.Join(dir, "az.cmd"), "@echo off\r\necho %*>>\"%~dp0args.txt\"\r\n"+
			"if \"%2\"==\"show\" (type \"%~dp0type.txt\" & exit /b 0)\r\n"+
			"if exist \"%~dp0fail.txt\" (type \"%~dp0fail.txt\" 1>&2 & exit /b 1)\r\n"+
			"type \"%~dp0token.txt\"\r\n"
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAz, path)
	f := &fakeAz{dir: dir}
	f.set("type.txt", "user")
	f.set("token.txt", "eyJ0b2tlbg.eyJ1c2VyIjoxfQ.c2ln")
	return f
}

func (f *fakeAz) set(name, content string) {
	os.WriteFile(filepath.Join(f.dir, name), []byte(content+"\n"), 0o600)
}

// lastCall is the last get-access-token call's arguments.
func (f *fakeAz) lastCall(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(f.dir, "args.txt"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	os.Remove(filepath.Join(f.dir, "args.txt"))
	return strings.TrimSpace(lines[len(lines)-1])
}

func TestAzAccessToken(t *testing.T) {
	az := newFakeAz(t)
	info := &pb.AuthInfoResponse{EntraTenantId: "tttt", EntraClientId: "cccc", EntraScope: "api://cccc/longarm.connect"}
	ctx := context.Background()

	// a user: the delegated scope, in the hub's tenant
	tok, err := azAccessToken(ctx, info, "")
	if err != nil || tok != "eyJ0b2tlbg.eyJ1c2VyIjoxfQ.c2ln" {
		t.Fatalf("token %q %v", tok, err)
	}
	if call := az.lastCall(t); !strings.Contains(call, "get-access-token --scope api://cccc/longarm.connect") || !strings.Contains(call, "--tenant tttt") {
		t.Fatalf("az %s", call)
	}
	// --scope overrides
	if _, err := azAccessToken(ctx, info, "api://cccc/other"); err != nil {
		t.Fatal(err)
	}
	if call := az.lastCall(t); !strings.Contains(call, "--scope api://cccc/other") {
		t.Fatalf("az %s", call)
	}
	// a service principal (CI): app-only token for .default, no --tenant
	az.set("type.txt", "servicePrincipal")
	if _, err := azAccessToken(ctx, info, ""); err != nil {
		t.Fatal(err)
	}
	if call := az.lastCall(t); !strings.Contains(call, "--scope api://cccc/.default") || strings.Contains(call, "--tenant") {
		t.Fatalf("az %s", call)
	}
	// not signed in: the exact az login to run
	az.set("type.txt", "user")
	az.set("fail.txt", "ERROR: AADSTS70043: The refresh token has expired. Please run 'az login' to setup account.")
	_, err = azAccessToken(ctx, info, "")
	if err == nil || !strings.Contains(err.Error(), "az login --tenant tttt --scope api://cccc/longarm.connect") {
		t.Fatalf("not signed in: %v", err)
	}
	// other failures keep az's message
	az.set("fail.txt", "ERROR: something else broke")
	if _, err = azAccessToken(ctx, info, ""); err == nil || !strings.Contains(err.Error(), "something else broke") {
		t.Fatalf("az failure: %v", err)
	}
	// no az at all
	t.Setenv(EnvAz, filepath.Join(az.dir, "missing"))
	if _, err = azAccessToken(ctx, info, ""); err == nil || !strings.Contains(err.Error(), "az login --tenant tttt") {
		t.Fatalf("missing az: %v", err)
	}
}
