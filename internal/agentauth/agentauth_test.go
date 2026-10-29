package agentauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestMain doubles as a fake gh: copied onto PATH as gh(.exe) it answers
// `gh auth token [--user X]` with "gho_fake_<X|active>" (exit 1 for the
// user "nobody").
func TestMain(m *testing.M) {
	if os.Getenv("AGENTAUTH_FAKE_GH") == "1" {
		args := os.Args[1:]
		if len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			user := "active"
			if len(args) == 4 && args[2] == "--user" {
				user = args[3]
			}
			if user == "nobody" {
				fmt.Fprintln(os.Stderr, "no oauth token found for github.com account nobody")
				os.Exit(1)
			}
			fmt.Println("gho_fake_" + user)
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "fake gh: unexpected", args)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// fakeGHOnPath puts a fake gh (this test binary) alone on PATH.
func fakeGHOnPath(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name = "gh.exe"
	}
	in, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	out.Close()
	t.Setenv("PATH", dir)
	t.Setenv("AGENTAUTH_FAKE_GH", "1")
}

func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseSelection(t *testing.T) {
	for _, c := range []struct {
		vals     []string
		def      string
		auto     bool
		explicit []string
		err      bool
	}{
		{nil, "", true, nil, false},
		{nil, "auto", true, nil, false},
		{nil, "none", false, nil, false},
		{nil, "claude,gh", false, []string{"claude", "gh"}, false},
		{nil, "bogus", false, nil, true},
		{[]string{"none"}, "auto", false, nil, false},
		{[]string{"auto,gh"}, "", true, []string{"gh"}, false},
		{[]string{"claude", "Copilot", "claude"}, "", false, []string{"claude", "copilot"}, false},
		{[]string{"none,claude"}, "", false, nil, true},
		{[]string{"none", "auto"}, "", false, nil, true},
		{[]string{","}, "", false, nil, true},
		{[]string{"codex"}, "", false, nil, true},
	} {
		s, err := ParseSelection(c.vals, c.def)
		if (err != nil) != c.err {
			t.Errorf("%v / %q: err %v", c.vals, c.def, err)
			continue
		}
		if err == nil && (s.Auto != c.auto || !slices.Equal(s.Explicit, c.explicit)) {
			t.Errorf("%v / %q: got %+v", c.vals, c.def, s)
		}
	}
	if s, _ := ParseSelection([]string{"none"}, ""); !s.IsNone() {
		t.Error("none is not IsNone")
	}
}

func TestDetect(t *testing.T) {
	for argv0, want := range map[string]string{
		"claude": "claude", "claude.exe": "claude", `C:\Tools\Claude.EXE`: "claude", "/usr/local/bin/claude": "claude",
		"claude.cmd": "claude", "copilot": "copilot", "copilot.exe": "copilot", `D:\npm\copilot.ps1`: "copilot",
		"pwsh": "", "sh": "", "claude-code": "", "myclaude": "", "": "", "npx": "",
	} {
		if got := Detect([]string{argv0, "-p", "hi"}); got != want {
			t.Errorf("Detect(%q) = %q, want %q", argv0, got, want)
		}
	}
	if Detect(nil) != "" {
		t.Error("empty argv")
	}
	// a shell wrapper is not looked into (documented)
	if Detect([]string{"pwsh", "-c", "claude -p hi"}) != "" {
		t.Error("pwsh -c claude")
	}
}

func TestResolve(t *testing.T) {
	s, _ := ParseSelection([]string{"auto,gh"}, "")
	agents, explicit := s.Resolve([]string{"copilot", "-p", "x"})
	if !slices.Equal(agents, []string{"copilot", "gh"}) || explicit("copilot") || !explicit("gh") {
		t.Fatalf("auto,gh for copilot: %v", agents)
	}
	if agents, _ := s.Resolve([]string{"make"}); !slices.Equal(agents, []string{"gh"}) {
		t.Fatalf("auto,gh for make: %v", agents)
	}
	none, _ := ParseSelection([]string{"none"}, "")
	if agents, _ := none.Resolve([]string{"claude"}); len(agents) != 0 {
		t.Fatalf("none: %v", agents)
	}
}

func TestIsSecretName(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "AWS_SECRET_ACCESS_KEY",
		"MY_PASSWORD", "some_token", "GITHUB_TOKEN"} {
		if !IsSecretName(k) {
			t.Errorf("%s should be secret", k)
		}
	}
	for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
		"ENABLE_TOOL_SEARCH", "DISABLE_NON_ESSENTIAL_MODEL_CALLS"} {
		if IsSecretName(k) {
			t.Errorf("%s should be plain", k)
		}
	}
}

