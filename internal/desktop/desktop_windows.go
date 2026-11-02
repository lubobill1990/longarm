//go:build windows

package desktop

import (
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows/registry"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

const (
	tsKey     = `SYSTEM\CurrentControlSet\Control\Terminal Server`
	rdpTCPKey = `SYSTEM\CurrentControlSet\Control\Terminal Server\WinStations\RDP-Tcp`
	// w365Key exists on Windows 365 Cloud PCs, Dev Boxes included.
	w365Key = `SOFTWARE\Microsoft\Windows365`
	// avdKey: the Azure Virtual Desktop agent's boot loader (Dev Boxes,
	// Cloud PCs, AVD session hosts).
	avdKey = `SOFTWARE\Microsoft\RDAgentBootLoader`
)

func keyExists(path string) bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	k.Close()
	return true
}

// mayBeCloudVM: only Cloud PCs and AVD session hosts ask the metadata
// service (other machines would wait for a link-local address for nothing).
func mayBeCloudVM() bool { return keyExists(w365Key) || keyExists(avdKey) }

func localInfo() *pb.DesktopInfo {
	if keyExists(w365Key) {
		return &pb.DesktopInfo{Kind: KindCloudPC}
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, tsKey, registry.QUERY_VALUE)
	if err != nil {
		return &pb.DesktopInfo{Reason: "Remote Desktop is not available on this edition of Windows"}
	}
	deny, _, err := k.GetIntegerValue("fDenyTSConnections")
	k.Close()
	if err != nil || deny != 0 {
		return &pb.DesktopInfo{Reason: "Remote Desktop is off (Settings > System > Remote Desktop)"}
	}
	port := uint64(3389)
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, rdpTCPKey, registry.QUERY_VALUE); err == nil {
		if p, _, err := k.GetIntegerValue("PortNumber"); err == nil && p > 0 && p < 65536 {
			port = p
		}
		k.Close()
	}
	return &pb.DesktopInfo{Kind: KindRDP, RdpPort: int32(port)}
}

// readDevBoxFacts: Windows' install time (CurrentVersion InstallDate, Unix
// seconds) and the last remote desktop logon (21) or reconnect (25) since
// then, from the LocalSessionManager log.
func readDevBoxFacts() devBoxFacts {
	var f devBoxFacts
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE); err == nil {
		if v, _, err := k.GetIntegerValue("InstallDate"); err == nil && v > 0 {
			f.installed = time.Unix(int64(v), 0).UTC()
		}
		k.Close()
	}
	cmd := exec.Command("wevtutil", "qe", "Microsoft-Windows-TerminalServices-LocalSessionManager/Operational",
		"/q:*[System[(EventID=21 or EventID=25)]]", "/c:200", "/rd:true", "/f:xml")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.Output(); err == nil && !f.installed.IsZero() {
		f.logons = RemoteLogons(string(out), f.installed)
		if len(f.logons) > 0 {
			f.lastLogon = f.logons[0]
		}
	}
	return f
}
