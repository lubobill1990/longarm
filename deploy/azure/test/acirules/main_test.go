package main

import (
	"encoding/json"
	"os"
	"regexp"
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

// TestMirroredImages: the template's third-party images (caddyImage,
// headscaleImage) are the images pinned by digest in
// deploy/azure/mirror-images.txt, from the sources .github/workflows/mirror.yml
// allows; deploy.sh --acr imports exactly those into the registry. Docker Hub
// appears in the template only there (and in descriptions).
func TestMirroredImages(t *testing.T) {
	raw, err := os.ReadFile("../../azuredeploy.json")
	if err != nil {
		t.Fatal(err)
	}
	list, err := os.ReadFile("../../mirror-images.txt")
	if err != nil {
		t.Fatal(err)
	}
	// the repositories .github/workflows/mirror.yml allows (ALLOWED_SOURCES)
	allowed := map[string]bool{"docker.io/library/caddy": true, "docker.io/headscale/headscale": true}
	line := regexp.MustCompile(`^(docker\.io/[a-z0-9./-]+):([A-Za-z0-9._-]+)@sha256:([0-9a-f]{64})$`)
	pinned := map[string]bool{}
	for _, l := range strings.Split(strings.ReplaceAll(string(list), "\r", ""), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		m := line.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("mirror-images.txt: want docker.io/<repository>:<tag>@sha256:<digest>: %q", l)
		}
		if !allowed[m[1]] {
			t.Errorf("mirror-images.txt: %s is not a source mirror.yml allows", m[1])
		}
		pinned[l] = true
	}
	var tpl struct {
		Parameters map[string]struct {
			DefaultValue any `json:"defaultValue"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &tpl); err != nil {
		t.Fatal(err)
	}
	defaults := map[string]bool{}
	for _, p := range []string{"caddyImage", "headscaleImage"} {
		v, _ := tpl.Parameters[p].DefaultValue.(string)
		if !pinned[v] {
			t.Errorf("%s = %q: want an image of mirror-images.txt (pinned by digest)", p, v)
		}
		defaults[v] = true
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.Contains(l, "docker.io/") && !strings.Contains(l, "\"description\"") {
			if _, v, ok := strings.Cut(l, "\"defaultValue\": \""); !ok || !defaults[strings.TrimRight(strings.TrimSpace(v), "\",")] {
				t.Errorf("a Docker Hub image in the template that isn't a pinned sidecar default: %s", strings.TrimSpace(l))
			}
		}
	}
}
