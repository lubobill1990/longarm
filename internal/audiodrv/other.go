//go:build !windows

package audiodrv

func install(string) (bool, error) { return false, ErrUnsupported }
func uninstall() error             { return ErrUnsupported }
func devices() ([]string, error)   { return nil, ErrUnsupported }
