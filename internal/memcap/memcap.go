// Package memcap is a worker's memory capacity as placement sees it - one
// definition for the hub's scheduler, the client-side pick and the MCP
// views.
//
// A Hyper-V guest with Dynamic Memory reports what the host has given it
// so far (mem_total_bytes, e.g. 4 GB) and the ceiling it can grow to
// (mem_max_bytes, e.g. 88 GB); counted against the 4 GB it would never
// pass a memory gate. So the ceiling counts as the total. Growing is best
// effort - an overcommitted host may not hand the memory out - but the
// CPU gate and the reservations of placed jobs still apply.
package memcap

import (
	"fmt"

	pb "longarm/proto/longarm/v1"
)

const gb = 1 << 30

// TotalBytes is the memory a worker can use: the Dynamic Memory ceiling
// when it is above what the worker has now (Dynamic), else its physical
// memory.
func TotalBytes(m *pb.MetricsSnapshot) uint64 {
	if Dynamic(m) {
		return m.GetMemMaxBytes()
	}
	return m.GetMemTotalBytes()
}

// GB returns a worker's total and free memory in GB (free = total - used,
// see TotalBytes).
func GB(m *pb.MetricsSnapshot) (totalGB, freeGB float64) {
	total := TotalBytes(m)
	return float64(total) / gb, float64(total-min(m.GetMemUsedBytes(), total)) / gb
}

// Dynamic reports whether the worker is a Dynamic Memory guest below its
// ceiling (displays add "(max N G)"). A guest at its ceiling - or with
// static memory - reports a ceiling a little above its physical memory
// (which leaves out what the firmware reserves): within 1/32 it counts as
// static.
func Dynamic(m *pb.MetricsSnapshot) bool {
	ceil, total := m.GetMemMaxBytes(), m.GetMemTotalBytes()
	return ceil > total && ceil-total > ceil/32
}

// MaxSuffix is " (max 88G)" for a Dynamic Memory guest below its ceiling,
// else "" - appended to a "used/total" memory column.
func MaxSuffix(m *pb.MetricsSnapshot) string {
	if !Dynamic(m) {
		return ""
	}
	return fmt.Sprintf(" (max %.0fG)", float64(m.GetMemMaxBytes())/gb)
}
