//go:build windows

package windows

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modiphlpapi             = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable = modiphlpapi.NewProc("GetExtendedTcpTable")
)

const (
	tcpTableOwnerPIDListener = 3
	tcpTableOwnerPIDAll      = 5
)

// tcpTable returns GetExtendedTcpTable(class) for one address family: the
// raw rows and their count.
func tcpTable(af uint32, class uintptr) ([]byte, int, error) {
	var size uint32
	procGetExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, uintptr(af), class, 0)
	if size == 0 {
		return nil, 0, nil
	}
	var buf []byte
	for tries := 0; ; tries++ {
		buf = make([]byte, size+4096)
		size = uint32(len(buf))
		r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0,
			uintptr(af), class, 0)
		if r == 0 {
			break
		}
		if r != uintptr(windows.ERROR_INSUFFICIENT_BUFFER) || tries > 3 {
			return nil, 0, fmt.Errorf("GetExtendedTcpTable: %w", windows.Errno(r))
		}
	}
	return buf, int(binary.LittleEndian.Uint32(buf)), nil
}

// Row layouts of MIB_TCPROW_OWNER_PID / MIB_TCP6ROW_OWNER_PID.
type rowLayout struct {
	size, localAddr, localPort, remoteAddr, remotePort, pid, addrLen int
}

var (
	row4 = rowLayout{size: 24, localAddr: 4, localPort: 8, remoteAddr: 12, remotePort: 16, pid: 20, addrLen: 4}
	row6 = rowLayout{size: 56, localAddr: 0, localPort: 20, remoteAddr: 24, remotePort: 44, pid: 52, addrLen: 16}
)

func layout(af uint32) rowLayout {
	if af == windows.AF_INET6 {
		return row6
	}
	return row4
}

// port of a row field: network byte order in the low 16 bits.
func rowPort(row []byte, off int) uint16 { return uint16(row[off])<<8 | uint16(row[off+1]) }

func rowAddr(row []byte, off, n int) netip.Addr {
	a, _ := netip.AddrFromSlice(row[off : off+n])
	return a.Unmap()
}

// ListenerPIDs returns the processes listening on TCP port (IPv4 and IPv6,
// any local address) - GetExtendedTcpTable(TCP_TABLE_OWNER_PID_LISTENER).
func ListenerPIDs(port int) ([]int, error) {
	var pids []int
	for _, af := range []uint32{windows.AF_INET, windows.AF_INET6} {
		buf, n, err := tcpTable(af, tcpTableOwnerPIDListener)
		if err != nil {
			return nil, err
		}
		l := layout(af)
		for i := 0; i < n; i++ {
			row := buf[4+i*l.size:]
			if len(row) < l.size {
				break
			}
			if int(rowPort(row, l.localPort)) == port {
				pids = append(pids, int(binary.LittleEndian.Uint32(row[l.pid:])))
			}
		}
	}
	return pids, nil
}

// ConnPID returns the process owning the TCP socket with local address
// local connected to remote (e.g. the client end of a loopback connection
// longarmd accepted: local = the client's address, remote = longarmd's).
func ConnPID(local, remote netip.AddrPort) (int, error) {
	af := uint32(windows.AF_INET)
	if local.Addr().Is6() {
		af = windows.AF_INET6
	}
	buf, n, err := tcpTable(af, tcpTableOwnerPIDAll)
	if err != nil {
		return 0, err
	}
	l := layout(af)
	for i := 0; i < n; i++ {
		row := buf[4+i*l.size:]
		if len(row) < l.size {
			break
		}
		if rowPort(row, l.localPort) != local.Port() || rowPort(row, l.remotePort) != remote.Port() {
			continue
		}
		if rowAddr(row, l.localAddr, l.addrLen) == local.Addr() && rowAddr(row, l.remoteAddr, l.addrLen) == remote.Addr() {
			return int(binary.LittleEndian.Uint32(row[l.pid:])), nil
		}
	}
	return 0, errors.New("the peer's socket is not in the TCP table")
}
