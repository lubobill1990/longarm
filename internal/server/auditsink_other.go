//go:build windows || plan9

package server

import "errors"

// No log/syslog here (Windows has no syslog daemon).
const localSyslogSupported = false

func dialLocalSyslog() (localSyslog, error) {
	return nil, errors.New("no local syslog on this OS")
}
