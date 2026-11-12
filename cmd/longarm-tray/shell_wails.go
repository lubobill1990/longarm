//go:build windows || (darwin && cgo) || (linux && cgo && traynative)

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"longarm/internal/tray"
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
//
// A popup that shows nothing heals itself (heal): the page makes requests
// all the time (its pool stream), so a webview that makes none for a while
// lost it - WebView2 restarted its processes and came back blank, or on
// the URL whose one-time code is spent and without the session cookie
// (which lived in the old process). It gets the page again with a fresh
// code, then, if that doesn't bring it back, a new window.

type desktop struct {
	app  *application.App
	tray *application.SystemTray
	srv  *tray.Server
	log  *slog.Logger

	mu        sync.Mutex
	win       *application.WebviewWindow // the popup (replaced by heal)
	prefsDir  string
	busy      func() bool // a sign-in or a role change runs: never hide
	pinned    bool
	placed    bool      // dragged by the user: keep the position
	movedByUs time.Time // programmatic positioning (not a user drag)
	hiddenAt  time.Time
	loadedAt  time.Time // the popup was (re)created or sent to the page
	heals     int       // heal steps taken since the page was last alive
}

// pageGrace: how long a popup may be without a request of its page before
// it counts as blank (the page's stream reconnects within seconds).
const pageGrace = 60 * time.Second

func (d *desktop) popup() *application.WebviewWindow {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.win
}

// newPopup creates the popup window at url (hidden) with its handlers.
func (d *desktop) newPopup(url string) *application.WebviewWindow {
	w := d.app.Window.NewWithOptions(popupOptions(url, tray.CurrentPopupBackground(d.prefsDir)))
	w.OnWindowEvent(events.Common.WindowLostFocus, func(*application.WindowEvent) {
		// a short delay: focus can bounce while the window is being shown
		time.AfterFunc(150*time.Millisecond, func() {
			if d.popup() == w && d.autoHide() && w.IsVisible() && !w.IsFocused() {
				d.Hide()
			}
		})
	})
	w.OnWindowEvent(events.Common.WindowDidMove, func(*application.WindowEvent) {
		d.mu.Lock()
		if time.Since(d.movedByUs) > 500*time.Millisecond && w.IsVisible() {
			d.placed = true
		}
		d.mu.Unlock()
	})
	d.mu.Lock()
	d.win, d.loadedAt = w, time.Now()
	d.mu.Unlock()
	return w
}

// heal gets a blank popup its page back (see desktop): the page again with
// a fresh code, then a new window - one step per pageGrace, so a page that
// is just slow to load gets the time to.
func (d *desktop) heal(why string) {
	if d.srv.PageLive(pageGrace) {
		d.mu.Lock()
		d.heals = 0
		d.mu.Unlock()
		return
	}
	d.mu.Lock()
	if time.Since(d.loadedAt) < pageGrace {
		d.mu.Unlock()
		return
	}
	step := d.heals
	d.heals++
	old := d.win
	d.loadedAt = time.Now()
	d.mu.Unlock()
	url := d.srv.URL("/tray")
	if step%2 == 0 {
		d.log.Warn("tray popup: the page is not loaded; loading it again", "why", why, "attempt", step+1)
		old.SetURL(url)
		return
	}
	d.log.Warn("tray popup: still no page; replacing the window", "why", why, "attempt", step+1)
	visible := old.IsVisible()
	d.newPopup(url) // before closing the old one: the app never runs out of windows
	old.Close()
	if visible {
		d.show()
	}
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
	w := d.win
	d.mu.Unlock()
	w.Hide()
}

func (d *desktop) show() {
	d.heal("shown") // blank since: the page first
	d.mu.Lock()
	placed := d.placed
	if !placed {
		d.movedByUs = time.Now()
	}
	w := d.win
	d.mu.Unlock()
	if !placed {
		d.tray.PositionWindow(w, 8)
	}
	d.applyTheme() // the system's app mode may have changed meanwhile
	w.Show().Focus()
}

// applyTheme sets the window's background (what shows before and around
// the page) to the effective theme's: no dark flash for light themes.
func (d *desktop) applyTheme() {
	d.popup().SetBackgroundColour(rgba(tray.CurrentPopupBackground(d.prefsDir)))
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
	if d.popup().IsVisible() || recent {
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
		Name: "popup", Title: "Longarm", Width: 460, Height: 780, MinWidth: 360, MinHeight: 300,
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
	d := &desktop{prefsDir: o.LongarmConfigDir, pinned: !prefs.AutoHide, log: o.Log}
	srv, err := tray.NewServer(o, d, "")
	if err != nil {
		return err
	}
	d.srv, d.busy = srv, srv.Busy
	go srv.Serve()
	defer srv.Close()

	notes := notifications.New()
	app := application.New(application.Options{
		Name:        "longarm-tray",
		Description: "Longarm tray: worker pool, latency, jobs",
		Logger:      o.Log, // Wails' own (WebView2 failures and recovery) into tray.log
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
	// `longarm ui open`: the popup at a page (a fresh code: the page loads
	// anew at its tab)
	srv.ShowPopup = func(path string) {
		d.popup().SetURL(srv.URL(path))
		d.mu.Lock()
		d.loadedAt = time.Now()
		d.mu.Unlock()
		d.show()
	}
	d.newPopup(srv.URL("/tray"))

	t := app.SystemTray.New()
	d.tray = t
	t.SetIcon(tray.IconPNG(nil))
	t.SetTooltip("Longarm")
	t.OnClick(d.toggle)
	menu := app.NewMenu()
	menu.Add("Show panel").OnClick(func(*application.Context) { d.show() })
	signIn := menu.Add("Sign in…")
	signIn.OnClick(func(*application.Context) {
		srv.AutoSignIn() // when one is needed: the code at once
		d.show()
	})
	signIn.SetHidden(!srv.SignInNeeded()) // signed in: nothing to offer (another account: the Account tab)
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
		signIn.SetHidden(!srv.SignInNeeded())
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
	go func() { // a blank popup heals before it is opened
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				d.heal("no page requests")
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
	ctrl.Want = srv.WantDaemon
	go ctrl.Run(cctx)
	go srv.WatchSignIn(cctx, shell.Notify)
	go func() {
		if err := srv.ServeControl(cctx); err != nil {
			o.Log.Warn("longarm ui open: no control channel", "err", err)
		}
	}()
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
