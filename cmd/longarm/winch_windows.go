//go:build windows

package main

import (
	"context"
	"time"
)

// watchResize calls fn periodically; the caller sends a resize only when
// the console size actually changed (Windows has no SIGWINCH).
func watchResize(ctx context.Context, fn func()) {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}
