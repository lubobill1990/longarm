//go:build windows || (darwin && cgo) || (linux && cgo && traynative)

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"github.com/lubobill1990/longarm/internal/tray"
)

// The Wails v3 shell: a system tray icon and a frameless, always-on-top
// popup window next to it (a floating panel): left click toggles it, it is
// draggable by its title bar, and it stays where it was dragged to. It
// stays open until closed (pinned, the default); with auto-hide (the pin
// button off, remembered in tray-window.json) it hides when it loses the
// focus - but never while a sign-in or a role change runs (the browser
// takes the focus to show the /link page; the code must stay visible). The
// window shows the loopback server's /tray page (the server streams SSE,
// which Wails' asset server cannot on Windows).
//
// It behaves as an app, not a browser: no default context menu (the page
// has its own Copy / Paste menu for text), no browser accelerator keys
// (reload, print, find, zoom, devtools - Wails turns them off; editing keys
// stay), no zoom, no status bar, no file drops; devtools only in
// development builds (release builds use the "production" tag); links
// open in the system browser (the page sends them to /trayapi/open).

type desktop struct {
	app  *application.App
	win  *application.WebviewWindow
	tray *application.SystemTray

	mu        sync.Mutex
	prefsDir  string
	busy      func() bool // a sign-in or a role change runs: never hide
	pinned    bool
	placed    bool      // dragged by the user: keep the position
	movedByUs time.Time // programmatic positioning (not a user drag)
	hiddenAt  time.Time
}

func (d *desktop) Pinned() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pinned
}

func (d *desktop) SetPinned(p bool) {
	d.mu.Lock()
	d.pinned = p
	dir := d.prefsDir
	d.mu.Unlock()
	if dir != "" {
		tray.SaveWindowPrefs(dir, tray.WindowPrefs{AutoHide: !p})
	}
}

// autoHide: the popup hides on losing the focus (the user's choice, and no
// sign-in in progress).
func (d *desktop) autoHide() bool {
	return !d.Pinned() && (d.busy == nil || !d.busy())
}

func (d *desktop) Hide() {
	d.mu.Lock()
	d.hiddenAt = time.Now()
	d.mu.Unlock()
	d.win.Hide()
}

func (d *desktop) show() {
	d.mu.Lock()
	placed := d.placed
	if !placed {
		d.movedByUs = time.Now()
	}
	d.mu.Unlock()
	if !placed {
		d.tray.PositionWindow(d.win, 8)
	}
	d.applyTheme() // the system's app mode may have changed meanwhile
	d.win.Show().Focus()
}

// applyTheme sets the window's background (what shows before and around
// the page) to the effective theme's: no dark flash for light themes.
func (d *desktop) applyTheme() {
	d.win.SetBackgroundColour(rgba(tray.CurrentPopupBackground(d.prefsDir)))
}

// ThemeChanged: Settings > Appearance changed (tray.ThemeListener).
func (d *desktop) ThemeChanged() { d.applyTheme() }

func rgba(c tray.RGB) application.RGBA { return application.NewRGB(c.R, c.G, c.B) }

// toggle is the tray icon's left click. Clicking the icon while the popup
// is open first takes the focus away (which hides it): treat a click right
// after that as "close".
func (d *desktop) toggle() {
	d.mu.Lock()
	recent := time.Since(d.hiddenAt) < 400*time.Millisecond
	d.mu.Unlock()
	if d.win.IsVisible() || recent {
		d.Hide()
		return
	}
	d.show()
}

// wailsShell implements tray.Shell.
type wailsShell struct {
	tray  *application.SystemTray
	notes *notifications.NotificationService
	seq   int
	mu    sync.Mutex
}

func (s *wailsShell) SetIcon(png []byte)     { s.tray.SetIcon(png) }
func (s *wailsShell) SetTooltip(text string) { s.tray.SetTooltip(text) }
func (s *wailsShell) Notify(title, body string) {
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("longarm-tray-%d", s.seq)
	s.mu.Unlock()
	if err := s.notes.SendNotification(notifications.NotificationOptions{ID: id, Title: title, Body: body}); err != nil {
		fmt.Println("notification:", title, body, err)
	}
}

