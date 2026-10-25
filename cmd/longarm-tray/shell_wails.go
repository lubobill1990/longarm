//go:build windows || (darwin && cgo) || (linux && cgo && traynative)

package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"github.com/lubobill1990/longarm/internal/tray"
)

// The Wails v3 shell: a system tray icon and a frameless, always-on-top
// popup window next to it (a floating panel): left click
// toggles it, it hides when it loses focus unless pinned, it is draggable
// by its title bar, and it stays where it was dragged to. The window shows
// the loopback server's /tray page (the server streams SSE, which Wails'
// asset server cannot on Windows).

type desktop struct {
	app  *application.App
	win  *application.WebviewWindow
	tray *application.SystemTray

	mu        sync.Mutex
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
	d.mu.Unlock()
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
	d.win.Show().Focus()
}

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

func runGUI(ctx context.Context, o tray.Options, startDaemon func() error) error {
	d := &desktop{}
	srv, err := tray.NewServer(o, d, "")
	if err != nil {
		return err
	}
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
	d.win = app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "popup", Title: "Remote Workers", Width: 460, Height: 780, MinWidth: 360, MinHeight: 300,
		Frameless: true, AlwaysOnTop: true, Hidden: true, URL: srv.URL("/tray"),
		BackgroundColour: application.NewRGB(13, 17, 23),
		Windows:          application.WindowsWindow{HiddenOnTaskbar: true},
	})
	d.win.OnWindowEvent(events.Common.WindowLostFocus, func(*application.WindowEvent) {
		// a short delay: focus can bounce while the window is being shown
		time.AfterFunc(150*time.Millisecond, func() {
			if !d.Pinned() && d.win.IsVisible() && !d.win.IsFocused() {
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
	menu.Add("Quit").OnClick(func(*application.Context) { app.Quit() })
	t.SetMenu(menu)

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctrl := tray.NewController(o, &wailsShell{tray: t, notes: notes}, startDaemon)
	go ctrl.Run(cctx)
	go func() {
		<-ctx.Done()
		app.Quit()
	}()
	return app.Run()
}
