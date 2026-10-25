package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/update"
)

// findBash returns a bash for the deploy script tests: the system one on
// Unix, Git for Windows' on Windows (never WSL's System32\bash.exe).
func findBash(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		if p, err := exec.LookPath("bash"); err == nil {
			return p
		}
		t.Skip("no bash")
	}
	if git, err := exec.LookPath("git"); err == nil {
		// <git>\cmd\git.exe -> <git>\bin\bash.exe
		p := filepath.Join(filepath.Dir(filepath.Dir(git)), "bin", "bash.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p := `C:\Program Files\Git\bin\bash.exe`
	if _, err := os.Stat(p); err == nil {
		return p
	}
	t.Skip("no Git for Windows bash")
	return ""
}

// buildFakeHub builds testdata/fakehub into path: it reports version and
// passes every other command to the real longarm-hub realHub (old: like an
// longarm-hub without release verify).
func buildFakeHub(version, realHub, path string, old bool) error {
	goBin := filepath.Join(runtime.GOROOT(), "bin", exeName("go"))
	if _, err := os.Stat(goBin); err != nil {
		goBin = "go"
	}
	ld := fmt.Sprintf("-X main.version=%s -X 'main.realHub=%s'", version, realHub)
	if old {
		ld += " -X main.oldHub=1"
	}
	if b, err := exec.Command(goBin, "build", "-ldflags", ld, "-o", path, "./testdata/fakehub").CombinedOutput(); err != nil {
		return fmt.Errorf("build fakehub %s: %v\n%s", version, err, b)
	}
	return nil
}

// TestHubReleaseVerify: `longarm-hub release verify` accepts exactly the file,
// name and version the signature was made for, with the key from hub.toml
// or --public-key.
func TestHubReleaseVerify(t *testing.T) {
	t.Parallel()
	hubExe := buildHub(t)
	root := t.TempDir()
	pub, keyFile := releaseKey(t, root)
	f := filepath.Join(root, "longarm-hub-linux-amd64")
	os.WriteFile(f, []byte("hub binary"), 0o755)
	signFile(t, keyFile, f, "longarm-hub-linux-amd64", "1.0.0")
	cfg := filepath.Join(root, "hub.toml")
	os.WriteFile(cfg, []byte(fmt.Sprintf("data_dir = %q\n[releases]\npublic_key = %q\n", filepath.ToSlash(root), pub)), 0o600)
	run := func(args ...string) (string, error) {
		out, err := exec.Command(hubExe, append([]string{"--config", cfg, "release", "verify"}, args...)...).CombinedOutput()
		return string(out), err
	}
	if out, err := run(f, "--version", "1.0.0"); err != nil || !strings.Contains(out, "valid signature") {
		t.Fatalf("verify: %v %s", err, out)
	}
	if out, err := run(f, "--version", "1.0.0", "--public-key", pub, "--config", ""); err != nil {
		t.Fatalf("verify --public-key: %v %s", err, out)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{f, "--version", "1.0.1"}, "signature is for"},                                   // replayed for another version
		{[]string{f, "--version", "1.0.0", "--name", "longarmd-linux-amd64"}, "signature is for"}, // another file
	} {
		if out, err := run(c.args...); err == nil || !strings.Contains(out, c.want) {
			t.Errorf("verify %v: %v %s", c.args, err, out)
		}
	}
	os.WriteFile(f, []byte("hub binary, tampered"), 0o755)
	if out, err := run(f, "--version", "1.0.0"); err == nil || !strings.Contains(out, "invalid signature") {
		t.Errorf("tampered: %v %s", err, out)
	}
	os.WriteFile(cfg, []byte(fmt.Sprintf("data_dir = %q\n", filepath.ToSlash(root))), 0o600)
	if out, err := run(f, "--version", "1.0.0"); err == nil || !strings.Contains(out, "no public key") {
		t.Errorf("no key: %v %s", err, out)
	}
}

func releaseKey(t *testing.T, dir string) (pub, keyFile string) {
	t.Helper()
	pub, priv, err := update.GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	keyFile = filepath.Join(dir, "release.key")
	if err := os.WriteFile(keyFile, priv, 0o600); err != nil {
		t.Fatal(err)
	}
	return pub, keyFile
}

