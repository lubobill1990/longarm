package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// fakeAz is a stand-in for the Azure CLI (helper copied to az / az.exe) for
// deploy/azure/deploy.sh and deploy.ps1: just the calls they make, with the
// state in $HELPER_AZ_DIR (resource group, the container group as the last
// deployment's parameters, a restart counter, a log of every call). The
// deployment "outputs" point at a real hub ($HELPER_AZ_HUB) whose share has
// bootstrap.env ($HELPER_AZ_BOOTSTRAP); its boot_id is replaced by the
// restart counter, so the scripts see each "restart".
func fakeAz(args []string) {
	dir := os.Getenv("HELPER_AZ_DIR")
	if f, err := os.OpenFile(filepath.Join(dir, "calls.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
	}
	flag := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	exists := func(f string) bool { _, err := os.Stat(filepath.Join(dir, f)); return err == nil }
	read := func(f string) string { b, _ := os.ReadFile(filepath.Join(dir, f)); return strings.TrimSpace(string(b)) }
	write := func(f, s string) { os.WriteFile(filepath.Join(dir, f), []byte(s), 0o644) }
	fail := func(msg string) { fmt.Fprintln(os.Stderr, "ERROR:", msg); os.Exit(3) }
	// HELPER_AZ_RESTORE_SECONDS: every start restores the database that long
	// (Litestream replaying a long WAL): the log shows the restore running and
	// no bootstrap values of this start before it is done
	restart := func() {
		n, _ := strconv.Atoi(read("boots"))
		write("boots", strconv.Itoa(n+1))
		os.Remove(filepath.Join(dir, "crashes"))
		if secs, _ := strconv.Atoi(os.Getenv("HELPER_AZ_RESTORE_SECONDS")); secs > 0 {
			write("restore-until", strconv.FormatInt(time.Now().Add(time.Duration(secs)*time.Second).UnixMilli(), 10))
		} else {
			os.Remove(filepath.Join(dir, "restore-until"))
		}
	}
	restoring := func() bool {
		ms, _ := strconv.ParseInt(read("restore-until"), 10, 64)
		return time.Now().UnixMilli() < ms
	}
	// the container group = the parameters it was deployed with
	group := func() map[string]any {
		var doc struct {
			Parameters map[string]struct{ Value any } `json:"parameters"`
		}
		json.Unmarshal([]byte(read("cg")), &doc)
		m := map[string]any{"image": "ghcr.io/example/longarm-hub:latest", "size": "small", "enablePanel": true, "storageAuth": "key", "entraWeb": "spa"}
		for k, v := range doc.Parameters {
			m[k] = v.Value
		}
		return m
	}
	// the answer to a query that is one flat list: one tab-separated line,
	// or with HELPER_AZ_TSV_LINES=1 one field per line, as az 2.91 prints it
	// (deploy.ps1 joined those lines without a separator and read an
	// identity hub as storage auth key)
	tsv := func(vals ...string) {
		if os.Getenv("HELPER_AZ_TSV_LINES") == "1" {
			fmt.Println(strings.Join(vals, "\n"))
			return
		}
		fmt.Println(strings.Join(vals, "\t"))
	}
	// one row of a list of lists ([name, value] per element): always one
	// tab-separated line
	row := func(vals ...string) { fmt.Println(strings.Join(vals, "\t")) }
	// the real az prints None for a null field of a tsv list
	orNone := func(s string) string {
		if s == "" {
			return "None"
		}
		return s
	}
	// the hub's bootstrap values on the share (boot_id = the restart counter)
	bootstrap := func() []byte {
		b, err := os.ReadFile(os.Getenv("HELPER_AZ_BOOTSTRAP"))
		if err != nil {
			fail("ShareNotFound")
		}
		return regexp.MustCompile(`(?m)^boot_id=.*$`).ReplaceAll(b, []byte("boot_id=fake-"+read("boots")))
	}
	identity := func() bool { return group()["storageAuth"] == "identity" }
	// HELPER_AZ_CRASHLOOP_IMAGE: a group whose image contains it crash-loops
	// (the 0.1.18 deploy: Litestream's restore ran out of memory on every
	// start, exit 137): ACI counts two restarts between two looks
	crashLoop := func() bool {
		c := os.Getenv("HELPER_AZ_CRASHLOOP_IMAGE")
		return c != "" && exists("cg") && strings.Contains(fmt.Sprint(group()["image"]), c)
	}
	cmd := strings.Join(args[:min(len(args), 2)], " ")
	q := flag("--query")
	// the external relay (deploy.sh --relay): its own container group
	// <name>-relay = the parameters of its last deployment ("cg-relay")
	if strings.HasSuffix(flag("-n"), "-relay") && strings.HasPrefix(cmd, "container ") {
		relayParams := func() map[string]any {
			var doc struct {
				Parameters map[string]struct{ Value any } `json:"parameters"`
			}
			json.Unmarshal([]byte(read("cg-relay")), &doc)
			m := map[string]any{}
			for k, v := range doc.Parameters {
				m[k] = v.Value
			}
			return m
		}
		if cmd != "container delete" && !exists("cg-relay") {
			fail("ResourceNotFound")
		}
		switch {
		case cmd == "container delete":
			os.Remove(filepath.Join(dir, "cg-relay"))
		case cmd == "container show" && strings.Contains(q, "currentState"):
			fmt.Println("Running")
		case cmd == "container show" && strings.Contains(q, "image"):
			fmt.Println(relayParams()["image"])
		case cmd == "container logs":
			if os.Getenv("HELPER_AZ_RELAY_UNREGISTERED") == "1" || strings.Contains(fmt.Sprint(relayParams()["image"]), "unregistered") {
				fmt.Println(`level=WARN msg="relay: hub subscription ended; relaying continues with the last policy"`)
			} else {
				fmt.Println(`level=INFO msg="relay: subscribed to the hub" hub=` + fmt.Sprint(relayParams()["hubAddress"]))
			}
		default:
			fail("fake az: relay " + strings.Join(args, " "))
		}
		return
	}
	switch cmd {
	case "account show":
		if os.Getenv("HELPER_AZ_LOGGED_OUT") == "1" {
			fail("Please run 'az login' to setup account.")
		}
	case "account set":
	case "group exists":
		fmt.Println(exists("rg"))
	case "group create":
		write("rg", flag("-l")+"\n"+strings.TrimPrefix(flag("--tags"), "longarm-hub="))
	case "group show":
		if !exists("rg") {
			fail("ResourceGroupNotFound")
		}
		lines := strings.Split(read("rg"), "\n")
		tags := map[string]string{}
		json.Unmarshal([]byte(read("tags.json")), &tags)
		switch {
		case q == "location":
			fmt.Println(lines[0])
		case q == "id":
			fmt.Println("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/" + flag("-n"))
		case q == "tags":
			tags["longarm-hub"] = lines[1]
			b, _ := json.Marshal(tags)
			fmt.Println(string(b))
		case strings.HasPrefix(q, `tags."longarm-hub-prev`):
			// the settings snapshot (deploy.sh / deploy.ps1 before a delete); az prints nothing for a missing tag
			fmt.Println(tags[strings.TrimSuffix(strings.TrimPrefix(q, `tags."`), `"`)])
		default:
			fmt.Println(lines[1]) // tags."longarm-hub"
		}
	case "tag update":
		if !strings.Contains(flag("--resource-id"), "/resourceGroups/") || flag("--operation") != "Merge" {
			fail("fake az: tag update " + strings.Join(args[2:], " "))
		}
		tags := map[string]string{}
		json.Unmarshal([]byte(read("tags.json")), &tags)
		for i, a := range args {
			if a != "--tags" {
				continue
			}
			for _, kv := range args[i+1:] {
				if strings.HasPrefix(kv, "-") {
					break
				}
				k, v, _ := strings.Cut(kv, "=")
				tags[k] = v
			}
		}
		b, _ := json.Marshal(tags)
		write("tags.json", string(b))
	case "group delete":
		for _, f := range []string{"rg", "cg", "acr", "acr-admin", "tags.json", "deployments"} {
			os.Remove(filepath.Join(dir, f))
		}
	case "provider show":
		// HELPER_AZ_UNREGISTERED: namespaces of a new subscription
		ns := flag("-n")
		st := read("provider-" + ns)
		if st == "" {
			st = "Registered"
			if slices.Contains(strings.Split(os.Getenv("HELPER_AZ_UNREGISTERED"), ","), ns) {
				st = "NotRegistered"
			}
		}
		if st == "Registering" {
			write("provider-"+ns, "Registered") // the next look
		}
		fmt.Println(st)
	case "provider register":
		write("provider-"+flag("-n"), "Registering")
	case "container show":
		if !exists("cg") {
			fail("ResourceNotFound")
		}
		g := group()
		stuck := os.Getenv("HELPER_AZ_STUCK") == "1"
		switch {
		case strings.HasPrefix(q, "[containers[?name=='hub'].image | [0], ipAddress.fqdn"):
			// relay.sh: the hub's image, fqdn, registry (+ identity), state, STUN servers
			reg, ident := "", ""
			if acr, _ := g["acrName"].(string); g["storageAuth"] == "identity" && acr != "" {
				reg, ident = acr+".azurecr.io", "/subscriptions/0/resourceGroups/rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/hub-id"
			} else if g["registryUsername"] != nil {
				reg = "ghcr.io"
				if s, ok := g["registryServer"].(string); ok {
					reg = s
				}
			}
			stun := "self,stun.cloudflare.com:3478,stun.l.google.com:19302"
			if s, ok := g["stunServers"].(string); ok {
				stun = "self"
				if s != "" {
					stun += "," + s
				}
			}
			state := "Running"
			if os.Getenv("HELPER_AZ_HUB_STOPPED") == "1" {
				state = "Terminated"
			}
			h := os.Getenv("HELPER_AZ_HUB")
			tsv(fmt.Sprint(g["image"]), h[:strings.LastIndex(h, ":")], orNone(reg), orNone(ident), state, stun)
		case strings.HasPrefix(q, "[provisioningState"):
			// state, hub container state, container events, group events
			switch {
			case stuck:
				tsv("Creating", "Waiting", "0", "0")
			case crashLoop():
				tsv("Succeeded", "Waiting", "9", "0")
			default:
				tsv("Succeeded", "Running", "4", "0")
			}
		case strings.Contains(q, "restartCount"):
			// the hub container's instance view: restartCount, current state
			// and detail, the previous start's exit code, detail, finish time
			if !crashLoop() {
				tsv("0", "Running", "None", "None", "None", "None")
				return
			}
			n, _ := strconv.Atoi(read("crashes"))
			write("crashes", strconv.Itoa(n+2))
			tsv(strconv.Itoa(n+2), "Waiting", "CrashLoopBackOff: Back-off 40s restarting failed container=hub", "137", "Error", "2026-10-29T08:12:00+00:00")
		case strings.Contains(q, "currentState"):
			if stuck || crashLoop() {
				fmt.Println("Waiting")
			} else {
				fmt.Println("Running")
			}
		case strings.HasPrefix(q, "[containers[0].image"):
			// image, cpu, second container, registry, env names, log
			// workspace, STUN servers (as the template sets them: main.bicep)
			cpu, caddy, reg, logs := "1.0", "", "", ""
			id := g["storageAuth"] == "identity"
			if g["size"] == "medium" {
				cpu = "2.0"
			}
			if g["enablePanel"] == true && !id {
				caddy = "caddy"
			}
			if acr, _ := g["acrName"].(string); id && acr != "" {
				reg = acr + ".azurecr.io" // pulled with the managed identity
			} else if g["registryUsername"] != nil {
				reg = "ghcr.io"
				if s, ok := g["registryServer"].(string); ok {
					reg = s
				}
			}
			if g["enableLogAnalytics"] == true {
				logs = "00000000-0000-0000-0000-000000000000"
			}
			env := "LONGARM_HUB_DATA_DIR,LONGARM_HUB_BLOB_ENDPOINT"
			if id {
				env += ",AZURE_CLIENT_ID,LONGARM_HUB_CA_IN_DB"
			} else {
				env += ",LITESTREAM_AZURE_SAS_TOKEN"
			}
			if g["enablePanel"] == true {
				env += ",LONGARM_HUB_PUBLIC_URL"
			}
			if g["enableTelemetry"] == true {
				env += ",LONGARM_HUB_APPINSIGHTS_CONNECTION_STRING"
			}
			if g["enableDiagnostics"] == true && id {
				env += ",LONGARM_HUB_DIAG_BLOB_URL"
			}
			if g["enablePanel"] == true && id {
				env += ",LONGARM_HUB_HTTPS,LONGARM_HUB_HTTPS_REDIRECT"
			}
			if g["enableHeadscale"] == true && g["enablePanel"] == true {
				env += ",LONGARM_HUB_TAILNET_PROVIDER,LONGARM_HUB_HEADSCALE_DIR"
				if g["headscalePublicDerp"] == true {
					env += ",LONGARM_HUB_HEADSCALE_PUBLIC_DERP"
				}
			}
			if g["tailscaleOAuthSecret"] != nil {
				env += ",LONGARM_HUB_TAILSCALE_OAUTH_SECRET"
			}
			if g["entraTenantId"] != nil {
				env += ",LONGARM_HUB_ENTRA_TENANT_ID,LONGARM_HUB_ENTRA_CLIENT_ID,LONGARM_HUB_ENTRA_WEB"
				if g["entraAdmins"] != nil {
					env += ",LONGARM_HUB_ENTRA_ADMINS"
				}
				if g["entraApps"] != nil {
					env += ",LONGARM_HUB_ENTRA_APPS"
				}
			}
			// LONGARM_HUB_RELAY_STUN_ADVERTISE as the template sets it
			stun := "self,stun.cloudflare.com:3478,stun.l.google.com:19302"
			if s, ok := g["stunServers"].(string); ok {
				stun = "self"
				if s != "" {
					stun += "," + s
				}
			}
			env += ",LONGARM_HUB_RELAY_STUN_ADVERTISE"
			tsv(fmt.Sprint(g["image"]), cpu, orNone(caddy), orNone(reg), env, orNone(logs), stun)
		case strings.Contains(q, "LONGARM_HUB_ENTRA_"):
			// [name, value] of the Entra variables, one per line
			if t, _ := g["entraTenantId"].(string); t != "" {
				row("LONGARM_HUB_ENTRA_TENANT_ID", t)
				row("LONGARM_HUB_ENTRA_CLIENT_ID", fmt.Sprint(g["entraClientId"]))
				row("LONGARM_HUB_ENTRA_WEB", fmt.Sprint(g["entraWeb"]))
				if a, _ := g["entraAdmins"].(string); a != "" {
					row("LONGARM_HUB_ENTRA_ADMINS", a)
				}
				if a, _ := g["entraApps"].(string); a != "" {
					row("LONGARM_HUB_ENTRA_APPS", a)
				}
			}
			// the scheduler gate and the release key in the same query (set only when non-empty)
			if v, _ := g["schedulerGateFreeGb"].(string); v != "" && strings.Contains(q, "LONGARM_HUB_SCHED_GATE_FREE_GB") {
				row("LONGARM_HUB_SCHED_GATE_FREE_GB", v)
			}
			if v, _ := g["releasePublicKey"].(string); v != "" && strings.Contains(q, "LONGARM_HUB_RELEASES_PUBLIC_KEY") {
				row("LONGARM_HUB_RELEASES_PUBLIC_KEY", v)
			}
		case strings.Contains(q, "LONGARM_HUB_SCHED_GATE_FREE_GB"), strings.Contains(q, "LONGARM_HUB_RELEASES_PUBLIC_KEY"):
			// one plain variable (main.bicep sets it only when non-empty)
			p := "schedulerGateFreeGb"
			if strings.Contains(q, "LONGARM_HUB_RELEASES_PUBLIC_KEY") {
				p = "releasePublicKey"
			}
			v, _ := g[p].(string)
			fmt.Println(orNone(v))
		case strings.Contains(q, "image"):
			fmt.Println(g["image"])
		default:
			fail("fake az: container show --query " + q)
		}
	case "container logs":
		// the hub's log: its LONGARM_BOOTSTRAP line has the bootstrap values
		// without the join code; a stale line first (an earlier start)
		if !exists("cg") {
			fail("ResourceNotFound")
		}
		if flag("--container-name") != "hub" {
			fail("fake az: container logs of " + flag("--container-name"))
		}
		line := []string{"LONGARM_BOOTSTRAP"}
		for _, l := range strings.Split(strings.ReplaceAll(string(bootstrap()), "\r", ""), "\n") {
			if k, _, ok := strings.Cut(l, "="); ok && !strings.HasPrefix(k, "#") && !strings.HasPrefix(k, "admin_join_code") {
				line = append(line, l)
			}
		}
		fmt.Println("litestream: restoring hub.db")
		if crashLoop() {
			// the current start, before ACI restarts it again
			fmt.Println("longarm-hub container: restoring /var/lib/longarm-hub/hub.db from the replica (if it has one)")
			fmt.Println("longarm-hub container: restore of /var/lib/longarm-hub/hub.db failed after 9m2s: litestream restore killed by SIGKILL (out of memory? the container has 1536 MB)")
			return
		}
		fmt.Println("LONGARM_BOOTSTRAP boot_id=stale ca_fingerprint=stale hub_address=stale admin_joined=true")
		if exists("restore-until") {
			// this start's restore (the stale line above: an earlier start)
			fmt.Println("longarm-hub container: restoring /var/lib/longarm-hub/hub.db from the replica (if it has one)")
			if restoring() {
				fmt.Println("longarm-hub container: restore of /var/lib/longarm-hub/hub.db still running after 1m0s")
				return
			}
			fmt.Println("longarm-hub container: restore took 7m1.5s (/var/lib/longarm-hub/hub.db)")
		}
		fmt.Println(strings.Join(line, " "))
		fmt.Println("hub: serving")
	case "container delete":
		os.Remove(filepath.Join(dir, "cg"))
	case "container restart":
		restart()
	case "container exec":
		switch flag("--exec-command") {
		case "/usr/local/bin/longarm-hub-admin-code":
			fmt.Println("ABC-DEF")
		case "/usr/local/bin/longarm-hub-relay-cert":
			// like longarm-hub issue --b64 through a TTY: a note, then the bundle (CRLF)
			b, err := os.ReadFile(os.Getenv("HELPER_AZ_RELAY_TLS"))
			if err != nil {
				fail("exec: no such file /usr/local/bin/longarm-hub-relay-cert") // an older hub image
			}
			fmt.Print("relay/myhub-relay bundle (base64 tar.gz of ca.crt node.crt node.key; it holds a private key):\r\n" + strings.TrimSpace(string(b)) + "\r\n")
		default:
			fail("ACI exec takes no arguments") // like the real one
		}
	case "deployment group":
		if !exists("rg") {
			fail("ResourceGroupNotFound")
		}
		host := os.Getenv("HELPER_AZ_HUB")
		panel := ""
		// every deployment: its state (decided when it was created) and parameters
		deps := strings.Fields(read("deployments"))
		params := func(dep string) map[string]any {
			var doc struct {
				Parameters map[string]struct{ Value any } `json:"parameters"`
			}
			json.Unmarshal([]byte(read("dparams-"+dep+".json")), &doc)
			m := map[string]any{}
			for k, v := range doc.Parameters {
				m[k] = v.Value
			}
			return m
		}
		switch args[2] {
		case "validate":
			// preflight: the template and parameters, the subscription's policy
			b, err := os.ReadFile(strings.TrimPrefix(flag("--parameters"), "@"))
			if err != nil || flag("--template-file") == "" {
				fail("fake az: deployment group validate " + strings.Join(args[3:], " "))
			}
			var doc struct {
				Parameters map[string]struct{ Value any } `json:"parameters"`
			}
			if err := json.Unmarshal(b, &doc); err != nil {
				fail("bad parameters file: " + err.Error())
			}
			if os.Getenv("HELPER_AZ_VALIDATE_FAIL") == "1" ||
				(os.Getenv("HELPER_AZ_DENY_KEYS") == "1" && doc.Parameters["storageAuth"].Value != "identity") {
				fail("InvalidTemplateDeployment: The template deployment failed because of policy violation (storage accounts must not allow shared key access).")
			}
			write(fmt.Sprintf("validated-%x", sha256.Sum256(b)), "")
			return
		case "list":
			if !strings.Contains(q, "Succeeded") || os.Getenv("HELPER_AZ_NO_HISTORY") == "1" {
				return
			}
			for _, d := range deps {
				if read("dstate-"+d) == "Succeeded" {
					fmt.Println(d)
				}
			}
			return
		}
		if args[2] == "show" {
			// --no-wait deployments: state and outputs by name
			dep := flag("-n")
			if !slices.Contains(deps, dep) {
				fail("DeploymentNotFound")
			}
			switch {
			case strings.HasPrefix(q, "[properties.parameters."):
				// the kept settings of a past deployment: [properties.parameters.<p>.value, ...,
				// properties.parameters.<p> == null]; az prints True/False and None
				p := params(dep)
				var vals []string
				for _, e := range strings.Split(strings.Trim(q, "[]"), ", ") {
					k := strings.TrimPrefix(e, "properties.parameters.")
					if n, ok := strings.CutSuffix(k, " == null"); ok {
						_, has := p[n]
						vals = append(vals, map[bool]string{true: "False", false: "True"}[has])
						continue
					}
					switch v := p[strings.TrimSuffix(k, ".value")].(type) {
					case nil:
						vals = append(vals, "None")
					case bool:
						vals = append(vals, map[bool]string{true: "True", false: "False"}[v])
					default:
						vals = append(vals, fmt.Sprint(v))
					}
				}
				tsv(vals...)
			case strings.Contains(q, "provisioningState"):
				fmt.Println(read("dstate-" + dep))
			case strings.Contains(q, "relayTcp"):
				// relay.json's outputs: its name, and its TLS fallback (any listener: the hub's)
				tsv("myhub-relay.eastasia.azurecontainer.io", host)
			case strings.Contains(q, "outputs"):
				if g := group(); g["enablePanel"] == true {
					panel = os.Getenv("HELPER_AZ_PANEL")
				}
				tsv(host, host[:strings.LastIndex(host, ":")], "lahubtest", "7445", panel)
			default:
				fail("fake az: deployment group show --query " + q)
			}
			return
		}
		if !slices.Contains(args, "--no-wait") {
			fail("fake az: the scripts deploy with --no-wait")
		}
		dep := flag("-n")
		write("deployment", dep)
		b, err := os.ReadFile(strings.TrimPrefix(flag("--parameters"), "@"))
		if err != nil {
			fail(err.Error())
		}
		var doc struct {
			Parameters map[string]struct{ Value any } `json:"parameters"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			fail("bad parameters file: " + err.Error())
		}
		if _, ok := doc.Parameters["location"]; ok {
			fail("location is the resource group's")
		}
		if !exists(fmt.Sprintf("validated-%x", sha256.Sum256(b))) {
			fail("fake az: deployed without validating these parameters first")
		}
		// the state: HELPER_AZ_DEPLOY_FAIL fails every deployment,
		// HELPER_AZ_DEPLOY_FAIL_IMAGE those of an image containing it,
		// HELPER_AZ_DEPLOY_FAIL_HEADSCALE those with the sidecar (its image
		// can't be pulled: the v0.1.9 incident)
		state := "Succeeded"
		img, _ := doc.Parameters["image"].Value.(string)
		switch {
		case os.Getenv("HELPER_AZ_DEPLOY_FAIL") == "1",
			os.Getenv("HELPER_AZ_DEPLOY_FAIL_IMAGE") != "" && strings.Contains(img, os.Getenv("HELPER_AZ_DEPLOY_FAIL_IMAGE")),
			os.Getenv("HELPER_AZ_DEPLOY_FAIL_HEADSCALE") == "1" && doc.Parameters["enableHeadscale"].Value == true:
			state = "Failed"
		case os.Getenv("HELPER_AZ_STUCK") == "1":
			state = "Running"
		}
		write("dstate-"+dep, state)
		write("dparams-"+dep+".json", string(b))
		write("deployments", strings.Join(append(deps, dep), " "))
		if strings.HasSuffix(flag("--template-file"), "relay.json") {
			// the relay's own group: the hub's stays as it is
			if _, ok := doc.Parameters["relayTls"]; !ok {
				fail("relay.json: relayTls is required")
			}
			write("cg-relay", string(b))
			return
		}
		write("params.json", string(b))
		write("cg", string(b)) // also a failed deployment leaves the group (in a failed state)
		restart()              // a changed group restarts (the SAS expiry changes every time)
	case "deployment operation":
		fmt.Println(`[{"resource": "myhub", "error": {"code": "InaccessibleImage", "message": "fake failure"}}]`)
	case "acr list":
		// one registry at most; queries by tag or by login server, or all as JSON
		n := read("acr")
		switch {
		case flag("-o") == "json":
			regs := []map[string]any{} // az prints [] for none
			if n != "" {
				hub := strings.Split(read("rg"), "\n")[1]
				regs = append(regs, map[string]any{"name": n, "loginServer": n + ".azurecr.io", "tags": map[string]string{"longarm-hub": hub}})
			}
			b, _ := json.Marshal(regs)
			fmt.Println(string(b))
		case n != "" && (strings.Contains(q, "tags") || strings.Contains(q, n+".azurecr.io")):
			fmt.Println(n)
		}
	case "acr create":
		if a := flag("--admin-enabled"); flag("--sku") == "" || (a != "true" && a != "false") {
			fail("fake az: acr create without --sku / --admin-enabled")
		}
		write("acr", flag("-n"))
		write("acr-admin", flag("--admin-enabled"))
	case "acr show":
		if read("acr") != flag("-n") {
			fail("ResourceNotFound")
		}
		fmt.Println(flag("-n") + ".azurecr.io")
	case "acr credential":
		if read("acr") != flag("-n") {
			fail("ResourceNotFound")
		}
		if read("acr-admin") != "true" {
			fail("Run 'az acr update -n " + flag("-n") + " --admin-enabled true' to enable admin first.")
		}
		tsv("acruser", "acr-s3cret-pw")
	case "acr repository":
		// an image of the registry (deploy.sh's pull check with the managed
		// identity); mirror/ images: those imported into it (any registry)
		img := flag("--image")
		if strings.HasPrefix(img, "mirror/") {
			if args[2] != "show" || !slices.Contains(strings.Split(read("acr-mirror"), "\n"), flag("-n")+" "+img) {
				fail("ResourceNotFound: " + img)
			}
			return
		}
		if args[2] != "show" || read("acr") != flag("-n") || strings.Contains(img, "missing") {
			fail("ResourceNotFound: " + img)
		}
	case "acr import":
		// a pinned Docker Hub image by digest, tagged mirror/<repository>:<hex>;
		// HELPER_AZ_IMPORT_FAIL: Docker Hub's pull limit
		src, tag := flag("--source"), flag("-t")
		m := regexp.MustCompile(`^docker\.io/(?:library/)?([a-z0-9./-]+)@(sha256:([0-9a-f]{64}))$`).FindStringSubmatch(src)
		if m == nil || flag("-n") == "" || tag != "mirror/"+m[1]+":"+m[3] {
			fail("fake az: acr import " + strings.Join(args[2:], " "))
		}
		if os.Getenv("HELPER_AZ_IMPORT_FAIL") == "1" {
			fail("toomanyrequests: You have reached your unauthenticated pull rate limit.")
		}
		have := read("acr-mirror")
		for _, img := range []string{"mirror/" + m[1] + "@" + m[2], tag} {
			have += "\n" + flag("-n") + " " + img
		}
		write("acr-mirror", strings.TrimSpace(have))
	case "acr build":
		// what ACR Tasks needs: the stage it can parse, the Dockerfile in the source
		src := args[len(args)-1]
		if flag("--target") != "acr" || flag("-f") != "deploy/hub/Dockerfile" || read("acr") != flag("-r") {
			fail("fake az: acr build " + strings.Join(args[2:], " "))
		}
		if _, err := os.Stat(filepath.Join(src, "deploy", "hub", "Dockerfile")); err != nil {
			fail("no Dockerfile in the source: " + err.Error())
		}
		write("acr-build", strings.Join(args, " "))
		fmt.Println("Run ID: ca1 was successful after 2m3s")
	case "storage account":
		if args[2] == "keys" && identity() {
			fail("KeyBasedAuthenticationNotPermitted") // allowSharedKeyAccess: false
		}
		if args[2] == "list" {
			if exists("deployments") { // the template created it
				fmt.Println("lahubtest")
			}
		} else {
			fmt.Println("dGVzdGtleQ==")
		}
	case "storage blob":
		// the Litestream replica's files (deploy.sh's replica check):
		// HELPER_AZ_REPLICA_FILES L0 files and a snapshot; identity: read with
		// the caller's Entra login, key: the account key from the environment
		if args[2] != "list" || flag("-c") != "litestream" || flag("--prefix") != "hub.db/ltx/" {
			fail("fake az: storage blob " + strings.Join(args[2:], " "))
		}
		if identity() != (flag("--auth-mode") == "login") {
			fail("fake az: storage blob list: --auth-mode login exactly with storage-auth identity")
		}
		if !identity() && (os.Getenv("AZURE_STORAGE_KEY") != "dGVzdGtleQ==" || os.Getenv("AZURE_STORAGE_CONNECTION_STRING") != "") {
			fail("no storage key in AZURE_STORAGE_KEY")
		}
		n, _ := strconv.Atoi(os.Getenv("HELPER_AZ_REPLICA_FILES"))
		if n == 0 {
			fail("AuthorizationPermissionMismatch") // the caller may not read blobs
		}
		for i := 1; i <= n; i++ {
			fmt.Println(fmt.Sprintf("hub.db/ltx/0/%016x-%016x.ltx", i, i))
		}
		fmt.Println(fmt.Sprintf("hub.db/ltx/9/%016x-%016x.ltx", 1, n))
	case "storage file":
		if os.Getenv("AZURE_STORAGE_KEY") != "dGVzdGtleQ==" {
			fail("no storage key in AZURE_STORAGE_KEY")
		}
		// the real az prefers these over the key (another account: ShareNotFound)
		if os.Getenv("AZURE_STORAGE_CONNECTION_STRING") != "" || os.Getenv("AZURE_STORAGE_SAS_TOKEN") != "" || os.Getenv("AZURE_STORAGE_ACCOUNT") != "" {
			fail("ShareNotFound (a connection string / SAS / account from the environment won over the key)")
		}
		if identity() {
			fail("ShareNotFound") // identity: no file shares
		}
		if restoring() {
			fail("ResourceNotFound") // the hub writes bootstrap.env after its restore
		}
		os.WriteFile(flag("--dest"), bootstrap(), 0o644)
	default:
		fail("fake az: unhandled: " + strings.Join(args, " "))
	}
}
