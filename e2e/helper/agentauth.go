package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// agentAuthName: the helper copied to claude[.exe] / copilot[.exe] is a
// fake coding agent for the agent credential tests (e2e/agentauth_test.go).
func agentAuthName(argv0 string) string {
	b := strings.TrimSuffix(strings.ToLower(filepath.Base(argv0)), ".exe")
	if b == "claude" || b == "copilot" {
		return b
	}
	return ""
}

// fakeAgentAuth prints which agent variables it got - names and a hash of
// each value, never a value - then "echo <credential>" (the credential
// itself, which the worker must redact from the log), "copilot_home <dir>",
// and writes a token cache into COPILOT_HOME, as Copilot CLI may do.
func fakeAgentAuth(name string) {
	fmt.Println("agent " + name)
	var keys []string
	for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL",
		"CLAUDE_CODE_OAUTH_TOKEN", "COPILOT_GITHUB_TOKEN", "GH_TOKEN"} {
		if _, ok := os.LookupEnv(k); ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("env %s sha=%x\n", k, sha256.Sum256([]byte(os.Getenv(k))))
	}
	cred := os.Getenv("ANTHROPIC_AUTH_TOKEN")
	if name == "copilot" {
		cred = os.Getenv("COPILOT_GITHUB_TOKEN")
	}
	fmt.Println("echo " + cred)
	home := os.Getenv("COPILOT_HOME")
	fmt.Println("copilot_home " + home)
	if home != "" {
		if err := os.WriteFile(filepath.Join(home, "token-cache.json"), []byte(`{"token":"`+cred+`"}`), 0o600); err != nil {
			fmt.Println("cache error:", err)
			os.Exit(1)
		}
	}
}
