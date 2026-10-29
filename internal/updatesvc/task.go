package updatesvc

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
)

// InstallTaskName is the one-shot scheduled task that runs msiexec for the
// service: started by the Task Scheduler as SYSTEM, it is nobody's child -
// not the service's (whose process the MSI stops) and not in its job - so
// the upgrade finishes while LongarmUpdate is stopped and replaced.
const InstallTaskName = `LongarmUpdate-Install`

// msiexecArgs: a silent install of msi with a verbose log, no reboot. The
// installed product's properties (the remembered HUB, ROLES, RUN, ...) come
// from the machine itself (HKLM, AppSearch); nothing else is passed.
func msiexecArgs(msi, log string) string {
	return fmt.Sprintf(`/i "%s" /qn /norestart /l*v "%s"`, msi, log)
}

// quotable: a path that can sit between double quotes on a command line.
func quotable(p string) bool {
	return p != "" && !strings.ContainsAny(p, "\"\r\n\x00%")
}

// installTaskXML is the task's definition (Task Scheduler schema 1.2):
// SYSTEM, highest privileges, no trigger (started on demand by the
// service), two actions run one after the other - msiexec, then "sc start
// LongarmUpdate" so the service is up again even when the install failed
// before the installer could restart it (rollbacks restart it as well; a
// running service ignores the second start).
func installTaskXML(msiexec, sc, msi, log string) (string, error) {
	for _, p := range []string{msiexec, sc, msi, log} {
		if !quotable(p) {
			return "", fmt.Errorf("unusable path %q", p)
		}
	}
	esc := func(s string) string {
		var b bytes.Buffer
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Longarm Update: installs one verified Longarm MSI (created by the LongarmUpdate service)</Description>
  </RegistrationInfo>
  <Principals>
    <Principal id="System">
      <UserId>S-1-5-18</UserId>
      <RunLevel>HighestAvailable</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <ExecutionTimeLimit>PT2H</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="System">
    <Exec>
      <Command>` + esc(msiexec) + `</Command>
      <Arguments>` + esc(msiexecArgs(msi, log)) + `</Arguments>
    </Exec>
    <Exec>
      <Command>` + esc(sc) + `</Command>
      <Arguments>start ` + ServiceName + `</Arguments>
    </Exec>
  </Actions>
</Task>
`, nil
}
