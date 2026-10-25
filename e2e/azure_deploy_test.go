package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// deployRunner runs deploy/azure/deploy.sh or deploy.ps1 against the fake az
// (e2e/helper/fakeaz.go).
type deployRunner struct {
	name string
	argv func(args ...string) []string
	flag func(sh, ps string) string // the script's spelling of an option
}

func deployRunners(t *testing.T) []deployRunner {
	script, _ := filepath.Abs("../deploy/azure")
	var rs []deployRunner
	bash := findBash(t)
	rs = append(rs, deployRunner{
		name: "deploy.sh",
		argv: func(args ...string) []string {
			return append([]string{bash, bashPath(filepath.Join(script, "deploy.sh"))}, args...)
		},
		flag: func(sh, _ string) string { return sh },
	})
	shells := []string{"pwsh"}
	if runtime.GOOS == "windows" {
		shells = append(shells, "powershell") // Windows PowerShell 5.1
	}
	for _, sh := range shells {
		p, err := exec.LookPath(sh)
		if err != nil {
			t.Logf("no %s: deploy.ps1 not tested with it", sh)
			continue
		}
		rs = append(rs, deployRunner{
			name: "deploy.ps1/" + sh,
			argv: func(args ...string) []string {
				return append([]string{p, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(script, "deploy.ps1")}, args...)
			},
			flag: func(_, ps string) string { return ps },
		})
	}
	return rs
}

// TestAzureDeployScripts: deploy.sh / deploy.ps1 with a fake az whose
// deployment points at a real hub started like the image starts it. The
// printed `longarm join` command must work as printed; re-runs upgrade,
// secrets never reach az's command line, teardown only deletes what the
// script created.
func TestAzureDeployScripts(t *testing.T) {
	t.Parallel()
	hubBin := buildHub(t)
	c := newContainerHub(t)
	_, out, done := c.start(hubBin)
	c.waitHealthy(hubBin, out, done)
	b := bootstrapLine(t, out.String())
	e := &env{t: t, root: c.root}

	for i, r := range deployRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			dir := t.TempDir()
			az := filepath.Join(dir, exeName("az"))
			copyBin(t, bins.helper, az)
			azPath := az
			if r.name == "deploy.sh" {
				azPath = bashPath(az)
			}
			run := func(extraEnv []string, args ...string) result {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				argv := r.argv(args...)
				cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
				cmd.Env = append(append(os.Environ(),
					"LONGARM_DEPLOY_AZ="+azPath,
					"HELPER_AZ_DIR="+dir,
					"HELPER_AZ_HUB="+b["hub_address"],
					"HELPER_AZ_BOOTSTRAP="+filepath.Join(c.share, "bootstrap.env"),
					"LONGARM_REGISTRY_PASSWORD=",
				), extraEnv...)
				var so, se bytes.Buffer
				cmd.Stdout, cmd.Stderr = &so, &se
				err := cmd.Run()
				code := 0
				if ee, ok := err.(*exec.ExitError); ok {
					code = ee.ExitCode()
				} else if err != nil {
					t.Fatal(err)
				}
				return result{so.String(), se.String(), code}
			}
			calls := func() string { return string(must(os.ReadFile(filepath.Join(dir, "calls.log")))) }
			name, loc, noPanel := r.flag("--name", "-Name"), r.flag("--location", "-Location"), r.flag("--no-panel", "-NoPanel")

			if res := run([]string{"HELPER_AZ_LOGGED_OUT=1"}, name, "myhub", loc, "eastasia"); res.code == 0 || !strings.Contains(res.stderr+res.stdout, "az login") {
				t.Fatalf("logged out: %+v", res)
			}
			if res := run(nil, name, "My_Hub", loc, "eastasia"); res.code == 0 {
				t.Fatalf("bad name accepted: %+v", res)
			}

			res := run([]string{"LONGARM_REGISTRY_PASSWORD=s3cret-pw"}, name, "myhub", loc, "eastasia", noPanel,
				r.flag("--registry-user", "-RegistryUser"), "bot", r.flag("--timeout", "-TimeoutSeconds"), "60")
			if res.code != 0 {
				t.Fatalf("deploy: %+v\ncalls:\n%s", res, calls())
			}
			joinRE := regexp.MustCompile(`longarm join (\S+) (\S+) --ca-fingerprint (\S+)`)
			m := joinRE.FindStringSubmatch(res.stdout)
			if m == nil || m[1] != b["hub_address"] || m[3] != b["ca_fingerprint"] {
				t.Fatalf("no join command for %v in:\n%s", b, res.stdout)
			}
			for _, want := range []string{"longarmd join " + b["hub_address"] + " <worker-code> --ca-fingerprint " + b["ca_fingerprint"],
				"claude mcp add longarm -- longarm mcp", "USD/month", "panel           off"} {
				if !strings.Contains(res.stdout, want) {
					t.Errorf("output lacks %q:\n%s", want, res.stdout)
				}
			}
			if i == 0 {
				// the command works as printed (the code is one-time: the
				// other scripts only print the then used code)
				laptop := filepath.Join(c.root, "laptop")
				if j := e.longarm(laptop, "join", m[1], m[2], "--ca-fingerprint", m[3], "--name", "laptop"); j.code != 0 {
					t.Fatalf("printed join: %+v", j)
				}
			}
			var params struct {
				Parameters map[string]struct{ Value any } `json:"parameters"`
			}
			if err := json.Unmarshal(must(os.ReadFile(filepath.Join(dir, "params.json"))), &params); err != nil {
				t.Fatal(err)
			}
			p := params.Parameters
			if p["name"].Value != "myhub" || p["location"].Value != "eastasia" || p["size"].Value != "small" ||
				p["enablePanel"].Value != false || p["registryPassword"].Value != "s3cret-pw" {
				t.Fatalf("parameters %+v", p)
			}
			cl := calls()
			if strings.Contains(cl, "s3cret-pw") {
				t.Fatal("secret on az's command line")
			}
			if !strings.Contains(cl, "group create -n myhub-rg -l eastasia --tags longarm-hub=myhub") {
				t.Fatalf("calls:\n%s", cl)
			}

			// upgrade: same mutable tag twice -> pulled again (restart); a
			// pinned tag change is the template's update, no restart
			const edge = "ghcr.io/example/longarm-hub:edge"
			for range 2 {
				if res := run(nil, name, "myhub", r.flag("--image", "-Image"), edge, noPanel); res.code != 0 {
					t.Fatalf("upgrade: %+v", res)
				}
			}
			if n := strings.Count(calls(), "container restart"); n != 1 {
				t.Fatalf("%d restarts:\n%s", n, calls())
			}
			if res := run(nil, name, "myhub", r.flag("--image", "-Image"), "ghcr.io/example/longarm-hub:1.2.3", noPanel); res.code != 0 ||
				strings.Count(calls(), "container restart") != 1 {
				t.Fatalf("pinned upgrade: %+v", res)
			}

			// teardown: only a group created for this hub
			yes := r.flag("--yes", "-Yes")
			if res := run(nil, name, "otherhub", r.flag("--resource-group", "-ResourceGroup"), "myhub-rg", r.flag("--teardown", "-Teardown"), yes); res.code == 0 ||
				strings.Contains(calls(), "group delete") {
				t.Fatalf("foreign teardown: %+v", res)
			}
			if res := run(nil, name, "myhub", r.flag("--teardown", "-Teardown"), yes); res.code != 0 || !strings.Contains(calls(), "group delete -n myhub-rg --yes") {
				t.Fatalf("teardown: %+v\n%s", res, calls())
			}
		})
	}
}
