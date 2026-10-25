package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
		m := map[string]any{"image": "ghcr.io/example/longarm-hub:latest", "size": "small", "enablePanel": true}
		for k, v := range doc.Parameters {
			m[k] = v.Value
		}
		return m
	}
	tsv := func(vals ...string) { fmt.Println(strings.Join(vals, "\t")) }
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
		for _, f := range []string{"rg", "cg"} {
			os.Remove(filepath.Join(dir, f))
		}
	case "container show":
		if !exists("cg") {
			fail("ResourceNotFound")
		}
		g := group()
		switch {
		case strings.Contains(q, "currentState"):
			fmt.Println("Running")
		case strings.HasPrefix(q, "[containers[0].image"):
			// image, cpu, second container, registry, env names, log workspace
			cpu, caddy, reg, logs := "1.0", "", "", ""
			if g["size"] == "medium" {
				cpu = "2.0"
			}
			if g["enablePanel"] == true {
				caddy = "caddy"
			}
			if g["registryUsername"] != nil {
				reg = "ghcr.io"
			}
			if g["enableLogAnalytics"] == true {
				logs = "00000000-0000-0000-0000-000000000000"
			}
			env := "LONGARM_HUB_DATA_DIR,LITESTREAM_AZURE_SAS_TOKEN"
			if g["tailscaleOAuthSecret"] != nil {
				env += ",LONGARM_HUB_TAILSCALE_OAUTH_SECRET"
			}
			tsv(fmt.Sprint(g["image"]), cpu, caddy, reg, env, logs)
		case strings.Contains(q, "image"):
			fmt.Println(g["image"])
		default:
			fail("fake az: container show --query " + q)
		}
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
		panel := ""
		if v, ok := doc.Parameters["enablePanel"]; !ok || v.Value == true {
			panel = os.Getenv("HELPER_AZ_PANEL")
		}
		host := os.Getenv("HELPER_AZ_HUB")
		tsv(host, host[:strings.LastIndex(host, ":")], "lahubtest", "7443", panel)
	case "storage account":
		if args[2] == "list" {
			fmt.Println("lahubtest")
		} else {
			fmt.Println("dGVzdGtleQ==")
		}
	case "storage file":
		if os.Getenv("AZURE_STORAGE_KEY") != "dGVzdGtleQ==" {
			fail("no storage key in AZURE_STORAGE_KEY")
		}
		b, err := os.ReadFile(os.Getenv("HELPER_AZ_BOOTSTRAP"))
		if err != nil {
			fail("ShareNotFound")
		}
		b = regexp.MustCompile(`(?m)^boot_id=.*$`).ReplaceAll(b, []byte("boot_id=fake-"+read("boots")))
		os.WriteFile(flag("--dest"), b, 0o644)
	default:
		fail("fake az: unhandled: " + strings.Join(args, " "))
	}
}
