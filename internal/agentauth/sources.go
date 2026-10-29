package agentauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Bundle is what is forwarded for a command.
type Bundle struct {
	// Agents whose credentials were found (and are forwarded).
	Agents []string
	// Secret goes as secret_env: memory-only on the worker, redacted.
	Secret map[string]string
	// Plain goes as job environment (stored in job.json, shown by jobs).
	Plain map[string]string
	// Sources say where each agent's credential came from, for a note on
	// stderr ("copilot: gh auth token"); never a value.
	Sources []string
	// Notes are messages for the user (an agent skipped, and why).
	Notes []string
}

// Clear zeroes the bundle's secret values (best effort: Go strings are
// immutable; the map entries are dropped).
func (b *Bundle) Clear() {
	if b != nil {
		clear(b.Secret)
	}
}

// Has reports whether agent a is forwarded.
func (b *Bundle) Has(a string) bool { return b != nil && slices.Contains(b.Agents, a) }

// Merge adds the bundle to a request's env and secrets. What the user gave
// explicitly (-e, --secret-env) wins over a forwarded value of the same
// name, in either map. It returns the maps (allocated when nil).
func (b *Bundle) Merge(env, secrets map[string]string) (map[string]string, map[string]string) {
	if b == nil {
		return env, secrets
	}
	for k, v := range b.Plain {
		if _, ok := secrets[k]; ok {
			continue
		}
		if _, ok := env[k]; ok {
			continue
		}
		if env == nil {
			env = map[string]string{}
		}
		env[k] = v
	}
	for k, v := range b.Secret {
		if _, ok := env[k]; ok {
			continue
		}
		if _, ok := secrets[k]; ok {
			continue
		}
		if secrets == nil {
			secrets = map[string]string{}
		}
		secrets[k] = v
	}
	return env, secrets
}

// Sources reads credentials on the client. The zero value is not usable:
// see System.
type Sources struct {
	// Home is the user's home directory.
	Home string
	// LookupEnv reads the client's environment.
	LookupEnv func(string) (string, bool)
	// GH runs gh with args and returns its standard output; nil = gh is
	// not available.
	GH func(ctx context.Context, args ...string) ([]byte, error)
	// StateDir keeps the round-robin position of the Copilot token pool
	// (an index, never a token); "" = always the first account.
	StateDir string
	Config   Config
	// CopilotAccount picks an account of the Copilot token pool
	// (--copilot-account); it wins over every other source.
	CopilotAccount string
}

// System returns the sources of this machine: the real environment, home
// directory and gh on PATH.
func System(stateDir string, cfg Config, copilotAccount string) *Sources {
	home, _ := os.UserHomeDir()
	return &Sources{Home: home, LookupEnv: os.LookupEnv, GH: runGH, StateDir: stateDir, Config: cfg, CopilotAccount: copilotAccount}
}

