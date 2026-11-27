//go:build !windows

package vdisplay

func enable(Mode) (string, error) { return "", ErrUnsupported }
func disable() error              { return ErrUnsupported }
func remove() error               { return ErrUnsupported }
func supported() bool             { return false }