func TestClaudeSettingsEnv(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), "\xef\xbb\xbf"+`{
  "permissions": {"allow": []},
  "env": {
    "ANTHROPIC_BASE_URL": "https://proxy.example.test",
    "ANTHROPIC_AUTH_TOKEN": "tok-from-settings",
    "ANTHROPIC_MODEL": "model-x",
    "CLAUDE_CODE_MAX_OUTPUT_TOKENS": 32000,
    "DISABLE_TELEMETRY": true,
    "IGNORED": null
  }
}`)
	s := &Sources{Home: home, LookupEnv: envOf(nil)}
	b, err := s.Collect(context.Background(), []string{Claude}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Has(Claude) || b.Secret["ANTHROPIC_AUTH_TOKEN"] != "tok-from-settings" || b.Secret["CLAUDE_CODE_MAX_OUTPUT_TOKENS"] != "32000" {
		t.Fatalf("secret: %v", keysOf(b.Secret))
	}
	if b.Plain["ANTHROPIC_BASE_URL"] != "https://proxy.example.test" || b.Plain["ANTHROPIC_MODEL"] != "model-x" ||
		b.Plain["DISABLE_TELEMETRY"] != "true" {
		t.Fatalf("plain: %v", b.Plain)
	}
	if _, ok := b.Plain["IGNORED"]; ok {
		t.Fatal("null forwarded")
	}
	if _, ok := b.Plain["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Fatal("token in plain env")
	}
	if len(b.Sources) != 1 || strings.Contains(b.Sources[0], "tok-") {
		t.Fatalf("sources: %v", b.Sources)
	}

	// the environment wins; CLAUDE_CONFIG_DIR moves the settings
	cfg := t.TempDir()
	writeFile(t, filepath.Join(cfg, "settings.json"), `{"env": {"ANTHROPIC_MODEL": "model-y"}}`)
	s = &Sources{Home: home, LookupEnv: envOf(map[string]string{"CLAUDE_CONFIG_DIR": cfg, "ANTHROPIC_API_KEY": "sk-env",
		"CLAUDE_CODE_OAUTH_TOKEN": "oat-env"})}
	b, err = s.Collect(context.Background(), []string{Claude}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Plain["ANTHROPIC_MODEL"] != "model-y" || b.Secret["ANTHROPIC_API_KEY"] != "sk-env" || b.Secret["CLAUDE_CODE_OAUTH_TOKEN"] != "oat-env" ||
		b.Secret["ANTHROPIC_AUTH_TOKEN"] != "" {
		t.Fatalf("CLAUDE_CONFIG_DIR + env: plain %v secret %v", b.Plain, keysOf(b.Secret))
	}
}

func TestClaudeSubscriptionOnly(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude", ".credentials.json"), `{"claudeAiOauth": {"refreshToken": "rt-must-not-travel"}}`)
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{"env": {"ANTHROPIC_MODEL": "m"}}`)
	s := &Sources{Home: home, LookupEnv: envOf(nil)}
	// auto: a note, nothing forwarded
	b, err := s.Collect(context.Background(), []string{Claude}, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if b.Has(Claude) || len(b.Secret) != 0 || len(b.Plain) != 0 || len(b.Notes) != 1 || !strings.Contains(b.Notes[0], "claude setup-token") {
		t.Fatalf("auto: %+v", b)
	}
	// explicit: an error that says what to do
	_, err = s.Collect(context.Background(), []string{Claude}, func(string) bool { return true })
	var miss *MissingError
	if !errors.As(err, &miss) || miss.Agent != Claude || !strings.Contains(err.Error(), "CLAUDE_CODE_OAUTH_TOKEN") ||
		strings.Contains(err.Error(), "rt-must") {
		t.Fatalf("explicit: %v", err)
	}
	// no Claude Code at all
	s = &Sources{Home: t.TempDir(), LookupEnv: envOf(nil)}
	if _, err := s.Collect(context.Background(), []string{Claude}, func(string) bool { return true }); !errors.As(err, &miss) {
		t.Fatalf("nothing: %v", err)
	}
}

func TestClaudeMalformedSettingsNotQuoted(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".claude", "settings.json"), `{"env": {"ANTHROPIC_AUTH_TOKEN": "leaky-value"`)
	s := &Sources{Home: home, LookupEnv: envOf(nil)}
	_, err := s.Collect(context.Background(), []string{Claude}, nil)
	if err == nil || strings.Contains(err.Error(), "leaky") {
		t.Fatalf("err = %v", err)
	}
}

func TestCopilotSources(t *testing.T) {
	ctx := context.Background()
	fakeGHOnPath(t)
	home := t.TempDir()
	newSrc := func(env map[string]string, cfg Config) *Sources {
		return &Sources{Home: home, LookupEnv: envOf(env), GH: runGH, Config: cfg, StateDir: t.TempDir()}
	}
	get := func(s *Sources) (string, string) {
		t.Helper()
		b, err := s.Collect(ctx, []string{Copilot}, func(string) bool { return true })
		if err != nil {
			t.Fatal(err)
		}
		if len(b.Secret) != 1 || len(b.Plain) != 0 {
			t.Fatalf("bundle: %v %v", keysOf(b.Secret), b.Plain)
		}
		return b.Secret["COPILOT_GITHUB_TOKEN"], b.Sources[0]
	}
	// 1. the environment, in order
	if tok, src := get(newSrc(map[string]string{"GITHUB_TOKEN": "c", "GH_TOKEN": "b", "COPILOT_GITHUB_TOKEN": "a"}, Config{})); tok != "a" ||
		src != "copilot: $COPILOT_GITHUB_TOKEN" {
		t.Fatalf("env: %q %q", tok, src)
	}
	if tok, _ := get(newSrc(map[string]string{"GITHUB_TOKEN": "c", "GH_TOKEN": "b"}, Config{})); tok != "b" {
		t.Fatalf("GH_TOKEN: %q", tok)
	}
	// 2. the configured gh account
	if tok, src := get(newSrc(nil, Config{CopilotGHUser: "octo"})); tok != "gho_fake_octo" || !strings.Contains(src, "--user octo") {
		t.Fatalf("copilot_gh_user: %q %q", tok, src)
	}
	if _, err := newSrc(nil, Config{CopilotGHUser: "nobody"}).Collect(ctx, []string{Copilot}, nil); err == nil ||
		!strings.Contains(err.Error(), "copilot_gh_user") {
		t.Fatalf("copilot_gh_user nobody: %v", err)
	}
	// 3. gh's active account
	if tok, src := get(newSrc(nil, Config{})); tok != "gho_fake_active" || src != "copilot: gh auth token" {
		t.Fatalf("gh: %q %q", tok, src)
	}
	// 4. the pool, round-robin, when gh has nothing
	t.Setenv("PATH", t.TempDir())
	writeFile(t, filepath.Join(home, CopilotPoolFile), "\xef\xbb\xbf"+`{"zed": "tok-z", "amy": {"token": "tok-a"}, "empty": ""}`)
	s := newSrc(nil, Config{})
	var seq []string
	for i := 0; i < 3; i++ {
		tok, _ := get(s)
		seq = append(seq, tok)
	}
	if !slices.Equal(seq, []string{"tok-a", "tok-z", "tok-a"}) {
		t.Fatalf("round-robin: %v", seq)
	}
	// --copilot-account wins over everything
	s = newSrc(map[string]string{"COPILOT_GITHUB_TOKEN": "a"}, Config{})
	s.CopilotAccount = "zed"
	if tok, src := get(s); tok != "tok-z" || src != "copilot: token pool account zed" {
		t.Fatalf("--copilot-account: %q %q", tok, src)
	}
	s.CopilotAccount = "nope"
	if _, err := s.Collect(ctx, []string{Copilot}, nil); err == nil || !strings.Contains(err.Error(), "amy, zed") ||
		strings.Contains(err.Error(), "tok-") {
		t.Fatalf("unknown account: %v", err)
	}
	// nothing at all: auto = a note, explicit = an error
	os.Remove(filepath.Join(home, CopilotPoolFile))
	b, err := newSrc(nil, Config{}).Collect(ctx, []string{Copilot}, nil)
	if err != nil || b.Has(Copilot) || len(b.Notes) != 1 {
		t.Fatalf("auto without credentials: %+v %v", b, err)
	}
	if _, err := newSrc(nil, Config{}).Collect(ctx, []string{Copilot}, func(string) bool { return true }); err == nil {
		t.Fatal("explicit without credentials")
	}
}

func TestGHSource(t *testing.T) {
	ctx := context.Background()
	fakeGHOnPath(t)
	s := &Sources{Home: t.TempDir(), LookupEnv: envOf(nil), GH: runGH}
	b, err := s.Collect(ctx, []string{GH}, nil)
	if err != nil || b.Secret["GH_TOKEN"] != "gho_fake_active" {
		t.Fatalf("gh: %v %v", keysOf(b.Secret), err)
	}
	s.Config.GHUser = "octo"
	if b, _ := s.Collect(ctx, []string{GH}, nil); b.Secret["GH_TOKEN"] != "gho_fake_octo" {
		t.Fatal("gh_user")
	}
	s.Config.GHUser = ""
	s.LookupEnv = envOf(map[string]string{"GITHUB_TOKEN": "env-tok"})
	if b, _ := s.Collect(ctx, []string{GH}, nil); b.Secret["GH_TOKEN"] != "env-tok" {
		t.Fatal("GITHUB_TOKEN")
	}
}

func TestMerge(t *testing.T) {
	b := &Bundle{Secret: map[string]string{"ANTHROPIC_AUTH_TOKEN": "fwd", "GH_TOKEN": "fwd"}, Plain: map[string]string{"ANTHROPIC_MODEL": "fwd", "X": "fwd"}}
	env, sec := b.Merge(map[string]string{"ANTHROPIC_MODEL": "mine", "GH_TOKEN": "plain-mine"}, nil)
	if env["ANTHROPIC_MODEL"] != "mine" || env["X"] != "fwd" || env["GH_TOKEN"] != "plain-mine" {
		t.Fatalf("env %v", env)
	}
	if sec["ANTHROPIC_AUTH_TOKEN"] != "fwd" || len(sec) != 1 {
		t.Fatalf("secrets %v", keysOf(sec))
	}
	env, sec = b.Merge(nil, map[string]string{"X": "secret-mine"})
	if _, ok := env["X"]; ok || sec["X"] != "secret-mine" {
		t.Fatalf("a --secret-env of the name wins: %v %v", env, keysOf(sec))
	}
}

func TestCheck(t *testing.T) {
	type tc struct {
		name              string
		t                 Target
		explicit, shareOK bool
		ok, skip, note    bool
	}
	for _, c := range []tc{
		{"own worker", Target{Me: "alice", Owner: "alice", HubKnown: true, Isolation: "shared"}, false, false, true, false, false},
		{"personal worker (no hub)", Target{Me: "alice", Isolation: "shared"}, false, false, true, false, false},
		{"shared with me, per-user", Target{Me: "bob", Owner: "alice", HubKnown: true, Isolation: "per-user"}, true, false, true, false, false},
		{"fleet, per-user", Target{Me: "bob", HubKnown: true, Isolation: "per-user"}, false, false, true, false, false},
		{"shared with me, shared isolation", Target{Me: "bob", Owner: "alice", HubKnown: true, Isolation: "shared"}, false, false, false, false, false},
		{"fleet, shared isolation", Target{Me: "bob", HubKnown: true, Isolation: "shared"}, true, false, false, false, false},
		{"shared isolation, shared-ok", Target{Me: "bob", Owner: "alice", HubKnown: true, Isolation: "shared"}, false, true, true, false, true},
		{"admin worker, auto", Target{Me: "alice", Owner: "alice", HubKnown: true, Admin: true}, false, false, true, true, true},
		{"admin worker, explicit", Target{Me: "alice", Owner: "alice", HubKnown: true, Admin: true}, true, false, true, false, true},
		{"someone else's admin worker", Target{Me: "bob", Owner: "alice", HubKnown: true, Admin: true, Isolation: "per-user"}, true, true, false, false, false},
	} {
		d, err := Check(c.t, c.explicit, c.shareOK)
		var ref *RefusedError
		if (err == nil) != c.ok || (err != nil && !errors.As(err, &ref)) {
			t.Errorf("%s: err %v", c.name, err)
			continue
		}
		if d.Skip != c.skip || (d.Note != "") != c.note {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
	_, err := Check(Target{Worker: "w", Me: "bob", Owner: "alice", HubKnown: true, Isolation: "shared"}, false, false)
	if err == nil || !strings.Contains(err.Error(), "--agent-auth-shared-ok") || !strings.Contains(err.Error(), "alice's worker") {
		t.Fatalf("refusal message: %v", err)
	}
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
