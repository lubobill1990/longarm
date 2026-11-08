//go:build !testhooks

package metrics

// testHooks: every build without the "testhooks" tag - releases included -
// ignores LONGARM_TEST_MEM_MAX_GB.
const testHooks = false

func testMemMax() (uint64, bool) { return 0, false }
