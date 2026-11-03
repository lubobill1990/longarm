package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/workspace"
)

func TestTempBranchName(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 5, 0, 0, time.UTC)
	for user, want := range map[string]string{
		"alice":            "longarm/handoff/alice/5e55a7c1-202610280905-0a1b2c",
		"Bob.Smith@corp":   "longarm/handoff/bob.smith-corp/5e55a7c1-202610280905-0a1b2c",
		"../..":            "longarm/handoff/user/5e55a7c1-202610280905-0a1b2c",
		"a..b":             "longarm/handoff/a.b/5e55a7c1-202610280905-0a1b2c",
		"x y/z\\w:q*?[~^]": "longarm/handoff/x-y-z-w-q/5e55a7c1-202610280905-0a1b2c",
	} {
		got := tempBranchName(user, "5e55a7c1", at, "0a1b2c")
		if got != want {
			t.Errorf("tempBranchName(%q) = %q, want %q", user, got, want)
		}
		if !workspace.HandoffBranchRE.MatchString(got) {
			t.Errorf("%q is outside what a worker pushes", got)
		}
		if out, err := exec.Command("git", "check-ref-format", "refs/heads/"+got).CombinedOutput(); err != nil {
			t.Errorf("%q is no valid ref: %s", got, out)
		}
	}
}

// The per-user namespaces a repository may limit branches to (Azure DevOps
// at Microsoft) first, longarm/handoff/ last; all of them pushable by a
// worker; a refusal for the name tries the next.
func TestTempBranchNames(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 5, 0, 0, time.UTC)
	leaf := "5e55a7c1-202610280905-0a1b2c"
	got := tempBranchNames("Alice", "5e55a7c1", at, "0a1b2c")
	want := []string{"users/alice/longarm-handoff/" + leaf, "user/alice/longarm-handoff/" + leaf, "longarm/handoff/alice/" + leaf}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("names %v, want %v", got, want)
	}
	for _, b := range got {
		if !workspace.HandoffBranchRE.MatchString(b) {
			t.Errorf("%q is outside what a worker pushes", b)
		}
	}
	for _, bad := range []string{"users/alice/other/x", "user/alice/x", "main", "users/alice/longarm-handoff/../x"} {
		if workspace.HandoffBranchRE.MatchString(bad) && !strings.Contains(bad, "..") {
			t.Errorf("a worker would push %q", bad)
		}
	}
	for msg, name := range map[string]bool{
		"! [remote rejected] (VS403660: You do not have permission to create ref 'refs/heads/longarm/handoff/a/b'. Please ensure the ref naming convention being used is allowed in this repository.)": true,
		"remote: error: GH013: Repository rule violations found for refs/heads/longarm/handoff/a/b.":                                                                                                   true,
		`bad branch "users/a/longarm-handoff/x" (want longarm/handoff/<user>/<id>)`:                                                                                                                    true,
		"fatal: Authentication failed for 'https://example.com/r.git/'":                                                                                                                                false,
		"! [rejected] longarm/handoff/a/b (non-fast-forward)":                                                                                                                                          false,
	} {
		if got := nameRefusedRE.MatchString(msg); got != name {
			t.Errorf("refused for the name %v, want %v: %s", got, name, msg)
		}
	}
}

func TestResolveVia(t *testing.T) {
	dir := t.TempDir()
	old := configDir
	configDir = dir
	defer func() { configDir = old }()
	o := handoffOpts{via: defaultHandoffVia}
	if err := resolveVia(&o, false, defaultViaThreshold, false); err != nil || o.via != "auto" || o.viaThreshold != 20<<20 {
		t.Fatalf("defaults: %v %+v", err, o)
	}
	os.WriteFile(filepath.Join(dir, "workers.toml"), []byte("[handoff]\nvia = \"relay\"\nvia_threshold = \"1MiB\"\n"), 0o644)
	o = handoffOpts{via: defaultHandoffVia}
	if err := resolveVia(&o, false, defaultViaThreshold, false); err != nil || o.via != "relay" || o.viaThreshold != 1<<20 {
		t.Fatalf("config: %v %+v", err, o)
	}
	o = handoffOpts{via: "remote"}
	if err := resolveVia(&o, true, "5M", true); err != nil || o.via != "remote" || o.viaThreshold != 5<<20 {
		t.Fatalf("flags win: %v %+v", err, o)
	}
	o = handoffOpts{via: "carrier-pigeon"}
	if err := resolveVia(&o, true, "5M", true); !isUsage(err) {
		t.Fatalf("bad --via: %v", err)
	}
}

// TestScanSnapshot: the secret check of --via remote looks at the changed
// and untracked files of the snapshot and names file and rule only.
func TestScanSnapshot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "core.autocrlf", "false")
	writeFileT(t, filepath.Join(dir, "committed.txt"), "AKIA"+strings.Repeat("ABCD", 4)+" is old and committed\n") // built: no scanner flags the fixture
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-qm", "c1")
	ep := localEP(t)
	r, err := openHoRepo(ctx, ep, dir)
	if err != nil {
		t.Fatal(err)
	}
	clean := func() {
		snap, base, err := r.snapshot(ctx, "refs/longarm/handoff/t"+time.Now().Format("150405.000000"))
		if err != nil {
			t.Fatal(err)
		}
		hits, big, err := scanSnapshot(ctx, r, base, snap, 100)
		if err != nil || len(hits) != 0 || len(big) != 0 {
			t.Fatalf("clean snapshot: %v %v %v", hits, big, err)
		}
	}
	writeFileT(t, filepath.Join(dir, "notes.md"), "API_KEY=<your key here>\npassword: changeme\nnothing secret\n")
	clean() // placeholders and the committed file don't count

	writeFileT(t, filepath.Join(dir, "deploy", "app.env"), "export DB_PASSWORD=Hunter2Hunter2-real\n")
	writeFileT(t, filepath.Join(dir, "id_ed25519"), "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n")
	writeFileT(t, filepath.Join(dir, "src", "client.go"), `const tok = "ghp_`+strings.Repeat("a1B2", 9)+`"`+"\n")
	writeFileT(t, filepath.Join(dir, "big.bin"), strings.Repeat("x", 200))
	snap, base, err := r.snapshot(ctx, "refs/longarm/handoff/t2")
	if err != nil {
		t.Fatal(err)
	}
	hits, big, err := scanSnapshot(ctx, r, base, snap, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := describeHits(hits)
	for _, want := range []string{"deploy/app.env (secret assignment)", "id_ed25519 (private key)", "id_ed25519 (credential file name)", "src/client.go (GitHub token)"} {
		if !strings.Contains(got, want) {
			t.Errorf("hits lack %q: %s", want, got)
		}
	}
	if strings.Contains(got, "Hunter2") || strings.Contains(got, "a1B2") || strings.Contains(got, "AAAA") {
		t.Errorf("a secret value in the report: %s", got)
	}
	if len(big) != 1 || big[0] != "big.bin" {
		t.Errorf("files over the limit: %v", big)
	}
}
