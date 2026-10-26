//go:build !windows

package diag

import (
	"bufio"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func osInfo() map[string]string {
	m := map[string]string{}
	var u unix.Utsname
	if unix.Uname(&u) == nil {
		m["kernel"] = unix.ByteSliceToString(u.Sysname[:]) + " " + unix.ByteSliceToString(u.Release[:])
	}
	if f, err := os.Open("/etc/os-release"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
				m["os_name"] = strings.Trim(v, `"`)
			}
		}
	}
	return m
}
