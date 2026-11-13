package signin

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	pb "longarm/proto/longarm/v1"
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
	f.set("token.txt", "fake-access-token") // opaque to longarm login; gitleaks flags JWT-shaped strings
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
	info := &pb.AuthInfoResponse{EntraTenantId: "11111111-2222-4333-8444-555555555555", EntraClientId: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", EntraScope: "api://aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee/longarm.connect"}
	ctx := context.Background()

	// a user: the delegated scope, in the hub's tenant
	tok, err := AzToken(ctx, info, "")
	if err != nil || tok != "fake-access-token" {
		t.Fatalf("token %q %v", tok, err)
	}
	if call := az.lastCall(t); !strings.Contains(call, "get-access-token --scope api://aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee/longarm.connect") || !strings.Contains(call, "--tenant 11111111-2222-4333-8444-555555555555") {
		t.Fatalf("az %s", call)
	}
	// --scope overrides
	if _, err := AzToken(ctx, info, "api://aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee/other"); err != nil {
		t.Fatal(err)
	}
	if call := az.lastCall(t); !strings.Contains(call, "--scope api://aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee/other") {
		t.Fatalf("az %s", call)
	}
	// a service principal (CI): app-only token for .default, no --tenant
	az.set("type.txt", "servicePrincipal")
	if _, err := AzToken(ctx, info, ""); err != nil {
		t.Fatal(err)
	}
	if call := az.lastCall(t); !strings.Contains(call, "--scope api://aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee/.default") || strings.Contains(call, "--tenant") {
		t.Fatalf("az %s", call)
	}
	// not signed in: the exact az login to run
	az.set("type.txt", "user")
	az.set("fail.txt", "ERROR: AADSTS70043: The refresh token has expired. Please run 'az login' to setup account.")
	_, err = AzToken(ctx, info, "")
	if err == nil || !strings.Contains(err.Error(), "az login --tenant 11111111-2222-4333-8444-555555555555 --scope api://aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee/longarm.connect") || !Fallback(err) {
		t.Fatalf("not signed in: %v", err)
	}
	// other failures keep az's message
	az.set("fail.txt", "ERROR: something else broke")
	if _, err = AzToken(ctx, info, ""); err == nil || !strings.Contains(err.Error(), "something else broke") || Fallback(err) {
		t.Fatalf("az failure: %v", err)
	}
	// no az at all
	t.Setenv(EnvAz, filepath.Join(az.dir, "missing"))
	if _, err = AzToken(ctx, info, ""); err == nil || !strings.Contains(err.Error(), "az login --tenant 11111111-2222-4333-8444-555555555555") || !errors.Is(err, ErrNoAz) {
		t.Fatalf("missing az: %v", err)
	}
}

// A hub gets tokens for its own app only: az is not even asked for another
// resource's (Azure management, Graph, another app's).
func TestAzRefusesForeignScope(t *testing.T) {
	az := newFakeAz(t)
	const tenant, client = "11111111-2222-4333-8444-555555555555", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	for _, info := range []*pb.AuthInfoResponse{
		{EntraTenantId: tenant, EntraClientId: client, EntraScope: "https://management.azure.com/.default"},
		{EntraTenantId: tenant, EntraClientId: client, EntraScope: "https://graph.microsoft.com/Mail.Read"},
		{EntraTenantId: tenant, EntraClientId: client, EntraScope: "api://bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee/longarm.connect"},
		{EntraTenantId: tenant, EntraClientId: client, EntraScope: "api://" + client + "/a/b"},
		{EntraTenantId: tenant, EntraClientId: client, EntraScope: "api://" + client + "/x --scope https://management.azure.com/.default"},
		{EntraTenantId: tenant, EntraClientId: "https://management.azure.com", EntraScope: "api://https://management.azure.com/.default"},
		{EntraTenantId: "common --allow-no-subscriptions", EntraClientId: client, EntraScope: "api://" + client + "/longarm.connect"},
	} {
		if _, err := AzToken(context.Background(), info, ""); err == nil || Fallback(err) {
			t.Errorf("%v: %v", info, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(az.dir, "args.txt")); len(b) != 0 {
		t.Fatalf("az was run: %s", b)
	}
	for _, s := range []string{"longarm.connect", ".default"} {
		info := &pb.AuthInfoResponse{EntraTenantId: tenant, EntraClientId: client, EntraScope: "api://" + client + "/" + s}
		if _, err := AzToken(context.Background(), info, ""); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
}

// A service principal (CI) never falls back to a flow that waits for a person.
func TestAzServicePrincipalNoFallback(t *testing.T) {
	az := newFakeAz(t)
	az.set("type.txt", "servicePrincipal")
	az.set("fail.txt", "ERROR: AADSTS7000215: Invalid client secret provided. Please run 'az login'.")
	_, err := AzToken(context.Background(), &pb.AuthInfoResponse{EntraTenantId: "11111111-2222-4333-8444-555555555555", EntraClientId: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}, "")
	if err == nil || Fallback(err) || !strings.Contains(err.Error(), "service principal") {
		t.Fatalf("service principal failure: %v", err)
	}
}

// EnterPresses sends a press to whoever waits for it; one nobody waits for
// is dropped (it must not open a later code's page).
func TestEnterPresses(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	ch := EnterPresses(r)
	w.Write([]byte("\n")) // read now (a pipe); nobody waits: dropped
	time.Sleep(100 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("a press nobody waited for was kept")
	case <-time.After(100 * time.Millisecond):
	}
	go w.Write([]byte("\n"))
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no press delivered")
	}
}
