package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The template in the repository passes; the mistakes of the first real
// deployment are found.
func TestTemplate(t *testing.T) {
	raw, err := os.ReadFile("../../azuredeploy.json")
	if err != nil {
		t.Fatal(err)
	}
	cases, problems, err := Check(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 || cases < 100 {
		t.Fatalf("%d combinations, problems:\n%s", cases, strings.Join(problems, "\n"))
	}

	for _, tc := range []struct {
		name    string
		edit    func(string) string
		overr   map[string]any
		problem string
	}{
		{name: "same port for TCP and UDP", overr: map[string]any{"relayUdpPort": 7443.0}, problem: "DuplicateContainerPorts"},
		{name: "memory not in 0.1 GB steps", edit: func(s string) string {
			return strings.Replace(s, `"memoryInGB": "[json('0.3')]"`, `"memoryInGB": "[json('0.25')]"`, 1)
		}, problem: "steps of 0.1"},
		{name: "too much CPU", edit: func(s string) string {
			return strings.Replace(s, `"cpu": 2,`, `"cpu": 4,`, 1)
		}, problem: "4 vCPU"},
		{name: "group port without a container port", edit: func(s string) string {
			return strings.Replace(s, `"panelPorts": [`, `"panelPorts": [{"port": 8080, "protocol": "TCP"},`, 1)
		}, problem: "not a port of any container"},
		{name: "more than 5 ports per IP", edit: func(s string) string {
			return strings.Replace(s, `"hubPorts": [`, `"hubPorts": [{"port": 3478, "protocol": "UDP"},`, 1)
		}, problem: "at most 5 ports per IP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := string(raw)
			if tc.edit != nil {
				if s = tc.edit(s); s == string(raw) {
					t.Fatal("the edit did not apply: update the test")
				}
			}
			_, problems, err := Check([]byte(s), tc.overr)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(problems, "\n"), tc.problem) {
				t.Fatalf("want %q, got:\n%s", tc.problem, strings.Join(problems, "\n"))
			}
		})
	}
}

// TestMirroredImages: the template pulls nothing from Docker Hub; its
// third-party images are the GHCR mirrors of deploy/azure/mirror-images.txt
// (.github/workflows/mirror.yml), with the digests pinned there.
func TestMirroredImages(t *testing.T) {
	raw, err := os.ReadFile("../../azuredeploy.json")
	if err != nil {
		t.Fatal(err)
	}
	// docker.io may only appear in descriptions (where a mirror comes from)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "docker.io/") && !strings.Contains(line, "\"description\"") {
			t.Errorf("a Docker Hub image in the template: %s", strings.TrimSpace(line))
		}
	}
	list, err := os.ReadFile("../../mirror-images.txt")
	if err != nil {
		t.Fatal(err)
	}
	mirrors := map[string]string{} // mirror tag -> digest
	for _, line := range strings.Split(strings.ReplaceAll(string(list), "\r", ""), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		src, digest, ok := strings.Cut(f[0], "@")
		if len(f) != 2 || !ok || !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 || !strings.Contains(src, ":") {
			t.Fatalf("mirror-images.txt: want <source tag>@sha256:<digest> <mirror tag>: %q", line)
		}
		mirrors[f[1]] = digest
	}
	var tpl struct {
		Parameters map[string]struct {
			DefaultValue any `json:"defaultValue"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"caddyImage", "headscaleImage"} {
		v, _ := tpl.Parameters[p].DefaultValue.(string)
		ref, digest, _ := strings.Cut(v, "@")
		tag, ok := strings.CutPrefix(ref, "ghcr.io/lubobill1990/longarm-hub:")
		if !ok || mirrors[tag] == "" || mirrors[tag] != digest {
			t.Errorf("%s = %q: want ghcr.io/lubobill1990/longarm-hub:<mirror tag>@<digest> of mirror-images.txt", p, v)
		}
	}
}