// runGH runs gh (from PATH) with a timeout; stderr is dropped (it may name
// accounts, never tokens, but it is not ours to print).
func runGH(ctx context.Context, args ...string) ([]byte, error) {
	exe, err := exec.LookPath("gh")
	if err != nil {
		return nil, errNoGH
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(firstLine(stderr.String()))
		if msg != "" {
			return nil, fmt.Errorf("gh %s: %s", strings.Join(args, " "), msg)
		}
		return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

var errNoGH = errors.New("gh is not installed (not on PATH)")

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func (s *Sources) env(k string) string {
	if s.LookupEnv == nil {
		return ""
	}
	v, _ := s.LookupEnv(k)
	return v
}

// MissingError: an agent's credentials were not found on this machine.
type MissingError struct {
	Agent string
	Why   string
}

func (e *MissingError) Error() string { return e.Why }

// Collect gathers the credentials of agents. A missing credential of an
// explicit agent is an error (*MissingError); of an auto-detected one, a
// note in the bundle (the command runs without it).
func (s *Sources) Collect(ctx context.Context, agents []string, explicit func(string) bool) (*Bundle, error) {
	b := &Bundle{Secret: map[string]string{}, Plain: map[string]string{}}
	for _, a := range agents {
		var err error
		switch a {
		case Claude:
			err = s.claude(b)
		case Copilot:
			err = s.copilot(ctx, b)
		case GH:
			err = s.gh(ctx, b)
		default:
			err = fmt.Errorf("unknown agent %q", a)
		}
		var miss *MissingError
		if errors.As(err, &miss) && (explicit == nil || !explicit(a)) {
			b.Notes = append(b.Notes, miss.Why+"; running without forwarded "+a+" credentials")
			continue
		}
		if err != nil {
			b.Clear()
			return nil, err
		}
		b.Agents = append(b.Agents, a)
	}
	return b, nil
}

// ---------------------------------------------------------------- Claude Code

// ClaudeConfigDir is Claude Code's configuration directory: $CLAUDE_CONFIG_DIR,
// else ~/.claude.
func (s *Sources) ClaudeConfigDir() string {
	if d := s.env("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(s.Home, ".claude")
}

// claudeCredNames are the variables that authenticate Claude Code.
var claudeCredNames = []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}

// claudeCloudNames select a cloud provider; its credentials (AWS, Google,
// Azure) are the worker's own and not forwarded.
var claudeCloudNames = []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"}

// SubscriptionNote is printed when Claude Code is signed in with a
// subscription only: its OAuth refresh token is a long-lived account
// credential and stays on this machine.
const SubscriptionNote = "claude: this machine is signed in to Claude Code with a subscription (OAuth) only, and its refresh token is never forwarded: " +
	"run `claude setup-token` and set CLAUDE_CODE_OAUTH_TOKEN, or use an API key (ANTHROPIC_API_KEY) or a proxy token (settings.json env)"

// claude: the env block of settings.json, then ANTHROPIC_API_KEY /
// CLAUDE_CODE_OAUTH_TOKEN from the environment (which win).
func (s *Sources) claude(b *Bundle) error {
	dir := s.ClaudeConfigDir()
	got := map[string]string{}
	src := []string{}
	envBlock, err := readSettingsEnv(filepath.Join(dir, "settings.json"))
	if err != nil {
		return err
	}
	if len(envBlock) > 0 {
		src = append(src, "settings.json env")
	}
	for k, v := range envBlock {
		got[k] = v
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if v := s.env(k); v != "" {
			got[k] = v
			src = append(src, "$"+k)
		}
	}
	cred, cloud := false, false
	for _, k := range claudeCredNames {
		if got[k] != "" {
			cred = true
		}
	}
	for _, k := range claudeCloudNames {
		if v := strings.ToLower(got[k]); v != "" && v != "0" && v != "false" {
			cloud = true
		}
	}
	if !cred && !cloud {
		if _, err := os.Stat(filepath.Join(dir, ".credentials.json")); err == nil {
			return &MissingError{Claude, SubscriptionNote}
		}
		return &MissingError{Claude, "claude: no Claude Code credentials found on this machine (ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY / " +
			"CLAUDE_CODE_OAUTH_TOKEN in the environment or the env block of " + filepath.Join(dir, "settings.json") + "); " +
			"run `claude setup-token` and set CLAUDE_CODE_OAUTH_TOKEN, or use an API key"}
	}
	for k, v := range got {
		if IsSecretName(k) {
			b.Secret[k] = v
		} else {
			b.Plain[k] = v
		}
	}
	note := "claude: " + strings.Join(src, " + ")
	if cloud && !cred {
		note += " (cloud provider: its credentials are the worker's own)"
	}
	b.Sources = append(b.Sources, note)
	return nil
}

// readSettingsEnv reads the "env" object of a Claude Code settings.json:
// strings as they are, numbers and booleans as written, null skipped,
// anything else as JSON. A missing file is no error.
func readSettingsEnv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claude: read %s: %w", path, err)
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var doc struct {
		Env map[string]json.RawMessage `json:"env"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		// never quote the file: it may hold a token
		return nil, fmt.Errorf("claude: %s is not valid JSON", path)
	}
	out := map[string]string{}
	for k, raw := range doc.Env {
		if !validEnvName(k) {
			continue
		}
		var str string
		switch t := bytes.TrimSpace(raw); {
		case len(t) == 0 || string(t) == "null":
			continue
		case json.Unmarshal(t, &str) == nil:
			out[k] = str
		default:
			out[k] = string(t)
		}
	}
	return out, nil
}

func validEnvName(k string) bool {
	return k != "" && !strings.ContainsAny(k, "=\x00")
}

// ---------------------------------------------------------------- Copilot CLI

// CopilotPoolFile is the optional Copilot token pool, relative to the home
// directory: {"<account>": "<token>", ...} (or {"<account>": {"token": ...}}).
var CopilotPoolFile = filepath.Join(".copilot", "accounts", "tokens.json")

// copilot: --copilot-account (the pool), COPILOT_GITHUB_TOKEN / GH_TOKEN /
// GITHUB_TOKEN, gh auth token --user <copilot_gh_user>, gh auth token, the
// pool (round-robin). Forwarded as COPILOT_GITHUB_TOKEN.
func (s *Sources) copilot(ctx context.Context, b *Bundle) error {
	set := func(tok, src string) error {
		b.Secret["COPILOT_GITHUB_TOKEN"] = tok
		b.Sources = append(b.Sources, "copilot: "+src)
		return nil
	}
	if s.CopilotAccount != "" {
		tok, err := s.poolToken(s.CopilotAccount)
		if err != nil {
			return err
		}
		return set(tok, "token pool account "+s.CopilotAccount)
	}
	for _, k := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := s.env(k); v != "" {
			return set(v, "$"+k)
		}
	}
	if u := s.Config.CopilotGHUser; u != "" {
		tok, err := s.ghToken(ctx, u)
		if err != nil {
			return fmt.Errorf("copilot: [agent_auth] copilot_gh_user = %q: %w", u, err)
		}
		return set(tok, "gh auth token --user "+u)
	}
	tok, ghErr := s.ghToken(ctx, "")
	if ghErr == nil {
		return set(tok, "gh auth token")
	}
	acct, tok, err := s.poolNext()
	if err == nil {
		return set(tok, "token pool account "+acct)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return &MissingError{Copilot, "copilot: no GitHub token found on this machine (COPILOT_GITHUB_TOKEN / GH_TOKEN / GITHUB_TOKEN, " +
		"gh auth token: " + ghErr.Error() + ", or " + filepath.Join("~", CopilotPoolFile) + ")"}
}

// ghToken runs gh auth token [--user u].
func (s *Sources) ghToken(ctx context.Context, user string) (string, error) {
	if s.GH == nil {
		return "", errNoGH
	}
	args := []string{"auth", "token"}
	if user != "" {
		args = append(args, "--user", user)
	}
	out, err := s.GH(ctx, args...)
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" || strings.ContainsAny(tok, " \t\r\n") {
		return "", errors.New("gh auth token printed no token")
	}
	return tok, nil
}

// readPool reads the token pool: account -> token.
func (s *Sources) readPool() (map[string]string, error) {
	path := filepath.Join(s.Home, CopilotPoolFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("copilot: %s is not a JSON object of account -> token", path)
	}
	pool := map[string]string{}
	for acct, v := range raw {
		var tok string
		if json.Unmarshal(v, &tok) != nil {
			var obj map[string]any
			if json.Unmarshal(v, &obj) == nil {
				for _, f := range []string{"token", "github_token", "oauth_token"} {
					if t, ok := obj[f].(string); ok {
						tok = t
						break
					}
				}
			}
		}
		if tok = strings.TrimSpace(tok); tok != "" {
			pool[acct] = tok
		}
	}
	if len(pool) == 0 {
		return nil, fmt.Errorf("copilot: %s has no tokens", path)
	}
	return pool, nil
}

func (s *Sources) poolToken(acct string) (string, error) {
	pool, err := s.readPool()
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("--copilot-account %s: there is no token pool (%s)", acct, filepath.Join("~", CopilotPoolFile))
	}
	if err != nil {
		return "", err
	}
	tok, ok := pool[acct]
	if !ok {
		return "", fmt.Errorf("--copilot-account %s: not in the token pool (accounts: %s)", acct, strings.Join(sortedKeys(pool), ", "))
	}
	return tok, nil
}

// poolStateFile keeps the next round-robin index of the pool.
const poolStateFile = "agent-auth-pool.json"

// poolNext picks the pool's accounts in turn (sorted by name).
func (s *Sources) poolNext() (string, string, error) {
	pool, err := s.readPool()
	if err != nil {
		return "", "", err
	}
	accts := sortedKeys(pool)
	var st struct {
		Next int `json:"next"`
	}
	path := ""
	if s.StateDir != "" {
		path = filepath.Join(s.StateDir, poolStateFile)
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &st)
		}
	}
	i := st.Next
	if i < 0 || i >= len(accts) {
		i = 0
	}
	if path != "" {
		st.Next = (i + 1) % len(accts)
		if b, err := json.Marshal(st); err == nil {
			_ = os.WriteFile(path, b, 0o600)
		}
	}
	return accts[i], pool[accts[i]], nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- gh

// gh (opt-in only): GH_TOKEN for agents that use git / gh themselves -
// gh auth token --user <gh_user>, else GH_TOKEN / GITHUB_TOKEN, else gh auth
// token.
func (s *Sources) gh(ctx context.Context, b *Bundle) error {
	set := func(tok, src string) error {
		b.Secret["GH_TOKEN"] = tok
		b.Sources = append(b.Sources, "gh: "+src)
		return nil
	}
	if u := s.Config.GHUser; u != "" {
		tok, err := s.ghToken(ctx, u)
		if err != nil {
			return fmt.Errorf("gh: [agent_auth] gh_user = %q: %w", u, err)
		}
		return set(tok, "gh auth token --user "+u)
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := s.env(k); v != "" {
			return set(v, "$"+k)
		}
	}
	tok, err := s.ghToken(ctx, "")
	if err != nil {
		return &MissingError{GH, "gh: no GitHub token (GH_TOKEN / GITHUB_TOKEN, gh auth token: " + err.Error() + ")"}
	}
	return set(tok, "gh auth token")
}
