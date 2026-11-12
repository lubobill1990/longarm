package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"longarm/internal/localapi"
	"longarm/internal/uipage"
)

func uiCLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	var validated bool
	markUsageErrors(root, &validated)
	root.SetArgs(append([]string{"ui", "open"}, args...))
	err := root.Execute()
	if err != nil {
		out.WriteString(err.Error())
	}
	return out.String(), exitCodeFor(err, validated)
}

// fakeTray serves the control channel of a tray for dir and records the
// requests.
func fakeTray(t *testing.T, dir string) *[]uipage.Request {
	t.Helper()
	ch, err := localapi.ListenChannel("tray", dir, uipage.AddrFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []uipage.Request
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req uipage.Request
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		got = append(got, req)
		mu.Unlock()
		pg, _ := uipage.Resolve(req.Page, req.Target)
		res := uipage.Result{URL: "http://127.0.0.1:1" + pg.String(), Opened: !req.PrintURL}
		if req.PrintURL {
			res.URL = "http://127.0.0.1:1" + pg.Path + "?token=abc"
		}
		json.NewEncoder(w).Encode(res)
	})}
	go srv.Serve(ch.Listener)
	t.Cleanup(func() { ch.Release(); srv.Close() })
	return &got
}

func TestUIOpenCLI(t *testing.T) {
	dir := t.TempDir()
	// refused before anything starts: an unknown page (the list), a flag
	// the page does not take, a bad value
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--config-dir", dir, "pool"}, "pages: workers, add-machine, jobs, this-worker, settings, account, sessions"},
		{[]string{"--config-dir", dir, "workers", "--job", "x"}, "--job is not for the workers page"},
		{[]string{"--config-dir", dir, "sessions", "--agent", "aider"}, "--agent"},
		{[]string{"--config-dir", dir, "jobs", "extra"}, "accepts at most 1 arg"},
	} {
		if out, code := uiCLI(t, c.args...); code != exitUsage || !strings.Contains(out, c.want) {
			t.Errorf("%v: exit %d %s", c.args, code, out)
		}
	}

	got := fakeTray(t, dir)
	if out, code := uiCLI(t, "--config-dir", dir); code != 0 || out != "" {
		t.Fatalf("default page: %d %q", code, out)
	}
	out, code := uiCLI(t, "--config-dir", dir, "sessions", "--worker", "worker-1", "--job", "01JZ8K", "--json")
	var res uipage.Result
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || !res.Opened || res.URL != "http://127.0.0.1:1/sessions#job=01JZ8K&worker=worker-1" || !strings.Contains(out, "#job=01JZ8K&worker") {
		t.Fatalf("sessions --json: %d %s", code, out)
	}
	if out, code := uiCLI(t, "--config-dir", dir, "jobs", "--print-url"); code != 0 || out != "http://127.0.0.1:1/tray?tab=jobs?token=abc\n" {
		t.Fatalf("--print-url: %d %q", code, out)
	}
	want := []uipage.Request{
		{Page: "workers"},
		{Page: "sessions", Target: uipage.Target{Worker: "worker-1", Job: "01JZ8K"}},
		{Page: "jobs", PrintURL: true},
	}
	if len(*got) != len(want) {
		t.Fatalf("requests: %+v", *got)
	}
	for i := range want {
		if (*got)[i] != want[i] {
			t.Errorf("request %d: %+v, want %+v", i, (*got)[i], want[i])
		}
	}
}

// TestOpenUINoTray: without a tray answering and without longarm-tray to
// start, ui open says so (exit 3).
func TestOpenUINoTray(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := openUI(context.Background(), t.TempDir(), uipage.Request{Page: "workers"})
	if code := exitCodeFor(err, true); code != exitNotFound || !strings.Contains(err.Error(), "longarm-tray is not installed") {
		t.Fatalf("no tray: %v (exit %d)", err, code)
	}
}
