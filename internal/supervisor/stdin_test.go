package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lubobill1990/longarm/internal/jobs"
)

func init() {
	// The fake shim of TestSubmitStdin: copy what longarmd sends on stdin
	// to the file the test names, then exit (the job ends up lost).
	if out := os.Getenv("LONGARM_TEST_SUP_STDIN_OUT"); out != "" {
		b, _ := io.ReadAll(os.Stdin)
		os.WriteFile(out, b, 0o600)
		os.Exit(0)
	}
}

func TestSubmitStdin(t *testing.T) {
	root, tmp := t.TempDir(), t.TempDir()
	got := filepath.Join(tmp, "shim-stdin")
	m, err := New(Options{JobsDir: root, Logger: quiet(), ShimCommand: func(string) *exec.Cmd {
		c := exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(os.Environ(), "LONGARM_TEST_SUP_STDIN_OUT="+got)
		return c
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	const marker = "payload-MARKER-0x5EC2E7"
	payload := []byte("a prompt with " + marker + "\n")
	spec := jobs.Spec{ID: "s1", Argv: []string{"agent"}}
	sent := bytes.Clone(payload)
	j, err := m.SubmitStdin(ctx, spec, map[string]string{"TOKEN": "tok"}, sent)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sent, make([]byte, len(payload))) {
		t.Error("longarmd's copy of the payload not zeroed after the hand-off")
	}
	if j.Spec.StdinBytes != int64(len(payload)) {
		t.Fatalf("StdinBytes = %d", j.Spec.StdinBytes)
	}
	// The shim gets the secrets (one JSON value), then the payload.
	var b []byte
	for deadline := time.Now().Add(10 * time.Second); len(b) == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		b, _ = os.ReadFile(got)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var secrets map[string]string
	if err := dec.Decode(&secrets); err != nil || secrets["TOKEN"] != "tok" {
		t.Fatalf("shim stdin %q: %v", b, err)
	}
	if rest := b[dec.InputOffset():]; !bytes.Equal(rest, payload) { // (the fake shim's own copy)
		t.Fatalf("payload after the secrets = %q", rest)
	}
	// A retry while the job is active returns it (the payload sent again is
	// dropped); without the payload it is another spec.
	if _, err := m.SubmitStdin(ctx, spec, map[string]string{"TOKEN": "tok"}, bytes.Clone(payload)); err != nil {
		t.Fatalf("idempotent resubmit: %v", err)
	}
	if _, err := m.Submit(ctx, spec, map[string]string{"TOKEN": "tok"}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("resubmit without the payload: %v", err)
	}
	// The fake shim took the payload and exited without starting the job:
	// failed with the explicit reason, never "ran with an empty stdin".
	waitState(t, m, "s1", jobs.Failed)
	if j, _ := m.Get("s1"); j.Status.Reason != jobs.StdinLostReason || !j.Status.StartedAt.IsZero() {
		t.Fatalf("lost payload: %+v", j.Status)
	}
	// Resubmitting it with the payload replaces it: a new attempt that gets
	// the payload again.
	os.Remove(got)
	j2, err := m.SubmitStdin(ctx, spec, map[string]string{"TOKEN": "tok"}, bytes.Clone(payload))
	if err != nil || !j2.Spec.CreatedAt.After(j.Spec.CreatedAt) {
		t.Fatalf("resubmit after the loss: %+v %v", j2, err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if b, _ = os.ReadFile(got); bytes.HasSuffix(b, payload) {
			break
		}
	}
	if !bytes.HasSuffix(b, payload) {
		t.Fatalf("the new attempt's shim got %q", b)
	}
	waitState(t, m, "s1", jobs.Failed)
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if c, _ := os.ReadFile(p); bytes.Contains(c, []byte(marker)) {
				t.Errorf("payload on disk in %s", p)
			}
		}
		return nil
	})
	if s, err := jobs.Dir(filepath.Join(root, "s1")).ReadSpec(); err != nil || s.StdinBytes != int64(len(payload)) {
		t.Fatalf("job.json: %+v %v", s, err)
	}

	// Limits: size, and pipe jobs only.
	if _, err := m.SubmitStdin(ctx, jobs.Spec{ID: "s2", Argv: []string{"x"}}, nil, make([]byte, jobs.MaxStdinBytes+1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized payload: %v", err)
	}
	if _, err := m.SubmitStdin(ctx, jobs.Spec{ID: "s3", Argv: []string{"x"}, Pty: true}, nil, []byte("x")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("payload for a session: %v", err)
	}
}

func waitState(t *testing.T, m *Manager, id string, want jobs.State) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		m.pollOnce()
		if j, _ := m.Get(id); j.Status.State == want {
			return
		}
		if time.Now().After(deadline) {
			j, _ := m.Get(id)
			t.Fatalf("%s: state %s, want %s", id, j.Status.State, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestResubmitWorkspaceStdin: a job with a staged workspace (longarm run
// --repo <local checkout> uploads anew - a new staging id - on every
// attempt) whose stdin payload was lost before the start is replaced by a
// resubmit of the same id; per-attempt parts (staging id, whether a fetch
// credential came along) are no part of the idempotency comparison, what
// the workspace resolves to is. The forwarded credential reaches Prepare
// only and is zeroed afterwards.
func TestResubmitWorkspaceStdin(t *testing.T) {
	root, tmp := t.TempDir(), t.TempDir()
	got := filepath.Join(tmp, "shim-stdin")
	type prep struct {
		staging string
		auth    string
	}
	preps := make(chan prep, 10)
	var authSeen *jobs.GitAuth
	m, err := New(Options{JobsDir: root, Logger: quiet(), ShimCommand: func(string) *exec.Cmd {
		c := exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(os.Environ(), "LONGARM_TEST_SUP_STDIN_OUT="+got)
		return c
	}, Prepare: func(_ context.Context, _ jobs.Dir, spec jobs.Spec, _ map[string]string, auth *jobs.GitAuth, _ io.Writer) (string, error) {
		p := prep{staging: spec.Workspace.StagingID}
		if auth != nil {
			p.auth = string(auth.Secret)
			authSeen = auth
		}
		preps <- p
		return tmp, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	payload := []byte("the request\n")
	commit := strings.Repeat("a", 40)
	spec := func(staging string) jobs.Spec {
		return jobs.Spec{ID: "w1", Argv: []string{"agent"}, Workspace: &jobs.Workspace{Repo: "r", Commit: commit, StagingID: staging}}
	}
	if _, err := m.SubmitStdin(ctx, spec("STAGE1"), nil, bytes.Clone(payload)); err != nil {
		t.Fatal(err)
	}
	if p := <-preps; p.staging != "STAGE1" {
		t.Fatalf("prepared %+v", p)
	}
	// the fake shim takes the payload and exits: the payload is lost
	waitState(t, m, "w1", jobs.Failed)
	j, _ := m.Get("w1")
	if j.Status.Reason != jobs.StdinLostReason {
		t.Fatalf("reason %q", j.Status.Reason)
	}
	// the retry uploads again (STAGE2): a new attempt, with the new upload
	j2, err := m.SubmitStdin(ctx, spec("STAGE2"), nil, bytes.Clone(payload))
	if err != nil || !j2.Spec.CreatedAt.After(j.Spec.CreatedAt) {
		t.Fatalf("resubmit after the loss: %+v %v", j2, err)
	}
	if p := <-preps; p.staging != "STAGE2" {
		t.Fatalf("the new attempt prepared %+v", p)
	}
	waitState(t, m, "w1", jobs.Failed)
	// another commit is another job
	other := spec("STAGE4")
	other.Workspace.Commit = strings.Repeat("b", 40)
	if _, err := m.SubmitStdin(ctx, other, nil, bytes.Clone(payload)); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("another commit: %v", err)
	}

	// a forwarded credential: to Prepare only, zeroed after it, recorded as
	// a flag; it needs a workspace url
	secret := []byte("forwarded-SECRET-0001")
	auth := &jobs.GitAuth{Scheme: jobs.GitAuthBearer, Secret: secret}
	us := jobs.Spec{ID: "u1", Argv: []string{"agent"}, Workspace: &jobs.Workspace{Repo: "r", URL: "https://example.com/r", Ref: commit}}
	ju, err := m.SubmitWith(ctx, us, nil, Input{GitAuth: auth})
	if err != nil || !ju.Spec.Workspace.GitAuth {
		t.Fatalf("submit with a credential: %+v %v", ju.Spec.Workspace, err)
	}
	if p := <-preps; p.auth != "forwarded-SECRET-0001" {
		t.Fatalf("Prepare got %+v", p)
	}
	waitState(t, m, "u1", jobs.Lost) // (the fake shim exits)
	if !bytes.Equal(secret, make([]byte, len(secret))) || authSeen.Secret != nil {
		t.Fatalf("credential not zeroed after the fetch: %q", secret)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "u1", jobs.SpecFile)); bytes.Contains(b, []byte("SECRET")) {
		t.Fatal("credential in job.json")
	}
	// a retry (same job, credential again) is the same job; its credential
	// is zeroed unused
	again := []byte("forwarded-SECRET-0002")
	if _, err := m.SubmitWith(ctx, us, nil, Input{GitAuth: &jobs.GitAuth{Scheme: jobs.GitAuthBearer, Secret: again}}); err != nil ||
		!bytes.Equal(again, make([]byte, len(again))) {
		t.Fatalf("retry with a credential: %v %q", err, again)
	}
	noURL := []byte("forwarded-SECRET-0003")
	if _, err := m.SubmitWith(ctx, jobs.Spec{ID: "u2", Argv: []string{"x"}}, nil, Input{GitAuth: &jobs.GitAuth{Scheme: jobs.GitAuthBearer, Secret: noURL}}); !errors.Is(err, ErrInvalid) ||
		!bytes.Equal(noURL, make([]byte, len(noURL))) {
		t.Fatalf("credential without a url: %v %q", err, noURL)
	}
}
