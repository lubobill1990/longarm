package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	"github.com/lubobill1990/longarm/internal/trust"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// agentHome is a fake home of the client for agent credential tests: Claude
// Code's settings.json with a proxy token, and an environment that hides
// the real machine's credentials (HOME, USERPROFILE, CLAUDE_CONFIG_DIR, the
// agents' variables and an empty PATH for gh).
type agentHome struct {
	dir string
	env []string
}

const (
	e2eClaudeValue  = "claude-e2e-Secret-0xC1A0DE"
	e2eCopilotValue = "gho_copilotE2eSecret0xC0B1"
)

func newAgentHome(t *testing.T) *agentHome {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".claude")
	os.MkdirAll(cfg, 0o700)
	settings := fmt.Sprintf(`{"env": {"ANTHROPIC_AUTH_TOKEN": %q, "ANTHROPIC_BASE_URL": "https://proxy.example.test", "ANTHROPIC_MODEL": "model-e2e"}}`,
		e2eClaudeValue)
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + dir, "USERPROFILE=" + dir, "CLAUDE_CONFIG_DIR=" + cfg, "PATH=" + t.TempDir()}
	for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		env = append(env, k+"=")
	}
	return &agentHome{dir: dir, env: env}
}

// longarmWith runs longarm with extra environment.
func longarmWith(t *testing.T, cfgDir string, extra []string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bins.longarm, args...)
	cmd.Env = append(append(os.Environ(), "LONGARM_CONFIG_DIR="+cfgDir), extra...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("longarm %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return result{out.String(), errb.String(), code}
}

// fakeAgents copies the helper to claude[.exe] and copilot[.exe] (a fake
// agent: e2e/helper/agentauth.go).
func fakeAgents(t *testing.T) (claude, copilot string) {
	t.Helper()
	dir := t.TempDir()
	claude, copilot = filepath.Join(dir, exeName("claude")), filepath.Join(dir, exeName("copilot"))
	copyBin(t, bins.helper, claude)
	copyBin(t, bins.helper, copilot)
	return claude, copilot
}

func shaLine(name, value string) string {
	return fmt.Sprintf("env %s sha=%x", name, sha256.Sum256([]byte(value)))
}

// noSecretOnDisk fails if a file under root (but not under skip) holds one
// of the secrets.
func noSecretOnDisk(t *testing.T, root, skip string, secrets ...string) {
	t.Helper()
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			if skip != "" && p == skip {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, s := range secrets {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("a forwarded credential is on disk in %s", p)
			}
		}
		return nil
	})
}

