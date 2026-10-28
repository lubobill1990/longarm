package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/update"
)

// bashPath turns a path into one bash and the tools it runs understand
// (Git for Windows: C:\x -> /c/x; MSYS converts it back for native programs).
func bashPath(p string) string {
	p = filepath.ToSlash(p)
	if runtime.GOOS == "windows" && len(p) > 2 && p[1] == ':' {
		p = "/" + strings.ToLower(p[:1]) + p[2:]
	}
	return p
}

// tlsSecret is LONGARM_RELEASE_TLS: base64 of a tar.gz with the bundle files.
func tlsSecret(t *testing.T, dir string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []string{pki.CACertFile, pki.NodeCertFile, pki.NodeKeyFile} {
		b := must(os.ReadFile(filepath.Join(dir, f)))
		tw.WriteHeader(&tar.Header{Name: f, Mode: 0o600, Size: int64(len(b)), ModTime: time.Now()})
		tw.Write(b)
	}
	tw.Close()
	gz.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestCIScripts runs the release workflow's helper scripts like the jobs do:
// .github/scripts/longarm-release.sh publish|rollout with the secrets in the
// environment, against a real hub with the release-role certificate, and
// hub-deploy.sh against a fake ssh. The secrets' temp dirs are gone
// afterwards, also when the script fails.
func TestCIScripts(t *testing.T) {
	t.Parallel()
	bash := findBash(t)
	h, keyFile := newReleaseHub(t)
	h.mustLongarm(h.client, "admin", "issue", "--role", "release", "--name", "ci", "--out", filepath.Join(h.root, "ci-tls"))
	runnerTemp := filepath.Join(h.root, "runner-temp")
	os.MkdirAll(runnerTemp, 0o755)
	scripts, _ := filepath.Abs("../.github/scripts")
	rel := filepath.Join(h.root, "rel")
	os.MkdirAll(rel, 0o755)
	copyBin(t, bins.longarmd, filepath.Join(rel, update.ReleaseName("longarmd", runtime.GOOS, runtime.GOARCH)))
	copyBin(t, bins.longarm, filepath.Join(rel, update.ReleaseName("longarm", runtime.GOOS, runtime.GOARCH)))
	secrets := []string{
		"LONGARM_HUB=" + h.addr(),
		"LONGARM_RELEASE_TLS=" + tlsSecret(t, filepath.Join(h.root, "ci-tls")),
		"LONGARM_RELEASE_KEY=" + string(must(os.ReadFile(keyFile))),
		"LONGARM_RELEASE_KEY_PASSWORD=",
		"RUNNER_TEMP=" + bashPath(runnerTemp),
	}
	run := func(env []string, script string, args ...string) result {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bash, append([]string{bashPath(filepath.Join(scripts, script))}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s %v -> %d\n%s%s", script, args, code, out.String(), errb.String())
		if ents, _ := os.ReadDir(runnerTemp); len(ents) != 0 {
			t.Fatalf("secrets left in RUNNER_TEMP: %v", ents)
		}
		return result{out.String(), errb.String(), code}
	}
	longarm := bashPath(bins.longarm)

	// sign: no hub, no certificate needed (releases without a deployment)
	signDir := filepath.Join(h.root, "rel-sign")
	os.MkdirAll(signDir, 0o755)
	copyBin(t, bins.longarm, filepath.Join(signDir, update.ReleaseName("longarm", runtime.GOOS, runtime.GOARCH)))
	noHub := []string{"LONGARM_HUB=", "LONGARM_RELEASE_TLS=", secrets[2], secrets[3], secrets[4]}
	if r := run(noHub, "longarm-release.sh", "sign", longarm, bashPath(signDir), "1.6.0"); r.code != 0 || !strings.Contains(r.stdout, "signed") {
		t.Fatalf("sign: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(signDir, update.ReleaseName("longarm", runtime.GOOS, runtime.GOARCH)+update.SigExt)); err != nil {
		t.Fatal("sign: no signature")
	}
	if r := run(append(noHub, "LONGARM_RELEASE_KEY="), "longarm-release.sh", "sign", longarm, bashPath(signDir), "1.6.0"); r.code == 0 || !strings.Contains(r.stderr, "secret LONGARM_RELEASE_KEY is not set") {
		t.Fatalf("sign without key: %+v", r)
	}

	// publish: sign + upload; a re-run is a no-op
	r := run(secrets, "longarm-release.sh", "publish", longarm, bashPath(rel), "1.7.0", "stable")
	if r.code != 0 || !strings.Contains(r.stdout, "1.7.0 (stable) on the hub: 2 files, signed=true") {
		t.Fatalf("publish: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(rel, update.ReleaseName("longarmd", runtime.GOOS, runtime.GOARCH)+update.SigExt)); err != nil {
		t.Fatal("not signed")
	}
	if r := run(secrets, "longarm-release.sh", "publish", longarm, bashPath(rel), "1.7.0", "stable"); r.code != 0 || !strings.Contains(r.stdout, "already on the hub") {
		t.Fatalf("re-publish: %+v", r)
	}
	if ls := h.mustLongarm(h.admin, "admin", "release", "ls"); !strings.Contains(ls, "1.7.0") {
		t.Fatalf("release ls: %s", ls)
	}
	// rollout: reaches the hub's push; no self-updating worker here, which
	// is a warning (push --allow-none exits 79), not a failure
	if r := run(secrets, "longarm-release.sh", "rollout", longarm, "1.7.0", "1s"); r.code != 0 || !strings.Contains(r.stdout, "::warning title=no self-updating worker::") ||
		!strings.Contains(r.stderr, "nothing rolled out") {
		t.Fatalf("rollout: %+v", r)
	}
	// missing / broken secrets: clear errors, nothing left behind
	if r := run(append(secrets, "LONGARM_RELEASE_TLS="), "longarm-release.sh", "publish", longarm, bashPath(rel), "1.7.1", "stable"); r.code == 0 || !strings.Contains(r.stderr, "secret LONGARM_RELEASE_TLS is not set") {
		t.Fatalf("no TLS: %+v", r)
	}
	if r := run(append(secrets, "LONGARM_RELEASE_TLS=bm90IGEgdGFyCg=="), "longarm-release.sh", "publish", longarm, bashPath(rel), "1.7.1", "stable"); r.code == 0 || !strings.Contains(r.stderr, "not a base64 tar.gz") {
		t.Fatalf("bad TLS: %+v", r)
	}
	if r := run(append(secrets, "LONGARM_RELEASE_KEY="), "longarm-release.sh", "publish", longarm, bashPath(rel), "1.7.1", "stable"); r.code == 0 || !strings.Contains(r.stderr, "secret LONGARM_RELEASE_KEY is not set") {
		t.Fatalf("no key: %+v", r)
	}

	// hub-deploy.sh: a fake ssh records its arguments and the key it got
	fake := filepath.Join(h.root, "fakebin")
	os.MkdirAll(fake, 0o755)
	os.WriteFile(filepath.Join(fake, "ssh"), []byte(`#!/usr/bin/env bash
key="" kh=""
while [ $# -gt 2 ]; do
	case $1 in
	-i) key=$2; shift ;;
	-o) case $2 in UserKnownHostsFile=*) kh=${2#UserKnownHostsFile=} ;; esac; shift ;;
	esac
	shift
done
echo "host=$1 cmd=$2"
echo "key=$(cat "$key") known_hosts=$(cat "$kh")"
`), 0o755)
	sshEnv := []string{
		"HUB_DEPLOY_SSH_COMMAND=" + bashPath(filepath.Join(fake, "ssh")),
		"HUB_SSH=deploy@hub.example",
		"HUB_DEPLOY_SSH_KEY=-----KEY-----\r\nabc\r\n",
		"HUB_SSH_KNOWN_HOSTS=hub.example ssh-ed25519 AAAA",
		"RUNNER_TEMP=" + bashPath(runnerTemp),
	}
	r = run(sshEnv, "hub-deploy.sh", "check", "1.7.0")
	if r.code != 0 || !strings.Contains(r.stdout, "host=deploy@hub.example cmd=check 1.7.0") ||
		!regexp.MustCompile(`key=-----KEY-----\nabc known_hosts=hub.example ssh-ed25519 AAAA`).MatchString(r.stdout) {
		t.Fatalf("hub-deploy: %+v", r)
	}
	if r := run(append(sshEnv, "HUB_DEPLOY_SSH_KEY="), "hub-deploy.sh", "status"); r.code == 0 || !strings.Contains(r.stderr, "secret HUB_DEPLOY_SSH_KEY is not set") {
		t.Fatalf("hub-deploy without key: %+v", r)
	}
}
