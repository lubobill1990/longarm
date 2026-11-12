package memcap

import (
	"testing"

	pb "longarm/proto/longarm/v1"
)

func TestGB(t *testing.T) {
	const g = 1 << 30
	for _, c := range []struct {
		name              string
		total, used, max  uint64
		wantTotal, wantFr float64
		dynamic           bool
	}{
		{"static", 64 * g, 16 * g, 0, 64, 48, false},
		{"dynamic", 4 * g, 2 * g, 88 * g, 88, 86, true},
		{"used above total", 8 * g, 10 * g, 0, 8, 0, false},
		{"used above max", 4 * g, 90 * g, 88 * g, 88, 0, true},
		{"max below total", 16 * g, 4 * g, 8 * g, 16, 12, false},
		{"max equals total", 108 * g, 8 * g, 108 * g, 108, 100, false},
		{"at the ceiling", 115962957824, 8 * g, 115964116992, 115962957824.0 / g, 115962957824.0/g - 8, false},
	} {
		m := &pb.MetricsSnapshot{MemTotalBytes: c.total, MemUsedBytes: c.used, MemMaxBytes: c.max}
		total, free := GB(m)
		if total != c.wantTotal || free != c.wantFr || Dynamic(m) != c.dynamic {
			t.Errorf("%s: total %v free %v dynamic %v, want %v %v %v", c.name, total, free, Dynamic(m), c.wantTotal, c.wantFr, c.dynamic)
		}
	}
	if total, free := GB(nil); total != 0 || free != 0 || Dynamic(nil) || TotalBytes(nil) != 0 {
		t.Errorf("nil: %v %v", total, free)
	}
}

func TestMaxSuffix(t *testing.T) {
	if s := MaxSuffix(&pb.MetricsSnapshot{MemTotalBytes: 4 << 30, MemMaxBytes: 90112 << 20}); s != " (max 88G)" {
		t.Errorf("dynamic: %q", s)
	}
	if s := MaxSuffix(&pb.MetricsSnapshot{MemTotalBytes: 64 << 30}); s != "" {
		t.Errorf("static: %q", s)
	}
}
