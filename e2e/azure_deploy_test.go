package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
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

// azOnPath in run's extra environment: az is found on PATH (no LONGARM_DEPLOY_AZ)
const azOnPath = "test:az-on-path"

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
// script created. Like on a real laptop: az found on PATH (no
// LONGARM_DEPLOY_AZ: deploy.sh's az() once called itself forever), a new
// subscription (providers not registered), a global
// AZURE_STORAGE_CONNECTION_STRING / SAS for another account; images from
// ACR (the login must be for that registry), --build-in-acr, a container
// group stuck in Creating, a failed deployment.
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
			t.Parallel() // own fake az state each; the hub is shared read-only
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
				var base []string
				for _, kv := range os.Environ() {
					if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, "LONGARM_DEPLOY_AZ") && !strings.HasPrefix(strings.ToUpper(k), "AZURE_STORAGE_") {
						base = append(base, kv)
					}
				}
				if slices.Contains(extraEnv, azOnPath) {
					// az as the user has it: found on PATH
					for i, kv := range base {
						if k, v, _ := strings.Cut(kv, "="); strings.EqualFold(k, "PATH") {
							base[i] = k + "=" + dir + string(os.PathListSeparator) + v
						}
					}
				} else {
					base = append(base, "LONGARM_DEPLOY_AZ="+azPath)
				}
				cmd.Env = append(append(base,
					"HELPER_AZ_DIR="+dir,
					"HELPER_AZ_HUB="+b["hub_address"],
					"HELPER_AZ_BOOTSTRAP="+filepath.Join(c.share, "bootstrap.env"),
					"LONGARM_REGISTRY_PASSWORD=",
					"LONGARM_DEPLOY_POLL_SECONDS=1",
					// the caller's storage settings for another account: the
					// scripts must use the hub's key (fake az: ShareNotFound)
					"AZURE_STORAGE_CONNECTION_STRING=DefaultEndpointsProtocol=https;AccountName=other;AccountKey=b3RoZXI=",
					"AZURE_STORAGE_SAS_TOKEN=sv=2024-01-01&sig=other",
					"AZURE_STORAGE_ACCOUNT=other",
				), slices.DeleteFunc(slices.Clone(extraEnv), func(kv string) bool { return kv == azOnPath })...)
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

			pw := []string{"LONGARM_REGISTRY_PASSWORD=s3cret-pw"}
			regUser, size, image := r.flag("--registry-user", "-RegistryUser"), r.flag("--size", "-Size"), r.flag("--image", "-Image")
			recreate := r.flag("--recreate", "-Recreate")
			params := func() map[string]any {
				t.Helper()
				var doc struct {
					Parameters map[string]struct{ Value any } `json:"parameters"`
				}
				if err := json.Unmarshal(must(os.ReadFile(filepath.Join(dir, "params.json"))), &doc); err != nil {
					t.Fatal(err)
				}
				m := map[string]any{}
				for k, v := range doc.Parameters {
					m[k] = v.Value
				}
				return m
			}
			ok := func(res result, what string) {
				t.Helper()
				if res.code != 0 {
					t.Fatalf("%s: %+v\ncalls:\n%s", what, res, calls())
				}
			}
			refused := func(res result, what, why string) {
				t.Helper()
				if res.code == 0 || !strings.Contains(strings.ToLower(res.stderr+res.stdout), why) {
					t.Fatalf("%s: not refused with %q: %+v", what, why, res)
				}
			}
			restarts := func() int { return strings.Count(calls(), "container restart") }

			// the first run: az on PATH, a new subscription
			res := run(append([]string{azOnPath, "HELPER_AZ_UNREGISTERED=Microsoft.ContainerInstance,Microsoft.Storage"}, pw...),
				name, "myhub", loc, "East Asia", noPanel, regUser, "bot", r.flag("--timeout", "-TimeoutSeconds"), "60")
			ok(res, "deploy")
			for _, ns := range []string{"Microsoft.ContainerInstance", "Microsoft.Storage"} {
				if !strings.Contains(calls(), "provider register -n "+ns) || !strings.Contains(res.stdout, "registering the resource provider "+ns) {
					t.Fatalf("%s not registered:\n%s\ncalls:\n%s", ns, res.stdout, calls())
				}
			}
			if strings.Contains(calls(), "Microsoft.OperationalInsights") || strings.Contains(calls(), "Microsoft.ContainerRegistry") {
				t.Fatalf("providers that this deployment doesn't need:\n%s", calls())
			}
			joinRE := regexp.MustCompile(`longarm join (\S+) (\S+) --ca-fingerprint (\S+)`)
			m := joinRE.FindStringSubmatch(res.stdout)
			if m == nil || m[1] != b["hub_address"] || m[3] != b["ca_fingerprint"] {
				t.Fatalf("no join command for %v in:\n%s", b, res.stdout)
			}
			for _, want := range []string{"longarmd join " + b["hub_address"] + " <worker-code> --ca-fingerprint " + b["ca_fingerprint"],
				"claude mcp add longarm -- longarm mcp", "USD/month", "panel           off", "longnet://127.0.0.1:7445?tcp=127.0.0.1:" + b["hub_address"][strings.LastIndex(b["hub_address"], ":")+1:] + "&name=worker-1"} {
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
			// the location is the resource group's, not a template parameter
			// ("East Asia" would make a broken DNS name)
			p := params()
			// the template's image (ghcr.io): the template's registryServer
			if _, has := p["location"]; has || p["name"] != "myhub" || p["size"] != "small" || p["enablePanel"] != false || p["registryPassword"] != "s3cret-pw" || p["registryServer"] != nil ||
				p["storageAuth"] != "key" || p["enableTelemetry"] != false || p["enableDiagnostics"] != false || p["acrName"] != nil || p["entraTenantId"] != nil {
				t.Fatalf("parameters %+v", p)
			}
			cl := calls()
			if strings.Contains(cl, "s3cret-pw") || strings.Contains(cl, "dGVzdGtleQ==") {
				t.Fatal("a secret (registry password, storage key) on az's command line:\n" + cl)
			}
			if !strings.Contains(cl, "group create -n myhub-rg -l East Asia --tags longarm-hub=myhub") {
				t.Fatalf("calls:\n%s", cl)
			}

			// a re-run keeps the running hub's settings; what can't be read
			// back (the registry password) must be given again
			refused(run(nil, name, "myhub"), "re-run without the registry login", "registry")
			ok(run(pw, name, "myhub", regUser, "bot"), "re-run")
			// the fake prints None for null fields like the real az (no Log
			// Analytics workspace): not a workspace
			if p := params(); p["enablePanel"] != false || p["size"] != "small" || p["image"] != "ghcr.io/example/longarm-hub:latest" ||
				p["enableLogAnalytics"] != false || p["storageAuth"] != "key" || p["enableTelemetry"] != false {
				t.Fatalf("re-run lost settings: %+v", p)
			}
			if strings.Contains(calls(), "Microsoft.OperationalInsights") {
				t.Fatalf("None taken for a Log Analytics workspace:\n%s", calls())
			}
			n := restarts() // same mutable tag: pulled again
			if n != 1 {
				t.Fatalf("%d restarts:\n%s", n, calls())
			}
			// resizing needs --recreate (ACI can't resize a group)
			refused(run(pw, name, "myhub", regUser, "bot", size, "medium"), "resize", "recreate")
			ok(run(pw, name, "myhub", regUser, "bot", size, "medium", recreate), "resize with recreate")
			if p := params(); p["size"] != "medium" || p["enablePanel"] != false || !strings.Contains(calls(), "container delete -g myhub-rg -n myhub --yes") {
				t.Fatalf("recreate: %+v\n%s", p, calls())
			}

			// upgrades: a new tag is the template's update; the same mutable
			// tag again is pulled again (restart); a pinned one is not
			const edge = "ghcr.io/example/longarm-hub:edge"
			ok(run(pw, name, "myhub", regUser, "bot", image, edge), "upgrade to edge")
			ok(run(pw, name, "myhub", regUser, "bot"), "re-run on edge")
			if restarts() != n+1 || params()["image"] != edge {
				t.Fatalf("edge re-run: %d restarts, %+v", restarts(), params())
			}
			ok(run(pw, name, "myhub", regUser, "bot", image, "ghcr.io/example/longarm-hub:1.2.3"), "pinned upgrade")
			ok(run(pw, name, "myhub", regUser, "bot"), "re-run on 1.2.3")
			if restarts() != n+1 || params()["image"] != "ghcr.io/example/longarm-hub:1.2.3" {
				t.Fatalf("pinned re-run: %d restarts, %+v", restarts(), params())
			}

			// the external STUN servers the hub advertises (template default
			// unless given; "none" = only the hub's own ports)
			stun := r.flag("--stun-servers", "-StunServers")
			ok(run(pw, name, "myhub", regUser, "bot", stun, "stun.example.net:3478,[2001:db8::1]:3478"), "STUN servers")
			if p := params(); p["stunServers"] != "stun.example.net:3478,[2001:db8::1]:3478" {
				t.Fatalf("STUN servers: %+v", p)
			}
			ok(run(pw, name, "myhub", regUser, "bot", stun, "none"), "no STUN servers")
			if p := params(); p["stunServers"] != "" {
				t.Fatalf("no STUN servers: %+v", p)
			}
			ok(run(pw, name, "myhub", regUser, "bot"), "re-run keeps the STUN servers")
			if p := params(); p["stunServers"] != "" {
				t.Fatalf("re-run lost --stun-servers none: %+v", p)
			}
			refused(run(pw, name, "myhub", regUser, "bot", stun, "a b"), "bad STUN servers", "stun")

			// telemetry and Entra sign-in: template parameters a re-run keeps
			// (from the hub's environment); diagnostics and --acr need the
			// managed identity
			telemetry, diagnostics := r.flag("--telemetry", "-Telemetry"), r.flag("--diagnostics", "-Diagnostics")
			entraTenant, entraClient := r.flag("--entra-tenant", "-EntraTenant"), r.flag("--entra-client", "-EntraClient")
			entraAdmins, entraWeb := r.flag("--entra-admins", "-EntraAdmins"), r.flag("--entra-web", "-EntraWeb")
			entraApps := r.flag("--entra-apps", "-EntraApps")
			acrFlag := r.flag("--acr", "-Acr")
			refused(run(pw, name, "myhub", regUser, "bot", diagnostics), "diagnostics with storage keys", "diagnostics needs")
			refused(run(pw, name, "myhub", regUser, "bot", acrFlag, "myacr"), "--acr with storage keys", "acr needs")
			refused(run(pw, name, "myhub", regUser, "bot", entraTenant, "tid"), "Entra tenant without client", "go together")
			refused(run(pw, name, "myhub", regUser, "bot", entraTenant, "tid", entraClient, "cid", entraWeb, "bogus"), "bad Entra web mode", "spa, confidential or off")
			ok(run(pw, name, "myhub", regUser, "bot", telemetry, entraTenant, "tid", entraClient, "cid", entraAdmins, "alice@example.com,*@example.org",
				entraApps, "11111111-2222-3333-4444-555555555555=ci-bot"), "telemetry and Entra")
			if p := params(); p["enableTelemetry"] != true || p["enableDiagnostics"] != false || p["storageAuth"] != "key" ||
				p["entraTenantId"] != "tid" || p["entraClientId"] != "cid" || p["entraAdmins"] != "alice@example.com,*@example.org" || p["entraWeb"] != nil ||
				p["entraApps"] != "11111111-2222-3333-4444-555555555555=ci-bot" {
				t.Fatalf("telemetry and Entra: %+v", p)
			}
			ok(run(pw, name, "myhub", regUser, "bot", entraWeb, "off"), "Entra web mode")
			ok(run(pw, name, "myhub", regUser, "bot"), "re-run keeps telemetry and Entra")
			if p := params(); p["enableTelemetry"] != true || p["entraTenantId"] != "tid" || p["entraClientId"] != "cid" ||
				p["entraAdmins"] != "alice@example.com,*@example.org" || p["entraWeb"] != "off" ||
				p["entraApps"] != "11111111-2222-3333-4444-555555555555=ci-bot" {
				t.Fatalf("re-run lost telemetry / Entra: %+v", p)
			}
			ok(run(pw, name, "myhub", regUser, "bot", entraAdmins, "bob@example.com"), "other Entra admins")
			if p := params(); p["entraTenantId"] != "tid" || p["entraClientId"] != "cid" || p["entraAdmins"] != "bob@example.com" || p["entraWeb"] != "off" {
				t.Fatalf("other Entra admins: %+v", p)
			}

			// an image from another registry: the login is for that registry,
			// and needed again on a re-run
			const acrImage = "example.azurecr.io/longarm-hub:1.2.3"
			ok(run(pw, name, "myhub", regUser, "bot", image, acrImage), "ACR image")
			if p := params(); p["image"] != acrImage || p["registryServer"] != "example.azurecr.io" || p["registryUsername"] != "bot" {
				t.Fatalf("ACR image: %+v", p)
			}
			refused(run(nil, name, "myhub"), "re-run without the ACR login", "example.azurecr.io")
			if res := run(pw, name, "myhub", r.flag("--build-in-acr", "-BuildInAcr"), image, acrImage); res.code == 0 {
				t.Fatalf("--build-in-acr with --image: %+v", res)
			}

			// --build-in-acr: a registry in the resource group, az acr build of
			// the ACR stage from this checkout, deployed with its admin login
			res = run(nil, name, "myhub", r.flag("--build-in-acr", "-BuildInAcr"))
			ok(res, "build in ACR")
			acr := strings.TrimSpace(string(must(os.ReadFile(filepath.Join(dir, "acr")))))
			p = params()
			if img, _ := p["image"].(string); !strings.HasPrefix(img, acr+".azurecr.io/longarm-hub:build-") ||
				p["registryServer"] != acr+".azurecr.io" || p["registryUsername"] != "acruser" || p["registryPassword"] != "acr-s3cret-pw" {
				t.Fatalf("build in ACR: %+v\n%s", p, res.stdout)
			}
			if b := string(must(os.ReadFile(filepath.Join(dir, "acr-build")))); !strings.Contains(b, "--target acr") || !strings.Contains(b, "--platform linux/amd64") {
				t.Fatalf("acr build: %s", b)
			}
			if cl := calls(); strings.Contains(cl, "acr-s3cret-pw") || !strings.Contains(cl, "provider show -n Microsoft.ContainerRegistry") {
				t.Fatalf("calls:\n%s", cl)
			}
			builtImage := p["image"]
			// a re-run finds the registry's login by itself, a second build
			// reuses the registry
			ok(run(nil, name, "myhub"), "re-run on the ACR build")
			if p := params(); p["image"] != builtImage || p["registryUsername"] != "acruser" {
				t.Fatalf("re-run on the ACR build: %+v", p)
			}
			ok(run(nil, name, "myhub", r.flag("--build-in-acr", "-BuildInAcr")), "second build in ACR")
			if n := strings.Count(calls(), "acr create"); n != 1 {
				t.Fatalf("%d registries created", n)
			}

			// a container group stuck in Creating without events: an
			// actionable message instead of a long timeout
			stuckTimeout := r.flag("--stuck-timeout", "-StuckTimeoutSeconds")
			res = run([]string{"HELPER_AZ_STUCK=1"}, name, "myhub", stuckTimeout, "2", r.flag("--timeout", "-TimeoutSeconds"), "60")
			refused(res, "stuck group", "recreate")
			if !strings.Contains(res.stderr+res.stdout, "no events") {
				t.Fatalf("stuck group: %+v", res)
			}
			// a failed deployment shows the failed operations
			refused(run([]string{"HELPER_AZ_DEPLOY_FAIL=1"}, name, "myhub"), "failed deployment", "inaccessibleimage")

			// a new admin code: az container exec of the argument-free alias
			if res := run(nil, name, "myhub", r.flag("--admin-code", "-AdminCode")); res.code != 0 || !strings.Contains(res.stdout, "ABC-DEF") {
				t.Fatalf("admin code: %+v", res)
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

			// --storage-auth identity: no storage keys; the bootstrap values
			// come from the hub's log (its last LONGARM_BOOTSTRAP line), the
			// admin code from --admin-code, the panel from the hub (no Caddy)
			sa := r.flag("--storage-auth", "-StorageAuth")
			from := len(calls())
			res = run(nil, name, "idhub", loc, "eastasia", sa, "identity", telemetry, diagnostics,
				entraTenant, "tid", entraClient, "cid", entraAdmins, "alice@example.com", entraWeb, "confidential")
			ok(res, "identity")
			if p := params(); p["storageAuth"] != "identity" || p["enablePanel"] != true || p["enableTelemetry"] != true || p["enableDiagnostics"] != true ||
				p["entraTenantId"] != "tid" || p["entraClientId"] != "cid" || p["entraAdmins"] != "alice@example.com" || p["entraWeb"] != "confidential" ||
				p["acrName"] != nil || p["registryUsername"] != nil || p["enableLogAnalytics"] != false {
				t.Fatalf("identity: %+v", p)
			}
			if cl := calls()[from:]; strings.Contains(cl, "storage file") || strings.Contains(cl, "keys list") || !strings.Contains(cl, "container logs -g idhub-rg -n idhub --container-name hub") {
				t.Fatalf("identity: storage keys or no log:\n%s", cl)
			}
			for _, want := range []string{r.flag("--admin-code", "-AdminCode") + "      # a one-time admin join code",
				"longarm join " + b["hub_address"] + " <code> --ca-fingerprint " + b["ca_fingerprint"],
				"storage auth identity", "about 35 USD/month"} {
				if !strings.Contains(res.stdout, want) {
					t.Errorf("identity: output lacks %q:\n%s", want, res.stdout)
				}
			}
			// a re-run finds the mode and keeps everything; it can't switch
			from = len(calls())
			ok(run(nil, name, "idhub"), "identity re-run")
			if p := params(); p["storageAuth"] != "identity" || p["enablePanel"] != true || p["enableTelemetry"] != true || p["enableDiagnostics"] != true ||
				p["entraTenantId"] != "tid" || p["entraAdmins"] != "alice@example.com" || p["entraWeb"] != "confidential" || p["enableLogAnalytics"] != false {
				t.Fatalf("identity re-run: %+v", p)
			}
			// telemetry looks at its providers (already registered); Log
			// Analytics stays off (enableLogAnalytics above)
			if cl := calls()[from:]; strings.Contains(cl, "storage ") || strings.Contains(cl, "provider register") ||
				!strings.Contains(cl, "provider show -n Microsoft.Insights") {
				t.Fatalf("identity re-run:\n%s", cl)
			}
			refused(run(nil, name, "idhub", sa, "key"), "switching the storage auth", "switching needs")
			// the image built in ACR: pulled with the identity (acrName, no
			// admin user); a re-run finds the registry from the group's
			res = run(nil, name, "idhub", r.flag("--build-in-acr", "-BuildInAcr"))
			ok(res, "identity: build in ACR")
			acr = strings.TrimSpace(string(must(os.ReadFile(filepath.Join(dir, "acr")))))
			if p := params(); p["acrName"] != acr || p["registryUsername"] != nil || !strings.HasPrefix(fmt.Sprint(p["image"]), acr+".azurecr.io/longarm-hub:build-") {
				t.Fatalf("identity: build in ACR: %+v", p)
			}
			if !strings.Contains(calls(), "acr create -g idhub-rg -n "+acr+" -l eastasia --sku Basic --admin-enabled false") {
				t.Fatalf("identity: ACR with an admin user:\n%s", calls())
			}
			ok(run(nil, name, "idhub", image, "ghcr.io/example/longarm-hub:1.2.3"), "identity re-run after the ACR build")
			if p := params(); p["acrName"] != acr || p["registryUsername"] != nil {
				t.Fatalf("identity re-run lost the registry: %+v", p)
			}
			ok(run(nil, name, "idhub", acrFlag, "other"), "identity with --acr")
			if p := params(); p["acrName"] != "other" {
				t.Fatalf("identity --acr: %+v", p)
			}
			if res := run(nil, name, "idhub", r.flag("--teardown", "-Teardown"), yes); res.code != 0 || !strings.Contains(calls(), "group delete -n idhub-rg --yes") {
				t.Fatalf("identity teardown: %+v", res)
			}
		})
	}
}