// popupOptions: the popup window (and its WebView2 settings: an app, not a
// browser) with its background (the effective theme's) set before the page
// loads.
func popupOptions(url string, bg tray.RGB) application.WebviewWindowOptions {
	return application.WebviewWindowOptions{
		Name: "popup", Title: "Remote Workers", Width: 460, Height: 780, MinWidth: 360, MinHeight: 300,
		Frameless: true, AlwaysOnTop: true, Hidden: true, URL: url,
		BackgroundColour: rgba(bg),
		Windows:          application.WindowsWindow{HiddenOnTaskbar: true},
		// the page's own menu (Copy / Paste) only; no zoom, no file drops;
		// devtools follow the build (on without the production tag)
		DefaultContextMenuDisabled: true,
		ZoomControlEnabled:         false,
		EnableFileDrop:             false,
		DevToolsEnabled:            !releaseBuild,
	}
}

// sessionsOptions: the Sessions window (docs/agent-sessions-ui.md section
// 5) - a normal, resizable app window (no zoom, no file drops, no default
// context menu, like the popup).
func sessionsOptions(url string, bg tray.RGB) application.WebviewWindowOptions {
	return application.WebviewWindowOptions{
		Name: "sessions", Title: "Longarm Sessions", Width: 1200, Height: 760, MinWidth: 640, MinHeight: 400,
		URL: url, BackgroundColour: rgba(bg),
		DefaultContextMenuDisabled: true,
		ZoomControlEnabled:         false,
		EnableFileDrop:             false,
		DevToolsEnabled:            !releaseBuild,
	}
}

// sessionsWindow is the Sessions window: created when first opened;
// closing it hides it (re-opening is instant), quitting the tray closes it.
type sessionsWindow struct {
	app      *application.App
	srv      *tray.Server
	prefsDir string

	mu  sync.Mutex
	win *application.WebviewWindow
}

// open shows the window at the location hash ("": as it is) and focuses
// it; an open window only gets the new hash (the page follows hashchange).
func (s *sessionsWindow) open(hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.win == nil:
		s.win = s.app.Window.NewWithOptions(sessionsOptions(s.srv.SessionsURL(hash, true), tray.CurrentPopupBackground(s.prefsDir)))
		s.win.RegisterHook(events.Common.WindowClosing, s.hideOnClose)
	case hash != "":
		js, _ := json.Marshal(hash) // a JS string literal
		s.win.ExecJS("location.hash = " + string(js))
	}
	if s.win.IsMinimised() {
		s.win.Restore()
	}
	s.win.Show().Focus()
}

// hideOnClose: the close button hides the window, unless the tray quits.
func (s *sessionsWindow) hideOnClose(e *application.WindowEvent) {
	if s.app.Context().Err() != nil {
		return
	}
	e.Cancel()
	s.win.Hide()
}

// pauseLabel is the menu item that pauses (or, paused, resumes) this
// machine's worker.
func pauseLabel(paused bool) string {
	if paused {
		return "Resume this machine"
	}
	return "Pause this machine (no new jobs)"
}

