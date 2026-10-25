//go:build windows

package windows

import (
	"fmt"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
)

// kernel32 directly, which is
// locale-independent and far cheaper than CIM / performance counters.

var (
	modkernel32              = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes       = modkernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = modkernel32.NewProc("GlobalMemoryStatusEx")
	procK32GetProcessMemInfo = modkernel32.NewProc("K32GetProcessMemoryInfo")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

func ft(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

// SampleSystem reads GetSystemTimes + GlobalMemoryStatusEx. Kernel time
// includes idle time, so total = kernel + user.
func SampleSystem() (ptypes.SysSample, error) {
	var idle, kern, user windows.Filetime
	r, _, e := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kern)), uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return ptypes.SysSample{}, fmt.Errorf("GetSystemTimes: %w", e)
	}
	m := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	r, _, e = procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return ptypes.SysSample{}, fmt.Errorf("GlobalMemoryStatusEx: %w", e)
	}
	return ptypes.SysSample{
		IdleTicks:  ft(idle),
		TotalTicks: ft(kern) + ft(user),
		NumCPU:     runtime.NumCPU(),
		MemTotal:   m.TotalPhys,
		MemAvail:   m.AvailPhys,
		Uptime:     windows.DurationSinceBoot(),
	}, nil
}

// ListProcesses enumerates processes with a Toolhelp snapshot and fills CPU
// time / working set where the process can be opened with
// PROCESS_QUERY_LIMITED_INFORMATION (protected/system processes stay at 0,
// like Get-Process run unelevated).
func ListProcesses() ([]ptypes.ProcSample, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	out := make([]ptypes.ProcSample, 0, 512)
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		name := windows.UTF16ToString(pe.ExeFile[:])
		if len(name) > 4 && strings.EqualFold(name[len(name)-4:], ".exe") {
			name = name[:len(name)-4]
		}
		p := ptypes.ProcSample{PID: int(pe.ProcessID), PPID: int(pe.ParentProcessID), Name: name}
		if pe.ProcessID != 0 {
			fillProc(&p)
		}
		out = append(out, p)
	}
	return out, nil
}

func fillProc(p *ptypes.ProcSample) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(p.PID))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	var c, e, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &e, &k, &u) == nil {
		p.StartTime = ft(c)
		p.CPUTime = time.Duration(ft(k)+ft(u)) * 100 // 100 ns ticks
	}
	pmc := processMemoryCounters{CB: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	if r, _, _ := procK32GetProcessMemInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.CB)); r != 0 {
		p.RSS = uint64(pmc.WorkingSetSize)
	}
}
