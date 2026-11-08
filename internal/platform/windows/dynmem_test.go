//go:build windows

package windows

import "testing"

// TestDynamicMemoryMax: 0 outside a Dynamic Memory guest, a sane ceiling
// inside one - never an error either way.
func TestDynamicMemoryMax(t *testing.T) {
	v, err := DynamicMemoryMax()
	if err != nil {
		t.Fatal(err)
	}
	if v != 0 && (v < 512<<20 || v > 1<<50) {
		t.Fatalf("implausible ceiling %d", v)
	}
	if c := cachedDynamicMemoryMax(); c != v {
		t.Fatalf("cached %d, read %d", c, v)
	}
	t.Logf("dynamic memory ceiling: %d MB", v>>20)
}
