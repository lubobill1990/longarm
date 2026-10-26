package e2e

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestHubDockerfileACRTasks: deploy/hub/Dockerfile builds with ACR Tasks
// (az acr build --target acr, deploy.sh --build-in-acr). Before a build, ACR
// Tasks scans the Dockerfile for its base images (Azure/acr-builder
// scan/dependencies.go): every FROM line up to the --target stage must be
// "FROM <image> [AS <name>]" - a --platform=... flag fails the build ("failed
// to scan dependencies", seen on a real run) - with ${ARG}s that have
// defaults. The default stage stays BuildKit's cross-compiling one.
func TestHubDockerfileACRTasks(t *testing.T) {
	t.Parallel()
	f, err := os.Open("../deploy/hub/Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	args := map[string]string{}
	aliases := map[string]bool{}
	ref := regexp.MustCompile(`^[a-z0-9]+([._/:@-][a-zA-Z0-9_.-]+)*$`)
	reached, crossLast := false, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tok := strings.Fields(line)
		switch strings.ToUpper(tok[0]) {
		case "ARG":
			if k, v, ok := strings.Cut(tok[1], "="); ok {
				if _, set := args[k]; !set {
					args[k] = v
				}
			}
		case "FROM":
			if reached {
				// after the ACR stage: BuildKit only
				crossLast = strings.Contains(line, "--platform=$BUILDPLATFORM")
				if !crossLast && len(tok) == 2 {
					crossLast = aliases[tok[1]] // FROM runtime: the default stage
				}
				continue
			}
			img := os.Expand(tok[1], func(k string) string { return args[k] })
			if !aliases[img] && !ref.MatchString(img) {
				t.Fatalf("ACR Tasks can't parse %q (image %q)", line, img)
			}
			if len(tok) > 2 {
				if len(tok) < 4 || !strings.EqualFold(tok[2], "as") {
					t.Fatalf("ACR Tasks can't parse %q", line)
				}
				aliases[tok[3]] = true
				if tok[3] == "acr" {
					reached = true
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if !reached {
		t.Fatal("no stage acr (az acr build --target acr)")
	}
	b := strings.ReplaceAll(string(must(os.ReadFile("../deploy/hub/Dockerfile"))), "
", "
") // a Windows checkout
	if !strings.Contains(b, "FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build\n") || !crossLast {
		t.Fatal("the default stage must stay the cross-compiling BuildKit one")
	}
}
