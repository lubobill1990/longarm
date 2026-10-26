package diag

import (
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func osInfo() map[string]string {
	m := map[string]string{}
	v := windows.RtlGetVersion()
	m["kernel"] = fmt.Sprintf("%d.%d.%d", v.MajorVersion, v.MinorVersion, v.BuildNumber)
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return m
	}
	defer k.Close()
	name, _, _ := k.GetStringValue("ProductName")
	disp, _, _ := k.GetStringValue("DisplayVersion")
	ubr, _, err := k.GetIntegerValue("UBR")
	if v.BuildNumber >= 22000 {
		name = replaceFirst(name, "Windows 10", "Windows 11") // ProductName still says 10
	}
	m["os_name"] = name
	if disp != "" {
		m["os_name"] += " " + disp
	}
	if err == nil {
		m["kernel"] += fmt.Sprintf(".%d", ubr)
	}
	return m
}

func replaceFirst(s, old, new string) string {
	if len(s) >= len(old) && s[:len(old)] == old {
		return new + s[len(old):]
	}
	return s
}
