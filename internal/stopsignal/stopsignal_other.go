//go:build !windows

package stopsignal

import "context"

// NotifyContext returns ctx, canceled as well when the installer asks this
// process to stop (Windows only; elsewhere just a cancelable ctx).
func NotifyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

// Signal asks process pid to stop; false when it has no stop event.
func Signal(pid int) (bool, error) { return false, nil }
