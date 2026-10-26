//go:build !windows

package longarmd

import (
	"os"
	"runtime"
)

func currentDataDirEnv() dataDirEnv {
	home, _ := os.UserHomeDir()
	return dataDirEnv{
		getenv: os.Getenv,
		home:   home,
		system: os.Geteuid() == 0,
		sysDir: systemDataDirFor(runtime.GOOS, os.Getenv, ""),
		inUse:  DataDirInUse,
	}
}
