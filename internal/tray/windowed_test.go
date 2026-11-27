package tray

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestRunWindowed: an early failure comes back with its stderr's last line;
// a command that keeps running is started.
func TestRunWindowed(t *testing.T) {
	if mode := os.Getenv("LONGARM_TRAY_WINDOWED_TEST"); mode != "" {
		if mode == "fail" {
			fmt.Fprintln(os.Stderr, "longarm: w1 can't show its desktop: no display")
			os.Exit(1)
		}
		time.Sleep(10 * time.Second)
		os.Exit(0)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LONGARM_TRAY_WINDOWED_TEST", "fail")
	if err := RunWindowed([]string{self, "-test.run=^TestRunWindowed$"}); err == nil || err.Error() != "w1 can't show its desktop: no display" {
		t.Fatalf("early failure: %v", err)
	}
	old := windowedGrace
	windowedGrace = 500 * time.Millisecond
	defer func() { windowedGrace = old }()
	t.Setenv("LONGARM_TRAY_WINDOWED_TEST", "run")
	if err := RunWindowed([]string{self, "-test.run=^TestRunWindowed$"}); err != nil {
		t.Fatalf("a command that runs on: %v", err)
	}
	if err := RunWindowed(nil); err == nil {
		t.Fatal("an empty command started")
	}
}
