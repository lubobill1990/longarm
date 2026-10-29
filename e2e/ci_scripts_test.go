package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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

// fakeGH stands in for gh in TestMirrorGimScript: the pull requests of both
// sides are JSON files in $FAKE_GH_DIR, POST/PATCH change them and every call
// is logged ("<METHOD> <path> <GH_TOKEN>", then the request body); a
// commit lookup asks the source's bare repository.
const fakeGH = `#!/usr/bin/env bash
set -euo pipefail
d=$FAKE_GH_DIR
jq() { command jq "$@" | tr -d '\r'; }
[ "$1" = api ] || exit 2
shift
method=GET path=
while [ $# -gt 0 ]; do
	case $1 in
	-X) method=$2; shift ;;
	--paginate | --silent) ;;
	--input) shift ;;
	*) path=$1 ;;
	esac
	shift
done
echo "$method $path ${GH_TOKEN:-none}" >>"$d/log"
case "$method $path" in
"GET repos/$SRC_REPO/pulls?"*) cat "$d/src-pulls.json" ;;
"GET repos/$DST_REPO/pulls?"*) cat "$d/dst-pulls.json" ;;
"GET repos/$SRC_REPO/commits/"*) git --git-dir="$FAKE_SRC_GIT" cat-file -e "${path##*/}^{commit}" ;;
"POST repos/$DST_REPO/pulls")
	b=$(cat)
	echo "$b" >>"$d/log"
	n=$(jq '(map(.number) | max // 0) + 1' "$d/dst-pulls.json")
	jq --argjson b "$b" --argjson n "$n" --arg r "$DST_REPO" '. + [$b + {number: $n, state: "open", merged_at: null,
		head: {ref: $b.head, repo: {full_name: $r}}, base: {ref: $b.base}}]' "$d/dst-pulls.json" >"$d/t" && mv "$d/t" "$d/dst-pulls.json"
	echo "{\"number\": $n}" ;;
"PATCH repos/$DST_REPO/pulls/"*)
	b=$(cat)
	echo "$b" >>"$d/log"
	jq --argjson b "$b" --argjson n "${path##*/}" 'map(if .number == $n then . + ($b | del(.base)) +
		(if $b.base then {base: {ref: $b.base}} else {} end) else . end)' "$d/dst-pulls.json" >"$d/t" && mv "$d/t" "$d/dst-pulls.json"
	echo "{}" ;;
*) echo "fake gh: unexpected $method $path" >&2; exit 2 ;;
esac
`

