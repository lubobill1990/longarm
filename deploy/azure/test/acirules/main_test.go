package main

import (
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