func runGUI(ctx context.Context, o tray.Options, startDaemon func() error) error {
	prefs := tray.LoadWindowPrefs(o.LongarmConfigDir)
	d := &desktop{prefsDir: o.LongarmConfigDir, pinned: !prefs.AutoHide}
	srv, err := tray.NewServer(o, d, "")
	if err != nil {
		return err
	}
	d.busy = srv.Busy
	go srv.Serve()
	defer srv.Close()

	notes := notifications.New()
	app := application.New(application.Options{
		Name:        "longarm-tray",
		Description: "Longarm tray: worker pool, latency, jobs",
		Assets:      application.AssetOptions{Handler: http.NotFoundHandler()},
		Services:    []application.Service{application.NewService(notes)},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID:               "com.longarm.tray",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) { d.show() },
		},
		Mac: application.MacOptions{ActivationPolicy: application.ActivationPolicyAccessory},
	})
	d.app = app
	sessions := &sessionsWindow{app: app, srv: srv, prefsDir: o.LongarmConfigDir}
	srv.OpenSessions = sessions.open
	d.win = app.Window.NewWithOptions(popupOptions(srv.URL("/tray"), tray.CurrentPopupBackground(o.LongarmConfigDir)))
	d.win.OnWindowEvent(events.Common.WindowLostFocus, func(*application.WindowEvent) {
		// a short delay: focus can bounce while the window is being shown
		time.AfterFunc(150*time.Millisecond, func() {
			if d.autoHide() && d.win.IsVisible() && !d.win.IsFocused() {
				d.Hide()
			}
		})
	})
	d.win.OnWindowEvent(events.Common.WindowDidMove, func(*application.WindowEvent) {
		d.mu.Lock()
		if time.Since(d.movedByUs) > 500*time.Millisecond && d.win.IsVisible() {
			d.placed = true
		}
		d.mu.Unlock()
	})

	t := app.SystemTray.New()
	d.tray = t
	t.SetIcon(tray.IconPNG(nil))
	t.SetTooltip("Remote Workers")
	t.OnClick(d.toggle)
	menu := app.NewMenu()
	menu.Add("Show panel").OnClick(func(*application.Context) { d.show() })
	menu.Add("Sign in…").OnClick(func(*application.Context) {
		srv.AutoSignIn() // when one is needed: the code at once
		d.show()
	})
	menu.Add("Open hub panel").OnClick(func(*application.Context) {
		if u := tray.HubURL(o); u != "" {
			tray.OpenURL(u)
		}
	})
	menu.Add("Open in browser").OnClick(func(*application.Context) { tray.OpenURL(srv.URL("/tray")) })
	menu.Add("Reset position").OnClick(func(*application.Context) {
		d.mu.Lock()
		d.placed = false
		d.mu.Unlock()
		d.show()
	})
	menu.AddSeparator()
	// Closing the tray leaves this machine working (its worker, your
	// forwards): the menu says so, and offers pausing the worker or quitting
	// Longarm here (internal/tray/machine.go).
	pause := menu.Add(pauseLabel(false))
	pause.SetHidden(true) // until the local worker answers
	menu.AddSeparator()
	menu.Add("Close tray (keeps working)").OnClick(func(*application.Context) { app.Quit() })
	quitAll := menu.Add("Quit Longarm on this machine…")
	t.SetMenu(menu)

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	shell := &wailsShell{tray: t, notes: notes}
	refresh := func() tray.MachineState {
		st := srv.MachineState(cctx)
		pause.SetHidden(!st.Worker)
		pause.SetLabel(pauseLabel(st.Draining))
		menu.Update()
		return st
	}
	go func() {
		for {
			refresh()
			select {
			case <-cctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
	}()
	pause.OnClick(func(*application.Context) {
		go func() {
			st := srv.MachineState(cctx)
			if err := srv.PauseWorker(cctx, !st.Draining); err != nil {
				shell.Notify("Longarm", "Could not "+strings.ToLower(pauseLabel(st.Draining))+": "+err.Error())
			} else if st.Draining {
				shell.Notify("Longarm", "This machine takes new jobs again.")
			} else {
				shell.Notify("Longarm", "Paused: this machine takes no new jobs. Running jobs and sessions continue.")
			}
			refresh()
		}()
	})
	quitAll.OnClick(func(*application.Context) {
		st := srv.MachineState(cctx)
		dlg := app.Dialog.Question().SetTitle("Quit Longarm on this machine?").SetMessage(tray.QuitSummary(st))
		yes := dlg.AddButton("Quit Longarm")
		no := dlg.AddButton("Cancel")
		dlg.SetDefaultButton(no)
		dlg.SetCancelButton(no)
		yes.OnClick(func() {
			go func() {
				if problems := srv.QuitLongarm(st); len(problems) > 0 {
					shell.Notify("Longarm", "Quit, but: "+strings.Join(problems, "; "))
				}
				app.Quit()
			}()
		})
		dlg.Show()
	})
	ctrl := tray.NewController(o, shell, startDaemon)
	go ctrl.Run(cctx)
	go srv.WatchSignIn(cctx, shell.Notify)
	// a sign-in's code: the popup up (it stays: Busy), the code on the
	// clipboard, and a notification with it (when the popup is hidden
	// behind something after all)
	srv.OnSignInCode = func(c tray.SignInCode) {
		d.show()
		copied := app.Clipboard.SetText(c.Code)
		body := "Enter it at " + c.URL + " (opened in your browser)."
		if copied {
			body = "Copied to the clipboard. " + body
		}
		shell.Notify("Longarm sign-in code: "+c.Code, body)
	}
	go func() {
		<-ctx.Done()
		app.Quit()
	}()
	// started by the installer (--signin), or the first start of this
	// version: a sign-in that is needed starts by itself (the code shows at
	// once); without a known hub the popup asks for one
	first := tray.FirstStart(o.LongarmConfigDir, o.Version)
	if (o.ShowSignIn || first) && srv.NeedsSignIn() {
		time.AfterFunc(1500*time.Millisecond, func() {
			srv.AutoSignIn()
			d.show()
		})
	}
	return app.Run()
}