// TestMirrorGimScript runs .github/scripts/mirror-gim.sh (mirror-gim.yml)
// against two local bare repositories: "src" (this repository) and "dst"
// (gim-home, whose main carries governance commits of its own, merged in with
// unrelated histories), with a fake gh for the pull requests of both. dst also
// has things of its own the mirror must never touch: Dependabot's branch and
// pull request, another open native pull request whose head points at one of
// our commits, a dependabot/* branch without a pull request, a branch with
// commits only there.
func TestMirrorGimScript(t *testing.T) {
	t.Parallel()
	bash := findBash(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	root := t.TempDir()
	slash := filepath.ToSlash
	srcGit, dstGit, work, fake := filepath.Join(root, "src.git"), filepath.Join(root, "dst.git"), filepath.Join(root, "work"), filepath.Join(root, "gh")
	os.MkdirAll(fake, 0o755)
	idEnv := []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(root, "gitconfig")}
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, append(os.Environ(), idEnv...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(file, content, msg string) string { // in work, on the current branch
		t.Helper()
		p := filepath.Join(work, filepath.FromSlash(file))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
		git(work, "add", "-A")
		git(work, "commit", "-q", "-m", msg)
		return git(work, "rev-parse", "HEAD")
	}
	git(root, "init", "-q", "--bare", "-b", "main", srcGit)
	git(root, "init", "-q", "--bare", "-b", "main", dstGit)
	git(root, "init", "-q", "-b", "main", work)
	git(work, "remote", "add", "src", slash(srcGit))
	git(work, "remote", "add", "dst", slash(dstGit))
	a := commit("README.md", "one\n", "A")
	git(work, "checkout", "-q", "-b", "feat-1")
	commit("feat.txt", "1\n", "feat 1")
	git(work, "checkout", "-q", "-b", "stale", "main")
	commit("stale.txt", "s\n", "stale")
	git(work, "checkout", "-q", "-b", "fork3", "main")
	fork3 := commit("fork.txt", "3\n", "fork 3")
	git(work, "checkout", "-q", "-b", "fork4", "main")
	fork4 := commit(".github/workflows/evil.yml", "on: push\n", "fork 4")
	git(work, "checkout", "-q", "main")
	git(work, "push", "-q", "src", "main", "feat-1", "stale", fork3+":refs/pull/3/head", fork4+":refs/pull/4/head")
	// gim-home: its own governance history, merged with ours once
	git(work, "checkout", "-q", "--orphan", "gov")
	git(work, "rm", "-q", "-rf", ".")
	commit(".github/acl/access.yml", "acl\n", "governance")
	git(work, "merge", "-q", "--allow-unrelated-histories", "-m", "merge", "main")
	git(work, "push", "-q", "dst", "gov:refs/heads/main", a+":refs/heads/native-feature", a+":refs/heads/dependabot/npm/y")
	git(work, "checkout", "-q", "-b", "dep", "gov")
	dep := commit("go.mod", "bump\n", "dependabot bump")
	git(work, "checkout", "-q", "-b", "own", "gov")
	commit("own.txt", "o\n", "own")
	git(work, "push", "-q", "dst", "dep:refs/heads/dependabot/go_modules/x", "own:refs/heads/own-work")
	git(work, "checkout", "-q", "main")
	dstMain0 := git(work, "rev-parse", "gov")
	commit("README.md", "one and a half\n", "A2") // main moved on here since
	git(work, "push", "-q", "src", "main")

	writeJSON := func(name, s string) { os.WriteFile(filepath.Join(fake, name), []byte(s), 0o644) }
	srcPR := func(n int, ref, repo, title, body string) string {
		return fmt.Sprintf(`{"number":%d,"title":%q,"body":%q,"draft":false,"head":{"ref":%q,"repo":{"full_name":%q}},"base":{"ref":"main"}}`, n, title, body, ref, repo)
	}
	native := `{"number":1,"title":"Bump x","body":"Dependabot","state":"open","merged_at":null,"head":{"ref":"dependabot/go_modules/x","repo":{"full_name":"gim-home/longarm"}},"base":{"ref":"main"}},` +
		`{"number":2,"title":"native","body":null,"state":"open","merged_at":null,"head":{"ref":"native-feature","repo":{"full_name":"gim-home/longarm"}},"base":{"ref":"main"}}`
	writeJSON("dst-pulls.json", "["+native+"]")
	writeJSON("src-pulls.json", "["+srcPR(1, "feat-1", "example-owner/longarm", "Feature one", "Body 1\r\n")+","+
		srcPR(3, "patch-1", "someone/longarm", "Fork three", "")+","+srcPR(4, "main", "other/longarm", "Fork four", "x")+"]")
	ghPath := filepath.Join(fake, "gh.sh")
	os.WriteFile(ghPath, []byte(fakeGH), 0o755)
	script, _ := filepath.Abs("../.github/scripts/mirror-gim.sh")
	env := append(idEnv, "SRC_REPO=example-owner/longarm", "DST_REPO=gim-home/longarm", "SRC_URL="+slash(srcGit), "DST_URL="+slash(dstGit),
		"MIRROR_GH="+bashPath(ghPath), "FAKE_GH_DIR="+slash(fake), "FAKE_SRC_GIT="+slash(srcGit),
		"SRC_TOKEN=src-token", "GIM_MIRROR_TOKEN=dst-token", "GITHUB_STEP_SUMMARY=", "RUNNER_TEMP="+slash(root))
	run := func(extra ...string) result {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bash, bashPath(script))
		cmd.Env = append(append(os.Environ(), env...), extra...)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		t.Logf("mirror-gim.sh -> %d\n%s%s", code, out.String(), errb.String())
		if ents, _ := filepath.Glob(filepath.Join(root, "mirror-gim.*")); len(ents) != 0 {
			t.Fatalf("work dir left behind: %v", ents)
		}
		return result{out.String(), errb.String(), code}
	}
	heads := func() map[string]string {
		m := map[string]string{}
		for _, l := range strings.Split(git(root, "--git-dir="+dstGit, "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads"), "\n") {
			if k, v, ok := strings.Cut(l, " "); ok {
				m[k] = v
			}
		}
		return m
	}
	srcTip := func(ref string) string { return git(root, "--git-dir="+srcGit, "rev-parse", ref) }
	type pr struct {
		Number int
		Title  string
		State  string
		Body   *string
		Head   struct{ Ref string }
	}
	dstPRs := func() map[int]pr {
		var l []pr
		if err := json.Unmarshal(must(os.ReadFile(filepath.Join(fake, "dst-pulls.json"))), &l); err != nil {
			t.Fatal(err)
		}
		m := map[int]pr{}
		for _, p := range l {
			m[p.Number] = p
		}
		return m
	}
	readLog := func() string { return string(must(os.ReadFile(filepath.Join(fake, "log")))) }
	writes := func() int { return len(regexp.MustCompile(`(?m)^(POST|PATCH) `).FindAllString(readLog(), -1)) }
	checkNative := func(h map[string]string) {
		t.Helper()
		if h["native-feature"] != a || h["dependabot/npm/y"] != a || h["dependabot/go_modules/x"] != dep || h["own-work"] == "" {
			t.Fatalf("gim-home's own branches touched: %v", h)
		}
		ps := dstPRs()
		if ps[1].State != "open" || ps[1].Title != "Bump x" || ps[2].State != "open" || ps[2].Body != nil {
			t.Fatalf("gim-home's own pull requests touched: %+v", ps)
		}
		if l := readLog(); regexp.MustCompile(`PATCH repos/gim-home/longarm/pulls/[12] `).MatchString(l) {
			t.Fatalf("native pull request patched:\n%s", l)
		}
	}

	// 1: everything new there
	r := run()
	if r.code != 0 || !strings.Contains(r.stdout, "fork pull request #4 changes .github/") {
		t.Fatalf("run 1: %+v", r)
	}
	h := heads()
	if h["feat-1"] != srcTip("feat-1") || h["stale"] != srcTip("stale") || h["pr-3"] != fork3 || h["pr-4"] != "" {
		t.Fatalf("branches: %v", h)
	}
	m := h["main"]
	if p := git(root, "--git-dir="+dstGit, "log", "-1", "--format=%P %s", m); p != dstMain0+" "+srcTip("main")+" Sync main from example-owner/longarm" {
		t.Fatalf("main: %s", p)
	}
	git(root, "--git-dir="+dstGit, "cat-file", "-e", m+":.github/acl/access.yml")
	git(root, "--git-dir="+dstGit, "cat-file", "-e", m+":README.md")
	ps := dstPRs()
	if p := ps[3]; p.Head.Ref != "feat-1" || p.Title != "Feature one" || p.Body == nil || *p.Body != "Body 1\n\nMirrored from example-owner/longarm#1" || p.State != "open" {
		t.Fatalf("PR for #1: %+v", p)
	}
	if p := ps[4]; p.Head.Ref != "pr-3" || p.Body == nil || *p.Body != "Mirrored from example-owner/longarm#3" || len(ps) != 4 {
		t.Fatalf("PR for #3: %+v (%d)", p, len(ps))
	}
	checkNative(h)
	if l := readLog(); !strings.Contains(l, "GET repos/example-owner/longarm/pulls?state=open&per_page=100 src-token") ||
		!strings.Contains(l, "POST repos/gim-home/longarm/pulls dst-token") {
		t.Fatalf("tokens:\n%s", l)
	}

	// 2: nothing changed, nothing done
	w := writes()
	if r := run(); r.code != 0 || writes() != w || !strings.Contains(r.stdout, "0 branch(es) pushed, 0 deleted; main up to date; PRs: 0 opened, 0 updated, 0 closed") {
		t.Fatalf("run 2: %+v", r)
	}

	// 3: a new commit on main here and a governance bot's there; #1
	// retitled, #3 closed, stale deleted (squash merge); a branch here named
	// like gim-home's Dependabot branch
	commit("README.md", "two\n", "B")
	git(work, "push", "-q", "src", "main", ":stale")
	git(work, "fetch", "-q", "dst", "main")
	git(work, "checkout", "-q", "-B", "gov", "FETCH_HEAD")
	commit(".github/policies/jit.yml", "jit\n", "governance 2")
	git(work, "push", "-q", "dst", "gov:main")
	dstMain1 := git(work, "rev-parse", "gov")
	git(work, "checkout", "-q", "-b", "srcdep", "main")
	commit("go.mod", "other\n", "src bump")
	git(work, "push", "-q", "src", "srcdep:refs/heads/dependabot/go_modules/x")
	git(work, "checkout", "-q", "main")
	writeJSON("src-pulls.json", "["+srcPR(1, "feat-1", "example-owner/longarm", "Feature one, renamed", "Body 1\r\n")+"]")
	r = run()
	if r.code != 0 || !strings.Contains(r.stdout, "not pushing dependabot/go_modules/x") {
		t.Fatalf("run 3: %+v", r)
	}
	h = heads()
	if _, ok := h["stale"]; ok {
		t.Fatalf("stale not deleted: %v", h)
	}
	if _, ok := h["pr-3"]; ok {
		t.Fatalf("pr-3 not deleted: %v", h)
	}
	if p := git(root, "--git-dir="+dstGit, "log", "-1", "--format=%P", h["main"]); p != dstMain1+" "+srcTip("main") {
		t.Fatalf("main: %s", p)
	}
	git(root, "--git-dir="+dstGit, "merge-base", "--is-ancestor", m, h["main"])
	git(root, "--git-dir="+dstGit, "cat-file", "-e", h["main"]+":.github/policies/jit.yml")
	ps = dstPRs()
	if ps[3].Title != "Feature one, renamed" || ps[3].State != "open" || ps[4].State != "closed" {
		t.Fatalf("PRs: %+v", ps)
	}
	checkNative(h)

	// 4: #3 reopened here: pr-3 pushed again, its PR there reopened
	writeJSON("src-pulls.json", "["+srcPR(1, "feat-1", "example-owner/longarm", "Feature one, renamed", "Body 1")+","+
		srcPR(3, "patch-1", "someone/longarm", "Fork three", "")+"]")
	if r := run(); r.code != 0 || heads()["pr-3"] != fork3 || dstPRs()[4].State != "open" || len(dstPRs()) != 4 {
		t.Fatalf("run 4: %+v %+v", r, dstPRs())
	}
	checkNative(heads())

	// no secret: a notice, nothing else, exit 0
	w = writes()
	if r := run("GIM_MIRROR_TOKEN="); r.code != 0 || !strings.Contains(r.stdout, "::notice title=mirror-gim::secret GIM_MIRROR_TOKEN is not set") || writes() != w {
		t.Fatalf("no secret: %+v", r)
	}
}
