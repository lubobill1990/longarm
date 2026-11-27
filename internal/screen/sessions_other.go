//go:build !windows

package screen

func sessions() ([]Session, error)  { return nil, ErrUnsupported }
func connectToConsole(uint32) error { return ErrUnsupported }

// ConsoleSession: Windows' notion (0 elsewhere).
func ConsoleSession() uint32 { return 0 }
