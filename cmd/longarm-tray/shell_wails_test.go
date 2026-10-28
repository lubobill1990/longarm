//go:build windows || (darwin && cgo) || (linux && cgo && traynative)

package main

import "testing"

// TestPopupIsAnApp: the popup's WebView2 settings make it an app, not a
// browser - no default context menu, no zoom, no file drops, devtools only
// in development builds (Wails also turns the browser accelerator keys,
// the status bar and swipe navigation off).
func TestPopupIsAnApp(t *testing.T) {
	o := popupOptions("http://127.0.0.1:1/tray")
	if !o.DefaultContextMenuDisabled || o.ZoomControlEnabled || o.EnableFileDrop || o.DevToolsEnabled == releaseBuild {
		t.Fatalf("popup options %+v", o)
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
