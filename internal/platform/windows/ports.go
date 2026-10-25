//go:build windows

package windows

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modiphlpapi             = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = modiphlpapi.NewProc("GetExtendedTcpTable")
)

const tcpTableOwnerPIDListener = 3

// ListenerPIDs returns the processes listening on TCP port (IPv4 and IPv6,
// any local address) - GetExtendedTcpTable(TCP_TABLE_OWNER_PID_LISTENER).
func ListenerPIDs(port int) ([]int, error) {
	var pids []int
	for _, af := range []uint32{windows.AF_INET, windows.AF_INET6} {
		var size uint32
		procGetExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, uintptr(af), tcpTableOwnerPIDListener, 0)
		if size == 0 {
			continue
		}
		var buf []byte
		for tries := 0; ; tries++ {
			buf = make([]byte, size+4096)
			size = uint32(len(buf))
			r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0,
				uintptr(af), tcpTableOwnerPIDListener, 0)
			if r == 0 {
				break
			}
			if r != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || tries > 3 {
				return nil, fmt.Errorf("GetExtendedTcpTable: %w", windows.Errno(r))
			}
		}
		n := int(binary.LittleEndian.Uint32(buf))
		rowSize, portOff, pidOff := 24, 8, 20 // MIB_TCPROW_OWNER_PID
		if af == windows.AF_INET6 {
			rowSize, portOff, pidOff = 56, 20, 52 // MIB_TCP6ROW_OWNER_PID
		}
		for i := 0; i < n; i++ {
			row := buf[4+i*rowSize:]
			if len(row) < rowSize {
				break
			}
			// the port is in network byte order in the low 16 bits
			p := int(row[portOff])<<8 | int(row[portOff+1])
			if p == port {
				pids = append(pids, int(binary.LittleEndian.Uint32(row[pidOff:])))
			}
		}
	}
	return pids, nil
}
