package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// fakeAgent is a stand-in for Claude Code / Copilot CLI (helper copied to
// claude[.exe] / copilot[.exe]) for the longarm handoff tests:
//
//	--version                  prints $HELPER_AGENT_VERSION (default 9.9.9)
//	auth status --json         {"loggedIn": true, "configDirectory": ...} (claude)
//	--resume <id> | --resume=<id>
//	                           appends a record to $HOME/fake-agent.log - the arguments,
//	                           the working directory, whether the transcript is where the
//	                           agent looks for it and the cwd recorded in it, LONGARM_WORKER /
//	                           LONGARM_SESSION - then appends a "continued on <home>" line to
//	                           the transcript, prints "fake agent ready" and reads stdin until
//	                           it ends (or Ctrl-C ends the process). Claude: while it runs,
//	                           <config>/sessions/<pid>.json names the session, as Claude
//	                           Code's does (handoff's in-use check).
//	anything else              fakeAgentAuth (agentauth.go)
//
// It never prints a transcript's content.
func fakeAgent(agent string, args []string) {
	home, _ := os.UserHomeDir()
	cfg := filepath.Join(home, ".claude")
	if agent == "copilot" {
		cfg = filepath.Join(home, ".copilot")
	}
	switch {
	case len(args) == 1 && args[0] == "--version":
		v := os.Getenv("HELPER_AGENT_VERSION")
		if v == "" {
			v = "9.9.9"
		}
		fmt.Println(v + " (fake " + agent + ")")
		return
	case len(args) >= 2 && args[0] == "auth" && args[1] == "status":
		b, _ := json.Marshal(map[string]any{"loggedIn": true, "configDirectory": cfg, "projectsDirectory": filepath.Join(cfg, "projects")})
		fmt.Println(string(b))
		return
	}
	id := ""
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "--resume="); ok {
			id = v
		} else if a == "--resume" && i+1 < len(args) {
			id = args[i+1]
		}
	}
	if id == "" {
		fakeAgentAuth(agent) // the agent credential tests (agentauth.go)
		return
	}
	cwd, _ := os.Getwd()
	rec := map[string]any{"agent": agent, "args": args, "cwd": cwd, "worker": os.Getenv("LONGARM_WORKER"), "session": os.Getenv("LONGARM_SESSION")}
	var transcript string
	if agent == "claude" {
		transcript = filepath.Join(cfg, "projects", claudeDirName(cwd), id+".jsonl")
		if f, err := os.Open(transcript); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(nil, 16<<20)
			for sc.Scan() {
				var l struct {
					Cwd string `json:"cwd"`
				}
				if json.Unmarshal(sc.Bytes(), &l) == nil && l.Cwd != "" {
					rec["recordedCwd"] = l.Cwd
					break
				}
			}
			f.Close()
			rec["transcript"] = true
		}
	} else {
		transcript = filepath.Join(cfg, "session-state", id, "events.jsonl")
		if b, err := os.ReadFile(filepath.Join(cfg, "session-state", id, "workspace.yaml")); err == nil {
			for _, l := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(l), "cwd:"); ok {
					v = strings.TrimSpace(v)
					var s string
					if json.Unmarshal([]byte(v), &s) == nil {
						v = s
					}
					rec["recordedCwd"] = v
				}
			}
			rec["transcript"] = true
		}
	}
	if rec["transcript"] == true {
		f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			line, _ := json.Marshal(map[string]any{"type": "user", "cwd": cwd, "message": map[string]any{"role": "user", "content": "continued on " + filepath.Base(home)}})
			f.Write(append(line, '\n'))
			f.Close()
		}
	}
	b, _ := json.Marshal(rec)
	if f, err := os.OpenFile(filepath.Join(home, "fake-agent.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		f.Write(append(b, '\n'))
		f.Close()
	}
	if agent == "claude" {
		pidFile := filepath.Join(cfg, "sessions", fmt.Sprintf("%d.json", os.Getpid()))
		os.MkdirAll(filepath.Dir(pidFile), 0o755)
		b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "sessionId": id, "cwd": cwd})
		if os.WriteFile(pidFile, b, 0o600) == nil {
			defer os.Remove(pidFile)
		}
	}
	fmt.Println("fake agent ready")
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "/exit" {
			return
		}
	}
}

// claudeDirName is Claude Code's project directory name of a path.
func claudeDirName(p string) string {
	b := []byte(p)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}
