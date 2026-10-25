//go:build !windows

package platform

import "os"

func euid() int { return os.Geteuid() }
