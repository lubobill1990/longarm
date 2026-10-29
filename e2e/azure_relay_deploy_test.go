package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/pki"
)

// TestAzureDeployRelay: deploy.sh / deploy.ps1 --relay against the fake az
// (e2e/helper/fakeaz.go) with a hub already deployed: the relay certificate
// comes from the hub (az container exec longarm-hub-relay-cert) into a
// secure parameter, never onto az's command line; the relay gets its own
// container group with the hub's image and STUN servers and is checked to
// have registered with the hub; the hub's group is not touched. A hub that
// is stopped or too old (no relay certificate alias) is refused before
// anything changes; a relay upgrade that doesn't come up is rolled back to
// the previous image; --relay-remove deletes only the relay.
func TestAzureDeployRelay(t *testing.T) {
	t.Parallel()
	// the relay bundle as longarm-hub issue --role relay --b64 prints it
	ca, err := pki.InitCA(filepath.Join(t.TempDir(), "ca"), "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{Role: pki.RoleRelay, Name: "myhub-relay", Hosts: []string{"myhub-relay.eastasia.azurecontainer.io"}})
	if err != nil {
		t.Fatal(err)
	}
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		data []byte
	}{{"ca.crt", ca.CertPEM}, {"node.crt", certPEM}, {"node.key", keyPEM}} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o600, Size: int64(len(f.data)), ModTime: time.Now()})
		tw.Write(f.data)
	}
	tw.Close()
	gz.Close()
	bundle := base64.StdEncoding.EncodeToString(tgz.Bytes())
	tlsFile := filepath.Join(t.TempDir(), "relay-tls.b64")
	os.WriteFile(tlsFile, []byte(bundle), 0o600)
	// the relay's TLS fallback as the scripts check it (fake az: relay.json's relayTcp output)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	for _, r := range deployRunners(t) {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			az := filepath.Join(dir, exeName("az"))
			copyBin(t, bins.helper, az)
			azPath := az
			if r.name == "deploy.sh" {
				azPath = bashPath(az)
			}
			// a deployed hub: its resource group and container group
			os.WriteFile(filepath.Join(dir, "rg"), []byte("eastasia\nmyhub"), 0o644)
			os.WriteFile(filepath.Join(dir, "cg"), []byte(`{"parameters":{"name":{"value":"myhub"},"image":{"value":"ghcr.io/example/longarm-hub:0.2.0"}}}`), 0o644)
			hubCg := string(must(os.ReadFile(filepath.Join(dir, "cg"))))
			run := func(extraEnv []string, args ...string) result {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				argv := r.argv(args...)
				cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
				cmd.Env = append(append(os.Environ(), "LONGARM_DEPLOY_AZ="+azPath, "HELPER_AZ_DIR="+dir, "HELPER_AZ_HUB="+ln.Addr().String(),
					"HELPER_AZ_RELAY_TLS="+tlsFile, "LONGARM_DEPLOY_POLL_SECONDS=1", "LONGARM_REGISTRY_PASSWORD="), extraEnv...)
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
			calls := func() string { b, _ := os.ReadFile(filepath.Join(dir, "calls.log")); return string(b) }
			relayParams := func() map[string]any {
				t.Helper()
				var doc struct {
					Parameters map[string]struct{ Value any } `json:"parameters"`
				}
				if err := json.Unmarshal(must(os.ReadFile(filepath.Join(dir, "cg-relay"))), &doc); err != nil {
					t.Fatal(err)
				}
				m := map[string]any{}
				for k, v := range doc.Parameters {
					m[k] = v.Value
				}
				return m
			}
			name, relay, yes := r.flag("--name", "-Name"), r.flag("--relay", "-Relay"), r.flag("--yes", "-Yes")
			refused := func(res result, what, why string) {
				t.Helper()
				if res.code == 0 || !strings.Contains(strings.ToLower(res.stderr+res.stdout), strings.ToLower(why)) {
					t.Fatalf("%s: not refused with %q: %+v", what, why, res)
				}
				if _, err := os.Stat(filepath.Join(dir, "cg-relay")); err == nil && !strings.Contains(what, "upgrade") {
					t.Fatalf("%s: a relay was deployed anyway", what)
				}
			}

			refused(run([]string{"HELPER_AZ_HUB_STOPPED=1"}, name, "myhub", relay), "a stopped hub", "start the hub first")
			refused(run([]string{"HELPER_AZ_RELAY_TLS=" + filepath.Join(dir, "none")}, name, "myhub", relay), "a hub image without the alias", "upgrade the hub first")
			refused(run(nil, name, "myhub", relay, r.flag("--recreate", "-Recreate")), "--relay --recreate", "external relay only")

			res := run(nil, name, "myhub", relay)
			if res.code != 0 || !strings.Contains(res.stdout, "The external relay is up") || !strings.Contains(res.stdout, "longarm admin relays") {
				t.Fatalf("--relay: %+v\ncalls:\n%s", res, calls())
			}
			p := relayParams()
			host, _, _ := net.SplitHostPort(ln.Addr().String())
			if p["name"] != "myhub" || p["image"] != "ghcr.io/example/longarm-hub:0.2.0" || p["relayTls"] != bundle ||
				p["hubAddress"] != host+":7443" || p["stunServers"] != "stun.cloudflare.com:3478,stun.l.google.com:19302" || p["location"] != nil {
				t.Fatalf("relay parameters: %+v", p)
			}
			cl := calls()
			if strings.Contains(cl, bundle[:64]) {
				t.Fatal("the relay's private key bundle on az's command line:\n" + cl)
			}
			if !strings.Contains(cl, "deployment group validate") || !strings.Contains(cl, "relay.json") ||
				!strings.Contains(cl, "container exec -g myhub-rg -n myhub --container-name hub --exec-command /usr/local/bin/longarm-hub-relay-cert") {
				t.Fatalf("calls:\n%s", cl)
			}
			if string(must(os.ReadFile(filepath.Join(dir, "cg")))) != hubCg || strings.Contains(cl, "container restart") {
				t.Fatalf("the hub's container group was touched:\ncalls:\n%s", cl)
			}

			// an upgrade whose relay never registers with the hub: rolled back
			res = run(nil, name, "myhub", relay, r.flag("--image", "-Image"), "ghcr.io/example/longarm-hub:unregistered", r.flag("--timeout", "-TimeoutSeconds"), "5")
			if res.code == 0 || !strings.Contains(res.stderr+res.stdout, "ROLLED BACK") {
				t.Fatalf("failed upgrade: %+v", res)
			}
			if img := relayParams()["image"]; img != "ghcr.io/example/longarm-hub:0.2.0" {
				t.Fatalf("after the rollback the relay runs %v", img)
			}

			res = run(nil, name, "myhub", r.flag("--relay-remove", "-RelayRemove"), yes)
			if res.code != 0 || !strings.Contains(res.stdout, "longarm admin revoke myhub-relay") {
				t.Fatalf("--relay-remove: %+v", res)
			}
			if _, err := os.Stat(filepath.Join(dir, "cg-relay")); err == nil {
				t.Fatal("the relay group is still there")
			}
			if _, err := os.Stat(filepath.Join(dir, "cg")); err != nil {
				t.Fatal("--relay-remove deleted the hub")
			}
		})
	}
}
