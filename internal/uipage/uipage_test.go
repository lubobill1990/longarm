package uipage

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	for _, c := range []struct {
		page string
		t    Target
		want string
	}{
		{"", Target{}, "/tray?tab=pool"},
		{"workers", Target{}, "/tray?tab=pool"},
		{"add-machine", Target{}, "/tray?add=1&tab=pool"},
		{"jobs", Target{}, "/tray?tab=jobs"},
		{"jobs", Target{Job: "01JZ0000000000000000000000"}, "/tray?job=01JZ0000000000000000000000&tab=jobs"},
		{"this-worker", Target{}, "/tray?tab=worker"},
		{"settings", Target{}, "/tray?tab=settings"},
		{"account", Target{}, "/tray?tab=account"},
		{"sessions", Target{}, "/sessions"},
		{"sessions", Target{Worker: "worker-1"}, "/sessions#worker=worker-1"},
		{"sessions", Target{Worker: "worker-1", Job: "x7k2", Session: "dev", Agent: "copilot"}, "/sessions#agent=copilot&job=x7k2&session=dev&worker=worker-1"},
	} {
		pg, err := Resolve(c.page, c.t)
		if err != nil || pg.String() != c.want {
			t.Errorf("Resolve(%q, %+v) = %q, %v; want %q", c.page, c.t, pg, err, c.want)
		}
	}
	// every listed page resolves
	for _, p := range Pages {
		if _, err := Resolve(p, Target{}); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestResolveRefuses(t *testing.T) {
	var unknown *UnknownPageError
	if _, err := Resolve("pool", Target{}); !errors.As(err, &unknown) || !strings.Contains(err.Error(), strings.Join(Pages, ", ")) {
		t.Fatalf("unknown page: %v", err)
	}
	for _, c := range []struct {
		page string
		t    Target
		want string
	}{
		{"workers", Target{Job: "j"}, "--job is not for the workers page"},
		{"jobs", Target{Worker: "w"}, "--worker is not for the jobs page"},
		{"settings", Target{Session: "s"}, "--session is not for"},
		{"sessions", Target{Agent: "aider"}, "--agent"},
		{"sessions", Target{Worker: "w1&key=x"}, "--worker: not a valid name"},
		{"sessions", Target{Session: "-x"}, "--session: not a valid name"},
		{"jobs", Target{Job: "a/b"}, "--job: not a valid name"},
		{"sessions", Target{Worker: strings.Repeat("w", 129)}, "not a valid name"},
	} {
		_, err := Resolve(c.page, c.t)
		if err == nil || errors.As(err, &unknown) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Resolve(%q, %+v): %v, want %q", c.page, c.t, err, c.want)
		}
	}
}

func TestOpenNotRunning(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(context.Background(), AddrFile(dir), Request{Page: "jobs"}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("no address file: %v", err)
	}
	if AddrFile(dir) != filepath.Join(dir, "tray.addr") {
		t.Fatal(AddrFile(dir))
	}
}
