//go:build windows

package desktop

import (
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
