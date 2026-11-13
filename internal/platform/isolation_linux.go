//go:build linux

package platform

import (
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"

	impl "longarm/internal/platform/linux"
)

// CanIsolate reports whether this process can run workloads as other OS
// accounts (Windows: LocalSystem; Linux: root; macOS: never).
func CanIsolate() error { return impl.CanIsolate() }

// EnsureAccount creates the local account name if missing (locked password,
// own group, private home) and returns it.
func EnsureAccount(name string) (*Account, error) { return impl.EnsureAccount(name) }

// LookupAccount returns an existing local account.
func LookupAccount(name string) (*Account, error) { return impl.LookupAccount(name) }

// DeleteAccount kills the account's processes and deletes it with its home.
func DeleteAccount(name string) error { return impl.DeleteAccount(name) }

// AccountEnv is the base environment of the account's processes.
func AccountEnv(a *Account) ([]string, error) { return impl.AccountEnv(a) }

// SecureUserDir creates dir owned by a with mode 0700.
func SecureUserDir(dir string, a *Account) error { return secureDirOwner(dir, a.UID, a.GID) }

// SecurePrivateDir creates dir accessible to root only (0700).
func SecurePrivateDir(dir string) error { return secureDirOwner(dir, 0, 0) }

// secureDirOwner creates dir owned by uid:gid with mode 0700 - through an
// fd of the dir itself: a symlink a user swapped in after the check (in a
// parent the user owns) is refused, not followed to what it points at.
func secureDirOwner(dir string, uid, gid int) error {
	if err := secureDirNoLinks(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: dir, Err: err}
	}
	defer unix.Close(fd)
	if err := unix.Fchown(fd, uid, gid); err != nil {
		return &os.PathError{Op: "chown", Path: dir, Err: err}
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return &os.PathError{Op: "chmod", Path: dir, Err: err}
	}
	return nil
}

// PortOwners reports who listens on TCP port: whether a process of a does
// and whether a process of any other account does.
func PortOwners(port int, a *Account) (mine, others bool, err error) {
	uids, err := impl.ListenerUIDs(port)
	if err != nil {
		return false, false, err
	}
	for _, uid := range uids {
		if uid == a.UID {
			mine = true
		} else {
			others = true
		}
	}
	return mine, others, nil
}

// LoadProfile keeps a's profile loaded (Windows: HKCU of all its
// processes) until release; no-op elsewhere.
func LoadProfile(*Account) (release func(), err error) { return func() {}, nil }

// PeerIsAccount reports whether the process at the other end of c - a TCP
// connection longarmd accepted on loopback - runs as account a (the uid of
// the client's socket in /proc/net/tcp{,6}).
func PeerIsAccount(c net.Conn, a *Account) (bool, error) {
	local, remote, ok := tcpEnds(c.LocalAddr().String(), c.RemoteAddr().String())
	if !ok || a == nil {
		return false, errors.New("not a TCP connection")
	}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if uid, ok := procNetSocketUID(string(b), remote, local); ok {
			return uid == a.UID, nil
		}
	}
	return false, errors.New("the peer's socket is not in /proc/net/tcp")
}
