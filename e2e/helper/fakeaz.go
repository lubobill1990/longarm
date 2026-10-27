package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
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
	restart := func() { n, _ := strconv.Atoi(read("boots")); write("boots", strconv.Itoa(n+1)) }
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
	tsv := func(vals ...string) { fmt.Println(strings.Join(vals, "\t")) }
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
	cmd := strings.Join(args[:min(len(args), 2)], " ")
	q := flag("--query")
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
		switch q {
		case "location":
			fmt.Println(lines[0])
		case "tags":
			b, _ := json.Marshal(map[string]string{"longarm-hub": lines[1]})
			fmt.Println(string(b))
		default:
			fmt.Println(lines[1]) // tags."longarm-hub"
		}
	case "group delete":
		for _, f := range []string{"rg", "cg", "acr", "acr-admin"} {
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
		case strings.HasPrefix(q, "[provisioningState"):
			// state, hub container state, container events, group events
			if stuck {
				tsv("Creating", "Waiting", "0", "0")
			} else {
				tsv("Succeeded", "Running", "4", "0")
			}
		case strings.Contains(q, "currentState"):
			if stuck {
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
				tsv("LONGARM_HUB_ENTRA_TENANT_ID", t)
				tsv("LONGARM_HUB_ENTRA_CLIENT_ID", fmt.Sprint(g["entraClientId"]))
				tsv("LONGARM_HUB_ENTRA_WEB", fmt.Sprint(g["entraWeb"]))
				if a, _ := g["entraAdmins"].(string); a != "" {
					tsv("LONGARM_HUB_ENTRA_ADMINS", a)
				}
				if a, _ := g["entraApps"].(string); a != "" {
					tsv("LONGARM_HUB_ENTRA_APPS", a)
				}
			}
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
		fmt.Println("LONGARM_BOOTSTRAP boot_id=stale ca_fingerprint=stale hub_address=stale admin_joined=true")
		fmt.Println(strings.Join(line, " "))
		fmt.Println("hub: serving")
	case "container delete":
		os.Remove(filepath.Join(dir, "cg"))
	case "container restart":
		restart()
	case "container exec":
		if flag("--exec-command") != "/usr/local/bin/longarm-hub-admin-code" {
			fail("ACI exec takes no arguments") // like the real one
		}
		fmt.Println("ABC-DEF")
	case "deployment group":
		if !exists("rg") {
			fail("ResourceGroupNotFound")
		}
		host := os.Getenv("HELPER_AZ_HUB")
		panel := ""
		if args[2] == "show" {
			// --no-wait deployments: state and outputs by name
			if read("deployment") != flag("-n") {
				fail("DeploymentNotFound")
			}
			switch {
			case strings.Contains(q, "provisioningState"):
				switch {
				case os.Getenv("HELPER_AZ_DEPLOY_FAIL") == "1":
					fmt.Println("Failed")
				case os.Getenv("HELPER_AZ_STUCK") == "1":
					fmt.Println("Running")
				default:
					fmt.Println("Succeeded")
				}
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
		write("deployment", flag("-n"))
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
		write("params.json", string(b))
		write("cg", string(b))
		restart() // a changed group restarts (the SAS expiry changes every time)
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
			fmt.Println("lahubtest")
		} else {
			fmt.Println("dGVzdGtleQ==")
		}
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
		os.WriteFile(flag("--dest"), bootstrap(), 0o644)
	default:
		fail("fake az: unhandled: " + strings.Join(args, " "))
	}
}
