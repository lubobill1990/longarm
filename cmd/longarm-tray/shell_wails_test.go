//go:build windows || (darwin && cgo) || (linux && cgo && traynative)

package main

import (
	"testing"

	"github.com/wailsapp/wails/v3/pkg/application"

	"github.com/lubobill1990/longarm/internal/tray"
)

// TestPopupIsAnApp: the popup's WebView2 settings make it an app, not a
// browser - no default context menu, no zoom, no file drops, devtools only
// in development builds (Wails also turns the browser accelerator keys,
// the status bar and swipe navigation off).
func TestPopupIsAnApp(t *testing.T) {
	o := popupOptions("http://127.0.0.1:1/tray", tray.PopupLight)
	if !o.DefaultContextMenuDisabled || o.ZoomControlEnabled || o.EnableFileDrop || o.DevToolsEnabled == releaseBuild {
		t.Fatalf("popup options %+v", o)
	}
	if o.BackgroundColour != application.NewRGB(0xfb, 0xfa, 0xf8) {
		t.Fatalf("background %+v: the light theme's", o.BackgroundColour)
	}
	if !o.AlwaysOnTop || !o.Frameless || !o.Hidden {
		t.Fatalf("popup window %+v", o)
	}
}

// TestPopupStaysDuringSignIn: the popup hides on losing the focus only
// with auto-hide on and while no sign-in runs.
func TestPopupStaysDuringSignIn(t *testing.T) {
	busy := false
	d := &desktop{busy: func() bool { return busy }, pinned: true}
	if d.autoHide() {
		t.Fatal("pinned (the default) hides")
	}
	d.pinned = false
	if !d.autoHide() {
		t.Fatal("auto-hide off")
	}
	busy = true
	if d.autoHide() {
		t.Fatal("hides during a sign-in")
	}
}
