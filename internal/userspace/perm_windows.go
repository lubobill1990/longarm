//go:build windows

package userspace

import "github.com/lubobill1990/longarm/internal/platform"

// secureUsersDir creates the users dir (SYSTEM and Administrators only; the
// accounts reach their data below it by "bypass traverse checking").
func secureUsersDir(dir string) error { return platform.SecurePrivateDir(dir) }

// secureSpaceRoot creates a user's space root (SYSTEM and Administrators).
func secureSpaceRoot(dir string, _ *platform.Account) error { return platform.SecurePrivateDir(dir) }

func checkTraversable(string) error { return nil }
