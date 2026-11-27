//go:build !windows

package screen

// openClipboard: Linux (X11 selections) and macOS (NSPasteboard) are to
// come (todo/remote-desktop.md).
func openClipboard() (Clipboard, error) { return nil, ErrUnsupported }
