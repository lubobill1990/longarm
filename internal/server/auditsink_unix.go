//go:build !windows && !plan9

package server

import "log/syslog"

const localSyslogSupported = true

func dialLocalSyslog() (localSyslog, error) {
	return syslog.New(syslog.LOG_AUTH|syslog.LOG_INFO, "longarmd")
}
