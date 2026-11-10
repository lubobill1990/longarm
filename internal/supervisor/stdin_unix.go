//go:build !windows

package supervisor

import (
	"fmt"
	"os"

	"github.com/lubobill1990/longarm/internal/platform"
)

func secureStdinDir(dir string) error {
	if info, err := os.Lstat(dir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("stdin binding path is not a plain directory")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return platform.SecureDir(dir, "")
}
