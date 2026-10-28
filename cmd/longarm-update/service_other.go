//go:build !windows

package main

import "errors"

func runService() error {
	return errors.New("the Longarm Update service runs on Windows (MSI installs) only")
}
