// Package core is what gomobile binds for the Longarm phone app
// (docs/mobile.md): Start runs the phone core (internal/mobile) and gives
// the URL the app's WebView opens.
//
//	gomobile bind -target=android -androidapi 24 -o longarm.aar ./core   (in mobile/)
//	gomobile bind -target=ios -o Longarm.xcframework ./core
package core

import (
	"sync"

	"longarm/internal/mobile"
	"longarm/internal/version"
)

var (
	mu   sync.Mutex
	core *mobile.Core
)

// Start runs the core with its config under filesDir (the app's private
// files dir) and returns the page's URL; started already: the same URL.
func Start(filesDir string) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	if core == nil {
		c, err := mobile.Start(mobile.ConfigDir(filesDir))
		if err != nil {
			return "", err
		}
		core = c
	}
	return core.URL(), nil
}

// Stop ends the core (the app is going away).
func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if core != nil {
		core.Stop()
		core = nil
	}
}

// Version is the core's Longarm version.
func Version() string { return version.Version }
