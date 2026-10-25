//go:build linux || darwin

package platform

import (
	"fmt"
	"os"
)

// secureDirNoLinks refuses an existing dir that is a symlink (a user must
// not redirect where root creates or chowns things).
func secureDirNoLinks(dir string) error {
	if fi, err := os.Lstat(dir); err == nil && (fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir()) {
		return fmt.Errorf("%s exists and is not a plain directory", dir)
	}
	return nil
}
