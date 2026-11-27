//go:build !windows

package screen

// Privacy mode and locking on Linux and macOS are to come
// (todo/remote-desktop.md).
func newCurtain(string) (Curtain, error) { return nil, ErrUnsupported }
func lockSession() error                 { return ErrUnsupported }
