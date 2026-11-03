// Package uipage names the pages of Longarm's own UI (longarm-tray) that
// programs open with `longarm ui open` (docs/usage.md "Opening Longarm's
// UI"), and is how longarm asks the running tray to show one: the tray
// serves a local control channel (internal/localapi: a named pipe or unix
// socket only this user can reach; its address in <config dir>/tray.addr)
// with POST /ui/open.
//
// The page names are stable: a program may hard-code them (they keep
// their meaning; new pages get new names).
package uipage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/localapi"
)

// Pages are the names `longarm ui open` takes, in the order of its help.
var Pages = []string{"workers", "add-machine", "jobs", "this-worker", "settings", "account", "sessions"}

// Default is the page without a name.
const Default = "workers"

// Target says what a page shows (all optional): jobs takes Job; sessions
// takes Worker, Job, Session and Agent.
type Target struct {
	Worker  string `json:"worker,omitempty"`
	Job     string `json:"job,omitempty"`
	Session string `json:"session,omitempty"`
	Agent   string `json:"agent,omitempty"`
}

// Page is where a page is in the tray's web UI: its path with a query
// (the popup, /tray) and, for the Sessions window (/sessions), its
// location hash (without "#").
type Page struct {
	Path string
	Hash string
}

// SessionsPath is the Sessions window's page.
const SessionsPath = "/sessions"

// String is the path with the hash.
func (p Page) String() string {
	if p.Hash == "" {
		return p.Path
	}
	return p.Path + "#" + p.Hash
}

// UnknownPageError: a page name that isn't one of Pages.
type UnknownPageError struct{ Name string }

func (e *UnknownPageError) Error() string {
	return fmt.Sprintf("unknown page %q; pages: %s", e.Name, strings.Join(Pages, ", "))
}

// name: worker, job and session names (they go into URLs).
var name = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Resolve is the page called page ("" = Default) showing t; an error for
// an unknown page, a flag the page does not take or a malformed value.
func Resolve(page string, t Target) (Page, error) {
	if page == "" {
		page = Default
	}
	takes := map[string]string{} // the target fields the page takes
	tab := map[string]string{"workers": "pool", "add-machine": "pool", "jobs": "jobs", "this-worker": "worker", "settings": "settings", "account": "account"}
	switch page {
	case "jobs":
		takes["job"] = ""
	case "sessions":
		takes["worker"], takes["job"], takes["session"], takes["agent"] = "", "", "", ""
	default:
		if _, ok := tab[page]; !ok {
			return Page{}, &UnknownPageError{Name: page}
		}
	}
	for _, f := range [][2]string{{"worker", t.Worker}, {"job", t.Job}, {"session", t.Session}, {"agent", t.Agent}} {
		if f[1] == "" {
			continue
		}
		if _, ok := takes[f[0]]; !ok {
			return Page{}, fmt.Errorf("--%s is not for the %s page", f[0], page)
		}
		if f[0] == "agent" {
			if f[1] != "claude" && f[1] != "copilot" {
				return Page{}, fmt.Errorf("--agent: claude or copilot, not %q", f[1])
			}
		} else if !name.MatchString(f[1]) {
			return Page{}, fmt.Errorf("--%s: not a valid name: %q", f[0], f[1])
		}
	}
	q := url.Values{}
	if page == "sessions" {
		for _, kv := range [][2]string{{"worker", t.Worker}, {"agent", t.Agent}, {"job", t.Job}, {"session", t.Session}} {
			if kv[1] != "" {
				q.Set(kv[0], kv[1])
			}
		}
		return Page{Path: SessionsPath, Hash: q.Encode()}, nil
	}
	q.Set("tab", tab[page])
	if page == "add-machine" {
		q.Set("add", "1")
	}
	if t.Job != "" {
		q.Set("job", t.Job)
	}
	return Page{Path: "/tray?" + q.Encode()}, nil
}

// ---------------------------------------------------------------- the channel

// AddrFile is where the running tray records its control channel.
func AddrFile(configDir string) string { return filepath.Join(configDir, "tray.addr") }

// Request is POST /ui/open's body.
type Request struct {
	Page string `json:"page"`
	Target
	// PrintURL: do not open anything, answer a URL with a one-time code
	// (for a program's own webview).
	PrintURL bool `json:"print_url,omitempty"`
}

// Result is POST /ui/open's answer (and `longarm ui open --json`).
type Result struct {
	// URL is the page on the tray's loopback server: with PrintURL one
	// that works once, within 10 minutes (it signs that browser or
	// webview in); else the page's address (without a code).
	URL    string `json:"url"`
	Opened bool   `json:"opened"`
}

// ErrNotRunning: no tray answers on the channel.
var ErrNotRunning = errors.New("longarm-tray is not running")

// Open asks the tray whose channel is recorded in addrFile to open (or,
// PrintURL, mint a URL for) req's page. ErrNotRunning when no tray
// answers; a refusal (a bad page) is the tray's message.
func Open(ctx context.Context, addrFile string, req Request) (Result, error) {
	addr, err := localapi.ReadAddr(addrFile)
	if err != nil {
		return Result{}, ErrNotRunning
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://tray/ui/open", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	hr.Header.Set("Content-Type", "application/json")
	c := localapi.HTTPClient(addr)
	defer c.CloseIdleConnections()
	resp, err := c.Do(hr)
	if err != nil {
		return Result{}, fmt.Errorf("%w (%v)", ErrNotRunning, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return Result{}, errors.New(e.Error)
		}
		return Result{}, fmt.Errorf("longarm-tray: %s", resp.Status)
	}
	var res Result
	if err := json.Unmarshal(b, &res); err != nil {
		return Result{}, fmt.Errorf("longarm-tray: %v", err)
	}
	return res, nil
}
