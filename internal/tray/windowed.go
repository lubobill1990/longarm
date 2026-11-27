package tray

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// windowedGrace: how long RunWindowed waits for a command to fail early
// (a worker that can't be reached, no remote desktop there) before it
// counts it as started.
var windowedGrace = 3 * time.Second

// RunWindowed starts argv - a command that opens a window of its own, like
// longarm desktop - without a console or terminal window (CLAUDE.md "The
// tray"). A failure within windowedGrace comes back as the error, with the
// last line it wrote to stderr; after that the command runs on by itself.
func RunWindowed(argv []string) error {
	if len(argv) == 0 {
		return errors.New("empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	hideConsole(cmd)
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return nil
		}
		if line := lastLine(stderr.String()); line != "" {
			return errors.New(strings.TrimPrefix(line, "longarm: "))
		}
		return err
	case <-time.After(windowedGrace):
		return nil
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// lockedBuffer: the command writes while RunWindowed may read.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.b.Len() > 64<<10 {
		return len(p), nil // enough for the last line of an early failure
	}
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
