//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// watchResize calls fn whenever the terminal is resized (SIGWINCH).
func watchResize(ctx context.Context, fn func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			fn()
		}
	}
}