// TestAgentAuth covers --agent-auth: Claude Code's and Copilot CLI's
// credentials forwarded from the client per job (auto-detected by the
// command), delivered in the process environment, redacted from logs and
// session transcripts, never on the worker's disk (job.json, audit, logs) -
// except Copilot's own cache in the job's private COPILOT_HOME, which goes
// with the job.
func TestAgentAuth(t *testing.T) {
	e := newEnv(t)
	e.startLongarmd()
	home := newAgentHome(t)
	claude, copilot := fakeAgents(t)
	agentHomes := filepath.Join(e.dataDir, jobs.AgentHomeDir)
	withCopilot := append(append([]string(nil), home.env...), "COPILOT_GITHUB_TOKEN="+e2eCopilotValue)

	// Claude Code (auto: the command is claude): the settings.json env
	// block, the token as a secret, the rest as plain environment.
	r := longarmWith(t, e.client, home.env, "run", "w1", "--wait", "--", claude, "-p", "hi")
	if r.code != 0 || !strings.Contains(r.stderr, "forwarding agent credentials to w1: claude: settings.json env") {
		t.Fatalf("run claude: %+v", r)
	}
	id := strings.TrimSpace(strings.SplitN(r.stdout, "\n", 2)[0])
	logs := e.mustLongarm(e.client, "logs", "w1", id)
	for _, want := range []string{"agent claude", shaLine("ANTHROPIC_AUTH_TOKEN", e2eClaudeValue),
		shaLine("ANTHROPIC_BASE_URL", "https://proxy.example.test"), shaLine("ANTHROPIC_MODEL", "model-e2e"), "echo ***\n"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("claude job log lacks %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, e2eClaudeValue) || strings.Contains(logs, "copilot_home "+agentHomes) {
		t.Fatalf("claude job log:\n%s", logs)
	}
	spec, err := jobs.Dir(filepath.Join(e.dataDir, "jobs", id)).ReadSpec()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.AgentAuth, []string{"claude"}) || !slices.Contains(spec.SecretEnvNames, "ANTHROPIC_AUTH_TOKEN") ||
		spec.Env["ANTHROPIC_BASE_URL"] != "https://proxy.example.test" || spec.AgentHome != "" {
		t.Fatalf("job.json: %+v", spec)
	}
	jl := e.mustLongarm(e.client, "jobs", "w1", "--json", "--id", id)
	if !strings.Contains(compactJSON(jl), `"agentAuth":["claude"]`) || strings.Contains(jl, e2eClaudeValue) {
		t.Fatalf("jobs --json:\n%s", jl)
	}

	// --agent-auth none: nothing forwarded
	r = longarmWith(t, e.client, home.env, "run", "w1", "--wait", "--agent-auth", "none", "--", claude)
	id2 := strings.TrimSpace(r.stdout)
	if r.code != 0 || strings.Contains(r.stderr, "forwarding") ||
		strings.Contains(e.mustLongarm(e.client, "logs", "w1", id2), shaLine("ANTHROPIC_AUTH_TOKEN", e2eClaudeValue)) {
		t.Fatalf("--agent-auth none: %+v", r)
	}
	// an explicit agent without credentials here is an error (exit 2)
	if r := longarmWith(t, e.client, home.env, "run", "w1", "--agent-auth", "copilot", "--", bins.helper, "print", "1", "0", "0"); r.code != 2 ||
		!strings.Contains(r.stderr, "no GitHub token") {
		t.Fatalf("explicit copilot without a token: %+v", r)
	}

	// Copilot CLI: COPILOT_GITHUB_TOKEN, a private COPILOT_HOME per job
	var copHomes []string
	var copIDs []string
	for i := 0; i < 2; i++ {
		r := longarmWith(t, e.client, withCopilot, "run", "w1", "--wait", "--", copilot, "-p", "hi")
		if r.code != 0 || !strings.Contains(r.stderr, "copilot: $COPILOT_GITHUB_TOKEN") {
			t.Fatalf("run copilot: %+v", r)
		}
		cid := strings.TrimSpace(r.stdout)
		logs := e.mustLongarm(e.client, "logs", "w1", cid)
		if !strings.Contains(logs, shaLine("COPILOT_GITHUB_TOKEN", e2eCopilotValue)) || !strings.Contains(logs, "echo ***\n") ||
			strings.Contains(logs, e2eCopilotValue) {
			t.Fatalf("copilot job log:\n%s", logs)
		}
		_, after, _ := strings.Cut(logs, "copilot_home ")
		h := strings.TrimSpace(strings.SplitN(after, "\n", 2)[0])
		if filepath.Dir(h) != agentHomes {
			t.Fatalf("COPILOT_HOME %q is not under %s", h, agentHomes)
		}
		if _, err := os.Stat(filepath.Join(h, "token-cache.json")); err != nil {
			t.Fatalf("the agent's cache: %v", err)
		}
		copHomes, copIDs = append(copHomes, h), append(copIDs, cid)
	}
	if copHomes[0] == copHomes[1] {
		t.Fatal("two jobs shared a COPILOT_HOME")
	}
	// -e COPILOT_HOME wins (handoff resumes into the account's ~/.copilot)
	own := filepath.Join(e.root, "own-copilot-home")
	os.MkdirAll(own, 0o700)
	r = longarmWith(t, e.client, withCopilot, "run", "w1", "--wait", "-e", "COPILOT_HOME="+own, "--", copilot)
	if r.code != 0 || !strings.Contains(e.mustLongarm(e.client, "logs", "w1", strings.TrimSpace(r.stdout)), "copilot_home "+own+"\n") {
		t.Fatalf("-e COPILOT_HOME: %+v", r)
	}
	os.Remove(filepath.Join(own, "token-cache.json"))

	// exec: delivered and redacted; its COPILOT_HOME is gone after the run
	r = longarmWith(t, e.client, withCopilot, "exec", "w1", "--", copilot)
	if r.code != 0 || !strings.Contains(r.stdout, shaLine("COPILOT_GITHUB_TOKEN", e2eCopilotValue)) || !strings.Contains(r.stdout, "echo ***") ||
		strings.Contains(r.stdout, e2eCopilotValue) {
		t.Fatalf("exec copilot: %+v", r)
	}
	_, after, _ := strings.Cut(r.stdout, "copilot_home ")
	if h := strings.TrimSpace(strings.SplitN(after, "\n", 2)[0]); filepath.Dir(h) != agentHomes {
		t.Fatalf("exec COPILOT_HOME %q", h)
	} else if _, err := os.Stat(h); err == nil {
		t.Fatal("exec's COPILOT_HOME outlived the run")
	}

	// a session: the transcript is redacted, sessions --json names the agents
	r = longarmWith(t, e.client, withCopilot, "session", "start", "w1", "-s", "agent", "--cols", "400", "--rows", "40", "--", copilot)
	if r.code != 0 {
		t.Fatalf("session start: %+v", r)
	}
	waitFor(t, "the session's transcript", 30*time.Second, func() bool {
		return strings.Contains(plain(e.longarm(e.client, "logs", "w1", "agent", "--session").stdout), "copilot_home")
	})
	tr := plain(e.mustLongarm(e.client, "logs", "w1", "agent", "--session"))
	if !strings.Contains(tr, shaLine("COPILOT_GITHUB_TOKEN", e2eCopilotValue)) || !strings.Contains(tr, "echo ***") || strings.Contains(tr, e2eCopilotValue) {
		t.Fatalf("session transcript:\n%s", tr)
	}
	if sj := compactJSON(e.mustLongarm(e.client, "sessions", "w1", "--all", "--json")); !strings.Contains(sj, `"agentAuth":["copilot"]`) ||
		strings.Contains(sj, e2eCopilotValue) {
		t.Fatalf("sessions --json: %s", sj)
	}

	// Nothing on the worker's disk - job.json, status, logs, the audit log,
	// longarmd's own log - but the agents' caches in their private homes...
	noSecretOnDisk(t, e.root, agentHomes, e2eClaudeValue, e2eCopilotValue)
	var audit bytes.Buffer
	filepath.Walk(e.dataDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.Contains(strings.ToLower(filepath.Base(p)), "audit") {
			b, _ := os.ReadFile(p)
			audit.Write(b)
		}
		return nil
	})
	if !strings.Contains(audit.String(), `"agent_auth"`) {
		t.Errorf("the audit log does not name the forwarded agents")
	}
	// ... which go with their jobs
	if out := e.mustLongarm(e.admin, "admin", "gc", "w1", "--keep", "0s"); !strings.Contains(out, "removed") {
		t.Fatalf("gc: %s", out)
	}
	for _, h := range copHomes {
		if _, err := os.Stat(h); err == nil {
			t.Errorf("%s outlived its job", h)
		}
	}
	noSecretOnDisk(t, e.root, "", e2eClaudeValue, e2eCopilotValue)
}

