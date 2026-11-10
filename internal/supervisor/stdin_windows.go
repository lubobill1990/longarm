package supervisor

import (
	"os/user"

	"github.com/lubobill1990/longarm/internal/platform"
)

func secureStdinDir(dir string) error {
	account, err := user.Current()
	if err != nil {
		return err
	}
	return platform.SecureUserDir(dir, &platform.Account{SID: account.Uid})
}
