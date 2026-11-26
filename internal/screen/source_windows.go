//go:build windows

package screen

import (
	"time"

	"golang.org/x/sys/windows"
)

type winSource struct{ c *Capturer }

func openSource(display int) (Source, error) {
	c, err := Open(display)
	if err != nil {
		return nil, err
	}
	return &winSource{c}, nil
}

func (s *winSource) Display() Display { return s.c.Display }
func (s *winSource) Size() (int, int) { return s.c.W, s.c.H }
func (s *winSource) Frame() []byte    { return s.c.Frame() }
func (s *winSource) Close()           { s.c.Close() }

func (s *winSource) Cursor() Cursor {
	cur := s.c.cursor
	s.c.cursor.BGRA = nil // handed out once
	return cur
}

func (s *winSource) Next(timeout time.Duration) (bool, error) {
	d, err := s.c.Next(int(timeout / time.Millisecond))
	return d != nil, err
}

// InServiceSession: this process runs in session 0 (a Windows service),
// which has no desktop to show.
func InServiceSession() bool {
	var sid uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sid); err != nil {
		return false
	}
	return sid == 0
}

func available() string {
	if InServiceSession() {
		return "longarmd runs as a Windows service, which has no desktop: the Longarm Desktop service shows it"
	}
	ds, err := Displays()
	if err != nil {
		return "no display: " + err.Error()
	}
	if len(ds) == 0 {
		return "no display attached to the desktop"
	}
	return ""
}
