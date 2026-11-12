package supervisor

import (
	"os/user"

	"longarm/internal/platform"
)

func secureStdinDir(dir string) error {
	account, err := user.Current()
	if err != nil {
		return err
	}
	return platform.SecureUserDir(dir, &platform.Account{SID: account.Uid})
}
