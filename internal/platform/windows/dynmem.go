//go:build windows

package windows

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A Hyper-V guest with Dynamic Memory sees only what the host has given it
// so far (GlobalMemoryStatusEx: e.g. 4 GB); the host grows it on demand up
// to a configured maximum. The guest's Dynamic Memory integration service
// publishes that maximum as a performance counter - the only in-guest
// source of it ("Physical Memory" / "Added Memory" are not present on every
// guest). PdhAddEnglishCounterW takes the English path whatever the
// display language.

const dynMemCounter = `\Hyper-V Dynamic Memory Integration Service\Maximum Memory, MBytes`

var (
	modpdh                          = windows.NewLazySystemDLL("pdh.dll")
	procPdhOpenQueryW               = modpdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglishCounterW       = modpdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQueryData         = modpdh.NewProc("PdhCollectQueryData")
	procPdhGetFormattedCounterValue = modpdh.NewProc("PdhGetFormattedCounterValue")
	procPdhCloseQuery               = modpdh.NewProc("PdhCloseQuery")
)

const (
	pdhFmtLarge         = 0x00000400
	pdhCstatusNoObject  = 0xC0000BB8
	pdhCstatusNoCounter = 0xC0000BB9
	pdhNoData           = 0x800007D5
)

// pdhFmtCounterValue is PDH_FMT_COUNTERVALUE with the LONGLONG member of
// its union (8-byte aligned on every architecture).
type pdhFmtCounterValue struct {
	CStatus uint32
	_       uint32
	Large   int64
}

// DynamicMemoryMax reads the Hyper-V Dynamic Memory ceiling in bytes. It is
// 0 without an error when the counter does not exist (not a Hyper-V guest,
// or static memory).
func DynamicMemoryMax() (uint64, error) {
	if err := modpdh.Load(); err != nil {
		return 0, err
	}
	var q windows.Handle
	if r, _, _ := procPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
		return 0, fmt.Errorf("PdhOpenQuery: 0x%08X", uint32(r))
	}
	defer procPdhCloseQuery.Call(uintptr(q))
	path, _ := windows.UTF16PtrFromString(dynMemCounter)
	var c windows.Handle
	r, _, _ := procPdhAddEnglishCounterW.Call(uintptr(q), uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&c)))
	switch uint32(r) {
	case 0:
	case pdhCstatusNoObject, pdhCstatusNoCounter:
		return 0, nil
	default:
		return 0, fmt.Errorf("PdhAddEnglishCounter: 0x%08X", uint32(r))
	}
	switch r, _, _ := procPdhCollectQueryData.Call(uintptr(q)); uint32(r) {
	case 0:
	case pdhNoData:
		return 0, nil
	default:
		return 0, fmt.Errorf("PdhCollectQueryData: 0x%08X", uint32(r))
	}
	var v pdhFmtCounterValue
	var typ uint32
	r, _, _ = procPdhGetFormattedCounterValue.Call(uintptr(c), pdhFmtLarge, uintptr(unsafe.Pointer(&typ)), uintptr(unsafe.Pointer(&v)))
	if r != 0 {
		return 0, fmt.Errorf("PdhGetFormattedCounterValue: 0x%08X", uint32(r))
	}
	if v.CStatus != 0 && v.CStatus != 1 { // PDH_CSTATUS_VALID_DATA / NEW_DATA
		return 0, nil
	}
	if v.Large <= 0 {
		return 0, nil
	}
	return uint64(v.Large) << 20, nil
}

// dynMemRefresh: the ceiling is a VM setting that rarely changes, and a
// PDH query costs more than the rest of a sample.
const dynMemRefresh = 5 * time.Minute

var dynMem struct {
	sync.Mutex
	v  uint64
	at time.Time
}

// cachedDynamicMemoryMax is DynamicMemoryMax, read at most every
// dynMemRefresh; an error counts as 0 (sampling goes on without it).
func cachedDynamicMemoryMax() uint64 {
	dynMem.Lock()
	defer dynMem.Unlock()
	if dynMem.at.IsZero() || time.Since(dynMem.at) >= dynMemRefresh {
		dynMem.v, _ = DynamicMemoryMax()
		dynMem.at = time.Now()
	}
	return dynMem.v
}
