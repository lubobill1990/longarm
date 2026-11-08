//go:build testhooks

package metrics

import (
	"os"
	"strconv"
)

// testHooks: built with the "testhooks" tag - only the e2e harness builds
// longarmd so (e2e/e2e_test.go, the per-user CI job).
const testHooks = true

// testMemMax: LONGARM_TEST_MEM_MAX_GB simulates a Hyper-V Dynamic Memory
// guest with that ceiling.
func testMemMax() (uint64, bool) {
	v, err := strconv.ParseUint(os.Getenv("LONGARM_TEST_MEM_MAX_GB"), 10, 64)
	return v << 30, err == nil
}