// signFile writes file.minisig for (name, version).
func signFile(t *testing.T, keyFile, file, name, version string) {
	t.Helper()
	k, err := update.LoadPrivateKey(keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file+update.SigExt, update.Sign(k, must(os.ReadFile(file)), name, version), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHubDeployScript runs deploy/hub/longarm-hub-deploy (the CI's SSH forced
// command) against a fake systemd: the real longarm-hub installed, "releases"
// 2.0.0 and 2.1.0 that report their version and verify signatures with the
// real longarm-hub, the hub's real release store (longarm-hub release add),
// signatures checked by the installed binary, a /healthz that reports
// whatever binary the fake "systemctl restart" started. Covers command
// validation, check (dry run), deploy, idempotent re-deploy, a failed health
// check with automatic rollback, rollback, bad / missing signatures, the
// HTTP fallback and the version probe.
func TestHubDeployScript(t *testing.T) {
	t.Parallel()
	bash := findBash(t)
	script, _ := filepath.Abs("../deploy/hub/longarm-hub-deploy")
	root := t.TempDir()
	slash := filepath.ToSlash
	for _, d := range []string{"bin", "bin-old", "rel", "state", "http/releases/2.2.0", "fake"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	const file = "longarm-hub-linux-amd64"
	// installed: the current build ("dev"); releases 2.0.0 (good) and 2.1.0
	// (its health check fails)
	bin := filepath.Join(root, "bin", exeName("longarm-hub")) // .exe: Go only runs those on Windows
	realHub := buildHub(t)
	copyBin(t, realHub, bin)
	var wg sync.WaitGroup
	for _, v := range []string{"2.0.0", "2.1.0", "0.3.0"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := buildFakeHub(v, realHub, filepath.Join(root, "build-"+v), v == "0.3.0"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	// the hub's /healthz: version of the "running" binary
	unhealthy := map[string]bool{"2.1.0": true}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	running := filepath.Join(root, "state", "running")
	os.WriteFile(running, []byte("dev"), 0o644)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		v := strings.TrimSpace(string(must(os.ReadFile(running))))
		if v == "" || unhealthy[v] {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"version":%q,"workers":0}`, v)
	})
	mux.Handle("GET /releases/", http.StripPrefix("/releases/", http.FileServer(http.Dir(filepath.Join(root, "http", "releases")))))
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	// hub.toml with the release key; the release store via longarm-hub release add
	pub, keyFile := releaseKey(t, root)
	dataDir := filepath.Join(root, "data")
	cfg := filepath.Join(root, "hub.toml")
	os.WriteFile(cfg, []byte(fmt.Sprintf("data_dir = %q\nhttp = %q   # local listener\n\n[releases]\npublic_key = %q\n",
		slash(dataDir), ln.Addr().String(), pub)), 0o600)
	os.WriteFile(filepath.Join(root, "nokey.toml"), []byte(fmt.Sprintf("data_dir = %q\nhttp = %q\n", slash(dataDir), ln.Addr().String())), 0o600)
	for _, v := range []string{"2.0.0", "2.1.0"} {
		d := filepath.Join(root, "rel", v)
		os.MkdirAll(d, 0o755)
		copyBin(t, filepath.Join(root, "build-"+v), filepath.Join(d, file))
		signFile(t, keyFile, filepath.Join(d, file), file, v)
		if out, err := exec.Command(bin, "--config", cfg, "release", "add", d, "--version", v).CombinedOutput(); err != nil {
			t.Fatalf("release add %s: %v %s", v, err, out)
		}
	}
	// 2.0.9 on disk with 2.0.0's signature (a replay); 2.2.0 only over
	// HTTP, validly signed, but it is the 2.0.0 binary.
	bad := filepath.Join(dataDir, "releases", "2.0.9")
	os.MkdirAll(bad, 0o755)
	copyBin(t, filepath.Join(root, "build-2.0.0"), filepath.Join(bad, file))
	os.WriteFile(filepath.Join(bad, file+update.SigExt), must(os.ReadFile(filepath.Join(root, "rel", "2.0.0", file+update.SigExt))), 0o644)
	h220 := filepath.Join(root, "http", "releases", "2.2.0", file)
	copyBin(t, filepath.Join(root, "build-2.0.0"), h220)
	signFile(t, keyFile, h220, file, "2.2.0")

	// fake systemctl: start = the installed binary runs (2.1.0 "migrates"
	// the database first), stop = nothing runs
	fake := filepath.Join(root, "fake", "systemctl")
	os.WriteFile(fake, []byte(`#!/usr/bin/env bash
echo "$*" >> "$FAKE_STATE/systemctl.log"
case $1 in
start | restart)
	v=$("$LONGARM_HUB_DEPLOY_BIN" --version 2>/dev/null | sed -n 's/^longarm-hub version //p')
	if [[ $v == 2.1.0 ]]; then echo "schema 99" >> "$FAKE_DB"; fi
	echo "$v" > "$FAKE_STATE/running"
	;;
stop) : > "$FAKE_STATE/running" ;;
esac
`), 0o755)
	// minisign stand-in for a hub whose longarm-hub predates release verify:
	// checks with the real longarm-hub, prints minisign's output format
	fakeMinisign := filepath.Join(root, "fake", "minisign")
	os.WriteFile(fakeMinisign, []byte(`#!/usr/bin/env bash
while [ $# -gt 0 ]; do
	case $1 in -P) pub=$2; shift ;; -m) f=$2; shift ;; -x) sig=$2; shift ;; esac
	shift
done
tc=$(sed -n 's/^trusted comment: //p' "$sig")
name=${tc#longarm file:}; name=${name%% *}; ver=${tc##*version:}
if ! "$FAKE_REAL_HUB" release verify "$f" --sig "$sig" --public-key "$pub" --version "$ver" --name "$name" >/dev/null 2>&1; then
	echo "Signature verification failed"
	exit 1
fi
echo "Signature and comment signature verified"
echo "Trusted comment: $tc"
`), 0o755)
	db := filepath.Join(dataDir, "hub.db")

	type res struct {
		out  string
		code int
	}
	deploy := func(sshCmd string, extraEnv ...string) res {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bash, slash(script))
		cmd.Env = append(os.Environ(),
			"SSH_ORIGINAL_COMMAND="+sshCmd,
			"LONGARM_HUB_DEPLOY_NO_SUDO=1",
			"LONGARM_HUB_DEPLOY_BIN="+slash(bin),
			"LONGARM_HUB_DEPLOY_CONFIG="+slash(cfg),
			"LONGARM_HUB_DEPLOY_SYSTEMCTL="+slash(fake),
			"LONGARM_HUB_DEPLOY_TIMEOUT=4",
			"LONGARM_HUB_DEPLOY_LOCK="+slash(filepath.Join(root, "state", "lock")),
			"LONGARM_HUB_DEPLOY_ARCH=amd64",
			"LONGARM_HUB_DEPLOY_STATE="+slash(filepath.Join(root, "state", "deploy")),
			"FAKE_STATE="+slash(filepath.Join(root, "state")),
			"FAKE_DB="+slash(db),
			"FAKE_REAL_HUB="+slash(realHub),
		)
		cmd.Env = append(cmd.Env, extraEnv...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatalf("bash: %v", err)
		}
		t.Logf("$ %q -> %d\n%s", sshCmd, code, out.String())
		return res{out.String(), code}
	}
	version := func(p string) string {
		b, err := exec.Command(p, "--version").Output()
		if err != nil {
			return "none"
		}
		return strings.TrimPrefix(strings.TrimSpace(string(b)), "longarm-hub version ")
	}
	starts := func() int { // start + restart
		b, _ := os.ReadFile(filepath.Join(root, "state", "systemctl.log"))
		return strings.Count(string(b), "start longarm-hub")
	}
	expect := func(r res, code int, want ...string) {
		t.Helper()
		if r.code != code {
			t.Fatalf("exit %d, want %d:\n%s", r.code, code, r.out)
		}
		for _, w := range want {
			if !strings.Contains(r.out, w) {
				t.Fatalf("output lacks %q:\n%s", w, r.out)
			}
		}
	}
	state := func(wantBin, wantPrev, wantRunning string, wantStarts int) {
		t.Helper()
		run := strings.TrimSpace(string(must(os.ReadFile(running))))
		if v, p := version(bin), version(bin+".prev"); v != wantBin || p != wantPrev || run != wantRunning || starts() != wantStarts {
			t.Fatalf("installed %s, prev %s, running %s, starts %d; want %s, %s, %s, %d", v, p, run, starts(),
				wantBin, wantPrev, wantRunning, wantStarts)
		}
		for _, d := range []string{filepath.Dir(bin), filepath.Join(root, "bin-old")} {
			for _, e := range must(os.ReadDir(d)) {
				if strings.Contains(e.Name(), ".tmp.") || strings.HasPrefix(e.Name(), ".longarm-hub-deploy.") {
					t.Fatalf("temp files left: %s", e.Name())
				}
			}
		}
	}

	// ---- refused commands: nothing happens
	for _, c := range []string{"", "1.2.3; id", "$(id)", "`id`", "../../etc/passwd", "deploy", "deploy 1.2.3 x",
		"1.2", "v1.2.3", "rollback now", "check ../1.0.0", "deploy 1.2.3\nid", "status --x", "1.2.3-" + strings.Repeat("a", 40)} {
		r := deploy(c)
		if r.code != 2 || !strings.Contains(r.out, "longarm-hub-deploy: error:") {
			t.Fatalf("%q: exit %d\n%s", c, r.code, r.out)
		}
	}
	state("dev", "none", "dev", 0)
	expect(deploy("status"), 0, "installed: dev", "previous:  \n", "healthz:   dev")

	// ---- check = dry run
	expect(deploy("check 2.0.0"), 0, "signature OK: longarm-hub-linux-amd64 2.0.0", "nothing changed")
	state("dev", "none", "dev", 0)
	expect(deploy("check 2.0.9"), 2, "signature is for")                            // replayed signature
	expect(deploy("check 9.9.9"), 2, "no signed longarm-hub-linux-amd64 for 9.9.9") // not published
	expect(deploy("check 2.2.0"), 2, "fetched longarm-hub-linux-amd64 2.2.0 from http://", "signature OK", "reports version '2.0.0'")
	expect(deploy("check 2.0.0", "LONGARM_HUB_DEPLOY_CONFIG="+slash(filepath.Join(root, "nokey.toml"))), 2, "no [releases] public_key")
	state("dev", "none", "dev", 0)

	// ---- deploy (the CI sends the bare version)
	expect(deploy("2.0.0"), 0, "deploying longarm-hub 2.0.0 (installed: dev)", "2.0.0 deployed and healthy")
	state("2.0.0", "dev", "2.0.0", 1)
	expect(deploy("deploy 2.0.0"), 0, "already installed and healthy") // re-run: no restart
	state("2.0.0", "dev", "2.0.0", 1)

	// ---- a release that fails its health check is rolled back, with the
	// database it migrated
	dbBefore := must(os.ReadFile(db))
	expect(deploy("2.1.0"), 1, "did not report 2.1.0", "rolled back to 2.0.0 (database restored)")
	state("2.0.0", "dev", "2.0.0", 3)
	if !bytes.Equal(must(os.ReadFile(db)), dbBefore) {
		t.Fatal("database not restored after the failed deploy")
	}
	if _, err := os.Stat(filepath.Join(root, "state", "deploy", "db-before-2.1.0", "hub.db")); err != nil {
		t.Fatalf("no database snapshot: %v", err)
	}

	// ---- manual rollback (and back again)
	expect(deploy("rollback"), 0, "rolled back to dev")
	state("dev", "2.0.0", "dev", 4)
	expect(deploy("rollback"), 0, "rolled back to 2.0.0")
	state("2.0.0", "dev", "2.0.0", 5)

	// ---- a rollback whose target doesn't come up goes back
	unhealthy["dev"] = true
	expect(deploy("rollback"), 1, "rollback to dev failed; still on 2.0.0")
	state("2.0.0", "dev", "2.0.0", 7)
	delete(unhealthy, "dev")

	// ---- bootstrap: the installed longarm-hub predates release verify ->
	// minisign checks the signature, or nothing is installed
	old := filepath.Join(root, "bin-old", exeName("longarm-hub"))
	copyBin(t, filepath.Join(root, "build-0.3.0"), old)
	oldEnv := []string{"LONGARM_HUB_DEPLOY_BIN=" + slash(old)}
	expect(deploy("2.0.0", append(oldEnv, "LONGARM_HUB_DEPLOY_MINISIGN=no-such-minisign")...), 2, "minisign is not installed")
	expect(deploy("check 2.0.9", append(oldEnv, "LONGARM_HUB_DEPLOY_MINISIGN="+slash(fakeMinisign))...), 2, "signature is for")
	expect(deploy("2.0.0", append(oldEnv, "LONGARM_HUB_DEPLOY_MINISIGN="+slash(fakeMinisign))...), 0,
		"signature OK: longarm-hub-linux-amd64 2.0.0", "deploying longarm-hub 2.0.0 (installed: 0.3.0)", "2.0.0 deployed and healthy")
	if v, p := version(old), version(old+".prev"); v != "2.0.0" || p != "0.3.0" {
		t.Fatalf("old hub: installed %s, prev %s", v, p)
	}
}
