//go:build !windows && !(darwin && cgo) && !(linux && cgo && traynative)

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lubobill1990/longarm/internal/tray"
)

// runGUI without the native shell (Linux without -tags traynative, macOS
// cross builds): Wails needs cgo (+ GTK/WebKitGTK on Linux), so this build
// serves the UI and opens it in the default browser. Linux: build with
// CGO_ENABLED=1 -tags traynative for the tray icon; macOS: natively with cgo
// (see README). The tag keeps `go build/test ./...` working on Linux machines
// that have gcc but not the GTK dev packages.
func runGUI(ctx context.Context, o tray.Options, startDaemon func() error) error {
	fmt.Fprintln(os.Stderr, "longarm-tray: built without the native tray (Linux: CGO_ENABLED=1 -tags traynative): opening the panel in the browser")
	return serveOnly(ctx, o, "", true, startDaemon)
}
