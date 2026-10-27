package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
