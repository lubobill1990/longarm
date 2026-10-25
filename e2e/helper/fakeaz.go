package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fakeAz is a stand-in for the Azure CLI (helper copied to az / az.exe) for
// deploy/azure/deploy.sh and deploy.ps1: just the calls they make, with the
// state in $HELPER_AZ_DIR (resource group, container group image, the
// deployment's parameters, a log of every call). The deployment "outputs"
// point at a real hub ($HELPER_AZ_HUB) whose share has bootstrap.env
// ($HELPER_AZ_BOOTSTRAP).
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
		os.Remove(filepath.Join(dir, "rg"))
		os.Remove(filepath.Join(dir, "cg"))
	case "container show":
		if !exists("cg") {
			fail("ResourceNotFound")
		}
		switch {
		case strings.Contains(q, "currentState"):
			fmt.Println("Running")
		case strings.Contains(q, "image"):
			fmt.Println(read("cg"))
		}
	case "container delete":
		os.Remove(filepath.Join(dir, "cg"))
	case "container restart":
	case "container exec":
		fmt.Println("ABC-DEF")
	case "container logs":
		fmt.Println("LONGARM_BOOTSTRAP ca_fingerprint=from-logs")
	case "deployment group":
		if !exists("rg") {
			fail("ResourceGroupNotFound")
		}
		p := strings.TrimPrefix(flag("--parameters"), "@")
		b, err := os.ReadFile(p)
		if err != nil {
			fail(err.Error())
		}
		write("params.json", string(b))
		var doc struct {
			Parameters map[string]struct{ Value any } `json:"parameters"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			fail("bad parameters file: " + err.Error())
		}
		image := "ghcr.io/example/longarm-hub:latest"
		if v, ok := doc.Parameters["image"]; ok {
			image = fmt.Sprint(v.Value)
		}
		write("cg", image)
		panel := ""
		if v, ok := doc.Parameters["enablePanel"]; ok && v.Value == true {
			panel = os.Getenv("HELPER_AZ_PANEL")
		}
		host := os.Getenv("HELPER_AZ_HUB")
		fqdn := host[:strings.LastIndex(host, ":")]
		vals := []string{host, fqdn, "lahubtest", panel}
		if flag("-o") == "json" {
			b, _ := json.Marshal(vals)
			fmt.Println(string(b))
		} else {
			fmt.Println(strings.Join(vals, "\t"))
		}
	case "storage account":
		fmt.Println("dGVzdGtleQ==")
	case "storage file":
		b, err := os.ReadFile(os.Getenv("HELPER_AZ_BOOTSTRAP"))
		if err != nil {
			fail("ShareNotFound")
		}
		os.WriteFile(flag("--dest"), b, 0o644)
	default:
		fail("fake az: unhandled: " + strings.Join(args, " "))
	}
}
