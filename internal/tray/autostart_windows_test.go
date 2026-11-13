package tray

import "testing"

// TestRunKeyAutostartReadsRegistry: the real registry reads (nothing is
// written): whatever this machine has, State answers without an error.
func TestRunKeyAutostartReadsRegistry(t *testing.T) {
	st, err := newAutostart().State()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("start at login here: %+v", st)
}
