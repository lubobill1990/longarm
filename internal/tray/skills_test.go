package tray

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/agentskill"
)

// TestSkillsAPI: the Settings section's API behind the tray's guard: the
// session cookie, JSON and same-origin rules; an agent from the table only;
// a changed copy is refused (409) and left alone.
func TestSkillsAPI(t *testing.T) {
	home := t.TempDir()
	old := skillEnv
	skillEnv = func() agentskill.Env {
		return agentskill.Env{Home: home, Getenv: func(string) string { return "" },
			LookPath: func(b string) (string, error) {
				if b == "copilot" {
					return "/bin/copilot", nil
				}
				return "", errors.New("no")
			}}
	}
	t.Cleanup(func() { skillEnv = old })

	s, err := NewServer(Options{LongarmConfigDir: t.TempDir(), LongarmdDataDir: t.TempDir(), LongarmExe: "longarm", LongarmdExe: "longarmd", Roles: "client"}, &fakeDesk{}, "")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve()
	defer s.Close()
	base := "http://" + s.Addr()

	// no session: nothing is listed or installed
	if resp, _ := http.Get(base + "/trayapi/skills"); resp.StatusCode != 401 {
		t.Fatalf("no session: %d", resp.StatusCode)
	}
	if resp, _ := http.Post(base+"/trayapi/skills/install", "application/json", strings.NewReader(`{"agent":"copilot"}`)); resp.StatusCode != 401 {
		t.Fatalf("no session install: %d", resp.StatusCode)
	}

	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if resp, err := c.Get(s.URL("/tray")); err != nil || resp.StatusCode != 200 {
		t.Fatalf("handshake: %v", err)
	}
	call := func(method, p, body string, hdr ...string) (int, skillsAnswer, string) {
		req, _ := http.NewRequest(method, base+p, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var a skillsAnswer
		json.Unmarshal(b, &a)
		return resp.StatusCode, a, string(b)
	}
	by := func(a skillsAnswer, id string) agentskill.Status {
		for _, st := range a.Agents {
			if st.Agent == id {
				return st
			}
		}
		t.Fatalf("no %s in %+v", id, a)
		return agentskill.Status{}
	}

	code, a, raw := call("GET", "/trayapi/skills", "")
	if code != 200 || len(a.Agents) != len(agentskill.Agents) || !by(a, "copilot").Detected || by(a, "claude").Detected || by(a, "copilot").State != agentskill.Absent {
		t.Fatalf("list: %d %s", code, raw)
	}
	dir := filepath.Join(home, ".copilot", "skills", "longarm")
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("listing wrote the skill")
	}
	// cross-site and unknown agents are refused
	if code, _, _ := call("POST", "/trayapi/skills/install", `{"agent":"copilot"}`, "Origin", "http://evil.example"); code != 403 {
		t.Fatalf("cross-origin: %d", code)
	}
	for _, bad := range []string{`{"agent":"../../x"}`, `{"agent":""}`, `{"dir":"C:/Windows"}`} {
		if code, _, _ := call("POST", "/trayapi/skills/install", bad); code != 400 {
			t.Fatalf("%s: %d", bad, code)
		}
	}

	code, a, raw = call("POST", "/trayapi/skills/install", `{"agent":"copilot"}`)
	if code != 200 || by(a, "copilot").State != agentskill.Current || !strings.Contains(a.Message, "installed") {
		t.Fatalf("install: %d %s", code, raw)
	}
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	// changed by the user: remove is refused, the file stays
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("mine"), 0o644)
	if code, _, raw = call("POST", "/trayapi/skills/remove", `{"agent":"copilot"}`); code != 409 || !strings.Contains(raw, "SKILL.md") {
		t.Fatalf("remove modified: %d %s", code, raw)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "SKILL.md")); string(b) != "mine" {
		t.Fatal("changed file touched")
	}
	os.RemoveAll(dir)
	call("POST", "/trayapi/skills/install", `{"agent":"copilot"}`)
	if code, a, raw = call("POST", "/trayapi/skills/remove", `{"agent":"copilot"}`); code != 200 || by(a, "copilot").State != agentskill.Absent {
		t.Fatalf("remove: %d %s", code, raw)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("skill dir left behind")
	}
}
