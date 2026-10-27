package shim

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/lubobill1990/longarm/internal/jobs"
)

func TestReadInput(t *testing.T) {
	for _, c := range []struct {
		name, in string
		secrets  map[string]string
		stdin    string
	}{
		{"nothing", "", map[string]string{}, ""},
		{"null secrets", "null", nil, ""},
		{"secrets only", `{"A":"1"}`, map[string]string{"A": "1"}, ""},
		{"secrets and stdin", `{"A":"1"}line 1` + "\n{not json}\n", map[string]string{"A": "1"}, "line 1\n{not json}\n"},
		{"no secrets, stdin", `{}` + "  leading spaces kept", map[string]string{}, "  leading spaces kept"},
	} {
		sec, in, err := readInput(strings.NewReader(c.in))
		if err != nil || len(sec) != len(c.secrets) || sec["A"] != c.secrets["A"] || string(in) != c.stdin {
			t.Errorf("%s: %v %q %v", c.name, sec, in, err)
		}
	}
	if _, _, err := readInput(strings.NewReader("{bad")); err == nil {
		t.Error("bad JSON accepted")
	}
	big := append([]byte("{}"), bytes.Repeat([]byte("x"), jobs.MaxStdinBytes+1)...)
	if _, _, err := readInput(bytes.NewReader(big)); err == nil {
		t.Error("oversized stdin accepted")
	}
	if _, in, err := readInput(bytes.NewReader(big[:len(big)-1])); err != nil || len(in) != jobs.MaxStdinBytes {
		t.Errorf("stdin of exactly the limit: %d %v", len(in), err)
	}
}

// A payload that arrived short (longarmd died while handing it over) fails
// the job before anything starts.
func TestPartialPayloadNeverRuns(t *testing.T) {
	d := jobs.Dir(t.TempDir())
	if err := d.WriteSpec(&jobs.Spec{ID: "j", Argv: []string{"never-started"}, StdinBytes: 10}); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteStatus(&jobs.Status{State: jobs.Preparing}); err != nil {
		t.Fatal(err)
	}
	if err := run(d, strings.NewReader(`{}abc`), slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	st, err := d.ReadStatus()
	if err != nil || st.State != jobs.Failed || st.PID != 0 || !strings.HasPrefix(st.Reason, jobs.StdinLostReason) ||
		!strings.Contains(st.Reason, "got 3 of 10 bytes") {
		t.Fatalf("status %+v %v", st, err)
	}
}
