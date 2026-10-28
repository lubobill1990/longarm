// longarm-msihelper is the MSI's upgrade helper (internal/msiupgrade). It
// is not installed: the MSI carries it in its Binary table, because it
// must run before the files of the installed version are replaced (and the
// installed longarmd of an older version knows nothing of these steps).
//
//	longarm-msihelper stop-tray   #LONGARM-MSI#usersid=...   (immediate, as the installing user)
//	longarm-msihelper stop        #LONGARM-MSI#usersid=...   (deferred, elevated: before the old version goes)
//	longarm-msihelper stop-remove #LONGARM-MSI#usersid=...   (deferred, elevated: uninstall)
//	longarm-msihelper rollback    #LONGARM-MSI#usersid=...   (rollback of stop)
//
// It takes no path or program from the command line: the install dir is
// the one the MSI recorded in HKLM (administrators only), checked to be
// writable by administrators only. Built with -H windowsgui (no console
// window flashes during the install); it exits 0 unless the data is bad.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/lubobill1990/longarm/internal/msiupgrade"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: longarm-msihelper <step> "+msiupgrade.DataMarker+"key=value|...")
		os.Exit(2)
	}
	step := os.Args[1]
	env := msiupgrade.WindowsEnv()
	var log io.Writer = os.Stdout
	if step != "stop-tray" { // elevated: the installer's log in ProgramData\Longarm
		if dir, err := env.StateDir(); err == nil {
			if f, err := os.OpenFile(filepath.Join(dir, "install-helper.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
				defer f.Close()
				log = f
			}
		}
	}
	env.Log = log
	fmt.Fprintf(log, "%s msihelper %s\n", time.Now().Format(time.RFC3339), step)
	err := msiupgrade.RunStep(step, msiupgrade.RawCommandLine(), env)
	if err != nil {
		fmt.Fprintf(log, "%s msihelper %s: error: %v\n", time.Now().Format(time.RFC3339), step, err)
		os.Exit(1)
	}
	fmt.Fprintf(log, "%s msihelper %s: ok\n", time.Now().Format(time.RFC3339), step)
}