// TestAgentAuthE2ETrust: with end-to-end trust on the client, only the
// user's own endorsement of a worker proves it is theirs. A worker that
// claims to be alice's (worker-access.json, i.e. what a hub pushed) but is
// endorsed by another trusted user (bob) is treated as shared: refused with
// shared isolation unless --agent-auth-shared-ok. Endorsed by alice, it is
// hers.
func TestAgentAuthE2ETrust(t *testing.T) {
	h := newHubEnv(t, "")
	e := h.env
	// the hub's (forgeable) word: w1 belongs to alice
	if err := tenancy.WriteWorkerAccess(filepath.Join(e.dataDir, tenancy.WorkerAccessFile), &pb.WorkerAccess{Version: 1, Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	e.startLongarmd()
	home := newAgentHome(t)
	claude, _ := fakeAgents(t)
	run := func(args ...string) result {
		return longarmWith(t, e.client, home.env, append([]string{"run", "w1", "--wait"}, append(args, "--", claude)...)...)
	}
	// without end-to-end trust the hub's word counts (documented)
	if r := run(); r.code != 0 || !strings.Contains(r.stderr, "forwarding agent credentials to w1") {
		t.Fatalf("without e2e trust: %+v", r)
	}

	e.mustLongarm(e.client, "trust", "init")
	e.mustLongarm(e.client, "trust", "require", "on")
	// bob, a user alice trusts, endorses w1
	bobStore := trust.Store{Dir: t.TempDir()}
	bobKey, err := bobStore.CreateUserKey("bob")
	if err != nil {
		t.Fatal(err)
	}
	bobLine := pki.UserKeyLine{User: "bob", Key: bobKey.Public().(ed25519.PublicKey)}
	e.mustLongarm(e.client, "trust", "add-user", bobLine.String())
	ws := trust.Store{Dir: filepath.Join(e.dataDir, "trust")}
	req, err := ws.Request(pki.KindWorker, "w1", "")
	if err != nil {
		t.Fatal(err)
	}
	en, err := pki.Endorse(bobKey, "bob", pki.KindWorker, "w1", req.NodeKey)
	if err != nil {
		t.Fatal(err)
	}
	reissueWorker(t, e, ws, en)
	if out := e.mustLongarm(e.client, "info", "w1"); !strings.Contains(out, "w1") { // bob's endorsement is accepted for the connection
		t.Fatalf("info:\n%s", out)
	}
	r := run()
	if r.code != 2 || !strings.Contains(r.stderr, "not endorsed by your user key") || !strings.Contains(r.stderr, "--agent-auth-shared-ok") {
		t.Fatalf("hub-claimed ownership under e2e trust: %+v", r)
	}
	if r := run("--agent-auth-shared-ok"); r.code != 0 || !strings.Contains(r.stderr, "WARNING") {
		t.Fatalf("--agent-auth-shared-ok: %+v", r)
	}

	// alice endorses w1 herself: hers
	en, err = pki.ParseEndorsement(strings.TrimSpace(e.mustLongarm(e.client, "trust", "endorse", req.String())))
	if err != nil {
		t.Fatal(err)
	}
	reissueWorker(t, e, ws, en)
	if r := run(); r.code != 0 || !strings.Contains(r.stderr, "forwarding agent credentials to w1") || strings.Contains(r.stderr, "WARNING") {
		t.Fatalf("endorsed own worker: %+v", r)
	}
}

// reissueWorker gives w1 a certificate carrying endorsement en of its node
// key (longarmd trust accept + renewal, done directly) and restarts it.
func reissueWorker(t *testing.T, e *env, ws trust.Store, en *pki.Endorsement) {
	t.Helper()
	if err := ws.SetEndorsement(en, pki.KindWorker, "w1"); err != nil {
		t.Fatal(err)
	}
	ct, err := ws.CSRTrust()
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, csrPEM, err := pki.NewKeyAndCSR("w1", ct)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := pki.LoadCA(filepath.Join(e.client, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := ca.SignCSR(csrPEM, pki.IssueOptions{Role: pki.RoleWorker, Name: "w1", Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := pki.WriteBundleAtomic(filepath.Join(e.dataDir, "tls"), ca.CertPEM, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	e.killLongarmd()
	e.startLongarmd()
}
