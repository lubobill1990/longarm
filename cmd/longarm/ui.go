package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/uipage"
)

// `longarm ui open`: Longarm's own UI (longarm-tray) at a page, for people
// and for programs (TMIA Desktop) that link to it instead of re-building
// it (docs/usage.md "Opening Longarm's UI"; internal/uipage).

func uiCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ui",
		Short: "Longarm's own UI (the tray): ui open [page]",
		Args:  cobra.NoArgs,
	}
	c.AddCommand(uiOpenCmd())
	return c
}

// trayStartWait bounds how long ui open waits for a tray it started.
var trayStartWait = 30 * time.Second

func uiOpenCmd() *cobra.Command {
	var t uipage.Target
	var printURL, asJSON bool
	c := &cobra.Command{
		Use:   "open [page]",
		Short: "Open Longarm's UI at a page (starts longarm-tray if needed); --print-url for a program's own webview",
		Long: `Open longarm-tray's window at a page, starting longarm-tray when it is not
running. Pages (stable names):

  workers       the Workers tab (the default)
  add-machine   the Workers tab's "Add a machine", opened
  jobs          My jobs (--job ID: that job's output opened)
  this-worker   This worker
  settings      Settings
  account       Account
  sessions      the Sessions window (--worker W, --job ID, --session NAME, --agent claude|copilot)

--print-url opens nothing and prints a loopback URL with a one-time code
(valid once, for 10 minutes) for a program that shows the page in its own
webview. --json: {"url": "...", "opened": true|false}. An unknown page
exits 2 with the list.`,
		Example: `  longarm ui open
  longarm ui open add-machine
  longarm ui open sessions --worker worker-1 --job 01JZ8K
  longarm ui open jobs --print-url --json`,
		Args: cobra.MaximumNArgs(1),
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return uipage.Pages, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			req := uipage.Request{Page: uipage.Default, Target: t, PrintURL: printURL}
			if len(args) == 1 {
				req.Page = args[0]
			}
			if _, err := uipage.Resolve(req.Page, req.Target); err != nil {
				return usageErr(err)
			}
			res, err := openUI(cmd.Context(), configDir, req)
			if err != nil {
				return err
			}
			switch {
			case asJSON:
				e := json.NewEncoder(cmd.OutOrStdout())
				e.SetEscapeHTML(false) // the URL's & as is
				e.Encode(res)
			case printURL:
				fmt.Fprintln(cmd.OutOrStdout(), res.URL)
			}
			return nil
		},
	}
	c.Flags().StringVar(&t.Worker, "worker", "", "sessions: the worker's sessions")
	c.Flags().StringVar(&t.Job, "job", "", "jobs, sessions: this job (its id or the end of it)")
	c.Flags().StringVar(&t.Session, "session", "", "sessions: this Longarm session (its name)")
	c.Flags().StringVar(&t.Agent, "agent", "", "sessions: claude or copilot sessions only")
	c.Flags().BoolVar(&printURL, "print-url", false, "open nothing: print a URL with a one-time code (for a program's own webview)")
	c.Flags().BoolVar(&asJSON, "json", false, `print {"url": "...", "opened": bool}`)
	return c
}

// openUI asks the running tray to open req's page; with none running it
// starts longarm-tray and asks it once it answers.
func openUI(ctx context.Context, dir string, req uipage.Request) (uipage.Result, error) {
	addrFile := uipage.AddrFile(dir)
	res, err := uipage.Open(ctx, addrFile, req)
	if !errors.Is(err, uipage.ErrNotRunning) {
		return res, err
	}
	exe, err := trayExe()
	if err != nil {
		return uipage.Result{}, err
	}
	if _, err := platform.StartDetached(func() *exec.Cmd { return exec.Command(exe, "--config-dir", dir) }); err != nil {
		return uipage.Result{}, fmt.Errorf("starting %s: %w", exe, err)
	}
	deadline := time.Now().Add(trayStartWait)
	for {
		res, err := uipage.Open(ctx, addrFile, req)
		if !errors.Is(err, uipage.ErrNotRunning) {
			return res, err
		}
		if time.Now().After(deadline) {
			// a tray of a version without `ui open` keeps running and the
			// new one exits (one tray per user)
			return uipage.Result{}, fmt.Errorf("longarm-tray did not answer within %s; if an older one is running, quit it (tray menu: Close tray) and try again", trayStartWait)
		}
		select {
		case <-ctx.Done():
			return uipage.Result{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// trayExe is longarm-tray next to this longarm, else on PATH.
func trayExe() (string, error) {
	name := "longarm-tray"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if self, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(self), name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath(strings.TrimSuffix(name, ".exe")); err == nil {
		return p, nil
	}
	return "", notFoundf("longarm-tray is not installed here (not next to longarm, not on PATH): the UI comes with the desktop install")
}

type uiOpenIn struct {
	Page     string `json:"page,omitempty" jsonschema:"workers (default), add-machine, jobs, this-worker, settings, account or sessions"`
	Worker   string `json:"worker,omitempty" jsonschema:"sessions: the worker's sessions"`
	Job      string `json:"job,omitempty" jsonschema:"jobs, sessions: this job (its id or the end of it)"`
	Session  string `json:"session,omitempty" jsonschema:"sessions: this Longarm session (its name)"`
	Agent    string `json:"agent,omitempty" jsonschema:"sessions: claude or copilot sessions only"`
	PrintURL bool   `json:"print_url,omitempty" jsonschema:"open nothing; return a URL that works once (10 minutes)"`
}

// uiOpen is the MCP tool fleet_ui_open: `longarm ui open`.
func (t *mcpTools) uiOpen(ctx context.Context, _ *mcp.CallToolRequest, in uiOpenIn) (*mcp.CallToolResult, uipage.Result, error) {
	req := uipage.Request{Page: in.Page, PrintURL: in.PrintURL,
		Target: uipage.Target{Worker: in.Worker, Job: in.Job, Session: in.Session, Agent: in.Agent}}
	if _, err := uipage.Resolve(req.Page, req.Target); err != nil {
		return nil, uipage.Result{}, err
	}
	res, err := openUI(ctx, configDir, req)
	return nil, res, err
}
