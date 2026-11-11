//go:build windows || (darwin && cgo) || (linux && cgo && traynative)

package main

import (
	"log/slog"
	"net/url"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/lubobill1990/longarm/internal/tray"
)

// runUpdateProgress is --update-progress: one small window following this
// machine's update to version (internal/tray/updateprogress.go) - the page
// and its API from the window's own asset handler, no loopback server - and
// gone when the update is in or the user closes it.
func runUpdateProgress(version, theme, dataDir string, log *slog.Logger) error {
	var app *application.App
	quit := func() { app.Quit() }
	bg, title := tray.PopupDark, application.Dark // the title bar too
	if theme == "light" {
		bg, title = tray.PopupLight, application.Light
	}
	app = application.New(application.Options{
		Name:   "longarm-update-progress",
		Logger: log,
		Assets: application.AssetOptions{Handler: tray.ProgressHandler(version, log, quit)},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "com.longarm.tray.update-progress",
		},
		// its own WebView2 profile: the tray it outlives has the default one
		Windows: application.WindowsOptions{WebviewUserDataPath: dataDir},
	})
	q := url.Values{"to": {version}, "theme": {theme}}
	// closing it quits (the app's last window)
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name: "update-progress", Title: "Updating Longarm", Width: 440, Height: 260,
		DisableResize: true, InitialPosition: application.WindowCentered,
		MaximiseButtonState: application.ButtonHidden,
		URL:                 "/update?" + q.Encode(), BackgroundColour: rgba(bg),
		DefaultContextMenuDisabled: true,
		ZoomControlEnabled:         false,
		EnableFileDrop:             false,
		DevToolsEnabled:            !releaseBuild,
		Windows:                    application.WindowsWindow{Theme: title},
	})
	return app.Run()
}
