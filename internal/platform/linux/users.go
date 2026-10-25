//go:build linux

package linux

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
)

// Per-user isolation on Linux: one system account per Longarm user
// (la-<user>), created with useradd (locked password, its own group and a
// private home), workloads run with its uid/gid and no supplementary groups.

var accountMu sync.Mutex

// CanIsolate reports whether workloads can run as other accounts (root).
func CanIsolate() error {
	if os.Geteuid() != 0 {
		return errors.New("per-user isolation needs longarmd to run as root")
	}
	return nil
}

// EnsureAccount creates the account if missing and returns it.
func EnsureAccount(name string) (*ptypes.Account, error) {
	if name == "" || len(name) > 32 || strings.ContainsAny(name, ":/ \t\n") || strings.HasPrefix(name, "-") {
		return nil, fmt.Errorf("invalid account name %q", name)
	}
	accountMu.Lock()
	defer accountMu.Unlock()
	if a, err := LookupAccount(name); err == nil {
		return a, nil
	}
	shell := "/bin/sh"
	if _, err := os.Stat("/bin/bash"); err == nil {
		shell = "/bin/bash"
	}
	var out []byte
	var err error
	if p, lerr := exec.LookPath("useradd"); lerr == nil {
		// -K UMASK=077: a private home even where HOME_MODE says 0755.
		out, err = exec.Command(p, "--create-home", "--user-group", "--shell", shell, "-K", "UMASK=0077",
			"--comment", "Longarm per-user isolation account", name).CombinedOutput()
	} else if p, lerr := exec.LookPath("adduser"); lerr == nil { // busybox / alpine
		out, err = exec.Command(p, "-D", "-s", shell, "-g", "Longarm per-user isolation account", name).CombinedOutput()
	} else {
		return nil, errors.New("neither useradd nor adduser found")
	}
	if err != nil {
		return nil, fmt.Errorf("create account %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	a, err := LookupAccount(name)
	if err != nil {
		return nil, err
	}
	if a.Home != "" {
		os.Chmod(a.Home, 0o700)
	}
	return a, nil
}

// LookupAccount returns an existing account.
func LookupAccount(name string) (*ptypes.Account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("account %s: bad uid/gid", name)
	}
	if uid == 0 {
		return nil, fmt.Errorf("account %s is root", name)
	}
	return &ptypes.Account{Name: name, UID: uid, GID: gid, Home: u.HomeDir}, nil
}

// DeleteAccount kills the account's processes and deletes it with its home.
func DeleteAccount(name string) error {
	accountMu.Lock()
	defer accountMu.Unlock()
	a, err := LookupAccount(name)
	if err != nil {
		return nil // gone already
	}
	for i := 0; i < 20; i++ {
		if killUID(a.UID) == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	var out []byte
	if p, lerr := exec.LookPath("userdel"); lerr == nil {
		out, err = exec.Command(p, "--remove", name).CombinedOutput()
	} else if p, lerr := exec.LookPath("deluser"); lerr == nil {
		out, err = exec.Command(p, "--remove-home", name).CombinedOutput()
	} else {
		return errors.New("neither userdel nor deluser found")
	}
	if err != nil {
		// userdel exits 12 when it could not remove the mail spool / home
		// of a never used account; the account is gone then.
		if _, lerr := user.Lookup(name); lerr != nil {
			if a.Home != "" && strings.Contains(a.Home, name) {
				os.RemoveAll(a.Home)
			}
			return nil
		}
		return fmt.Errorf("delete account %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// killUID SIGKILLs every process of uid and returns how many it found.
func killUID(uid int) int {
	n := 0
	for _, pid := range listPIDs() {
		var st syscall.Stat_t
		if syscall.Stat(fmt.Sprintf("/proc/%d", pid), &st) == nil && int(st.Uid) == uid {
			syscall.Kill(pid, syscall.SIGKILL)
			n++
		}
	}
	return n
}

// AccountEnv is the base environment of the account's processes: like a
// login, nothing of longarmd's (root's) environment but the locale.
func AccountEnv(a *ptypes.Account) ([]string, error) {
	shell := "/bin/sh"
	if u, err := lookupShell(a.Name); err == nil && u != "" {
		shell = u
	}
	env := []string{"HOME=" + a.Home, "USER=" + a.Name, "LOGNAME=" + a.Name, "SHELL=" + shell,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	for _, k := range []string{"LANG", "LC_ALL", "LC_CTYPE", "TZ"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env, nil
}

func lookupShell(name string) (string, error) {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) >= 7 && parts[0] == name {
			return parts[6], nil
		}
	}
	return "", errors.New("not found")
}

// ListenerUIDs returns the uids owning sockets that listen on TCP port
// (/proc/net/tcp and tcp6, state LISTEN).
func ListenerUIDs(port int) ([]int, error) {
	var uids []int
	found := false
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		found = true
		for i, line := range strings.Split(string(b), "\n") {
			fs := strings.Fields(line)
			if i == 0 || len(fs) < 8 || fs[3] != "0A" { // 0A = LISTEN
				continue
			}
			_, portHex, ok := strings.Cut(fs[1], ":")
			if !ok {
				continue
			}
			pb, err := hex.DecodeString(portHex)
			if err != nil || len(pb) != 2 || int(pb[0])<<8|int(pb[1]) != port {
				continue
			}
			if uid, err := strconv.Atoi(fs[7]); err == nil {
				uids = append(uids, uid)
			}
		}
	}
	if !found {
		return nil, errors.New("/proc/net/tcp is not readable")
	}
	return uids, nil
}
