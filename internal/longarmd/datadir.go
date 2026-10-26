package longarmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/userspace"
)

// Default data directories (docs/usage.md "Data directory"):
//
//   - a normal account (any OS):  ~/.longarm
//   - root on Linux:              /var/lib/longarmd (the deb's
//     longarmd-peruser.service uses the same dir). Legacy: while it holds no
//     enrollment and ~root/.longarm does, ~root/.longarm stays the default so
//     existing root installs (shared mode) keep working; `longarmd service
//     install --isolation per-user` copies such an enrollment over.
//   - root on macOS:              ~/.longarm (no system default: per-user
//     isolation is refused there and the pkg names the dir explicitly)
//   - LocalSystem on Windows:     C:\ProgramData\Longarm\longarmd (the MSI's
//     DATADIR), with the same legacy rule for the systemprofile's .longarm.
//     An elevated administrator keeps ~\.longarm unless only the
//     ProgramData dir holds an enrollment (e.g. a per-user service's).
//
// LONGARM_DATA_DIR and --data-dir override all of it.
const (
	// LinuxSystemDataDir is root's default data dir on Linux.
	LinuxSystemDataDir = "/var/lib/longarmd"
	// DataDirEnv overrides the default data dir.
	DataDirEnv = "LONGARM_DATA_DIR"
	// SystemDataDirEnv replaces the system data dir (/var/lib/longarmd,
	// C:\ProgramData\Longarm\longarmd). For tests: e2e tests running as root
	// check the defaults without writing to the real one.
	SystemDataDirEnv = "LONGARM_TEST_SYSTEM_DATA_DIR"
	// homeDataDirName is the per-account data dir below the home dir.
	homeDataDirName = ".longarm"
)

// dataDirEnv is what the default data dir depends on (injected by tests).
type dataDirEnv struct {
	getenv   func(string) string
	home     string // "" = unknown
	system   bool   // root (Unix) / LocalSystem (Windows)
	elevated bool   // Windows: an elevated administrator (not LocalSystem)
	sysDir   string // the OS's system data dir, "" = none
	inUse    func(dir string) bool
}

// DefaultDataDir is longarmd's data dir when neither --data-dir nor
// LONGARM_DATA_DIR names one (see the table above).
func DefaultDataDir() string { return defaultDataDir(currentDataDirEnv()) }

// SystemDataDir is the OS's data dir for longarmd as root / LocalSystem
// (/var/lib/longarmd, C:\ProgramData\Longarm\longarmd), "" on macOS.
func SystemDataDir() string { return currentDataDirEnv().sysDir }

func (e dataDirEnv) homeDir() string {
	if e.home == "" {
		return homeDataDirName
	}
	return filepath.Join(e.home, homeDataDirName)
}

func defaultDataDir(e dataDirEnv) string {
	if d := e.getenv(DataDirEnv); d != "" {
		return d
	}
	home := e.homeDir()
	if e.sysDir == "" {
		return home
	}
	switch {
	case e.system:
		if e.home != "" && !e.inUse(e.sysDir) && e.inUse(home) {
			return home // legacy root / LocalSystem install
		}
		return e.sysDir
	case e.elevated:
		if !e.inUse(home) && e.inUse(e.sysDir) {
			return e.sysDir
		}
	}
	return home
}

// systemDataDirFor is the system data dir on goos ("" = none).
func systemDataDirFor(goos string, getenv func(string) string, programData string) string {
	if d := getenv(SystemDataDirEnv); d != "" {
		return d
	}
	switch goos {
	case "linux":
		return LinuxSystemDataDir
	case "windows":
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, "Longarm", "longarmd")
	}
	return ""
}

// enrollmentFiles are what `longarmd join` (or an admin) puts into a data
// dir and what a data dir migration copies: the certificate and keys, the
// hub address, the config, the ssh host key and authorized keys; tsnet*
// (tailnet keys and node state) are matched separately.
var enrollmentFiles = []string{TLSDir, hubclient.File, ConfigFile, SSHHostKeyFile, AuthorizedKeysFile}

// DataDirInUse reports whether dir holds an enrollment or a config
// (tls/, hub.toml or longarmd.toml).
func DataDirInUse(dir string) bool {
	for _, n := range []string{TLSDir, hubclient.File, ConfigFile} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return true
		}
	}
	return false
}

// ServiceDirRequest describes `longarmd service install`'s data dir.
type ServiceDirRequest struct {
	DataDir  string // --data-dir or the default
	Explicit bool   // --data-dir or LONGARM_DATA_DIR given
	PerUser  bool   // --isolation per-user
	User     string // --user
}

// ServiceDir is the data dir a service install uses.
type ServiceDir struct {
	Dir string
	// MigrateFrom holds an enrollment that is copied to Dir first (only for
	// the per-user default, when Dir has none yet).
	MigrateFrom string
	// UsersDir is where per-user isolation keeps the users' data.
	UsersDir string
}

// ResolveServiceDataDir picks and checks the data dir for `longarmd service
// install`:
//   - --data-dir / LONGARM_DATA_DIR: as given;
//   - per-user isolation: the system data dir (/var/lib/longarmd,
//     C:\ProgramData\Longarm\longarmd), adopting the enrollment of the
//     default dir / ~/.longarm when the system dir has none yet;
//   - shared mode with --user as root (Linux, macOS): that account's
//     ~/.longarm - an enrollment that only root's dir holds is refused, the
//     account could not read it;
//   - otherwise the default data dir.
//
// Per-user isolation also needs the users dir reachable by the accounts
// (Unix: every parent o+x), which a home dir like /root is not.
func ResolveServiceDataDir(r ServiceDirRequest) (ServiceDir, error) {
	return resolveServiceDataDir(r, currentDataDirEnv(), runtime.GOOS, lookupHome, userspace.CheckReachable)
}

func lookupHome(name string) (string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return "", err
	}
	if u.HomeDir == "" {
		return "", fmt.Errorf("account %s has no home directory", name)
	}
	return u.HomeDir, nil
}

func resolveServiceDataDir(r ServiceDirRequest, e dataDirEnv, goos string, home func(string) (string, error), reachable func(usersDir, dataDir string) error) (ServiceDir, error) {
	sd := ServiceDir{Dir: r.DataDir}
	switch {
	case r.Explicit:
	case r.PerUser && e.sysDir != "":
		sd.Dir = e.sysDir
		if !e.inUse(sd.Dir) {
			for _, src := range []string{r.DataDir, e.homeDir()} {
				if src != "" && !samePath(src, sd.Dir) && e.inUse(src) {
					sd.MigrateFrom = src
					break
				}
			}
		}
	case !r.PerUser && r.User != "" && r.User != "root" && goos != "windows" && e.system:
		name := r.User
		h, err := home(name)
		if err != nil {
			return sd, fmt.Errorf("--user %s: %w (pass --data-dir)", r.User, err)
		}
		sd.Dir = filepath.Join(h, homeDataDirName)
		if !samePath(sd.Dir, r.DataDir) && !e.inUse(sd.Dir) && e.inUse(r.DataDir) {
			return sd, fmt.Errorf("the enrollment is in %s (root's), but the service runs as %s with data dir %s, which holds none: "+
				"enroll as that account (sudo -u %s longarmd join <hub> <code>) or pass --data-dir with a dir the account owns",
				r.DataDir, r.User, sd.Dir, name)
		}
	}
	if !r.PerUser {
		return sd, nil
	}
	cfgDir := sd.Dir
	if sd.MigrateFrom != "" {
		cfgDir = sd.MigrateFrom // its longarmd.toml comes along
	}
	usersDir, err := configuredUsersDir(cfgDir, sd.Dir)
	if err != nil {
		return sd, err
	}
	sd.UsersDir = usersDir
	if err := reachable(usersDir, sd.Dir); err != nil {
		if !r.Explicit {
			return sd, err
		}
		return sd, fmt.Errorf("%w; or drop --data-dir / LONGARM_DATA_DIR to use the system data dir %s", err, e.sysDir)
	}
	return sd, nil
}

// configuredUsersDir is [isolation] users_dir of cfgDir's longarmd.toml
// (relative to dataDir), else <dataDir>/users.
func configuredUsersDir(cfgDir, dataDir string) (string, error) {
	var fc FileConfig
	p := filepath.Join(cfgDir, ConfigFile)
	if _, err := toml.DecodeFile(p, &fc); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	switch d := fc.Isolation.UsersDir; {
	case d == "":
		return filepath.Join(dataDir, userspace.UsersDir), nil
	case filepath.IsAbs(d):
		return d, nil
	default:
		return filepath.Join(dataDir, d), nil
	}
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// EnsureSystemDataDir creates the system data dir locked down (Windows: a
// protected DACL for SYSTEM and Administrators - ProgramData would let every
// local user read the node key; Unix: 0700) when dir is it and does not
// exist yet. Other dirs are left to their callers.
func EnsureSystemDataDir(dir string) error {
	sys := SystemDataDir()
	if sys == "" || !samePath(dir, sys) {
		return nil
	}
	if _, err := os.Lstat(dir); err == nil {
		return nil
	}
	// The ProgramData\Longarm parent: created locked down as well, so nobody
	// pre-creates anything in it. (Per-user isolation grants the accounts
	// traverse rights on it at start.)
	if parent := filepath.Dir(dir); runtime.GOOS == "windows" {
		if _, err := os.Lstat(parent); errors.Is(err, fs.ErrNotExist) {
			if err := platform.SecureDir(parent, ""); err != nil {
				return fmt.Errorf("secure %s: %w", parent, err)
			}
		}
	}
	if err := platform.SecureDir(dir, ""); err != nil {
		return fmt.Errorf("secure %s: %w", dir, err)
	}
	return nil
}

// MigrateEnrollment copies the enrollment (tls/, hub.toml, longarmd.toml,
// the ssh host key, authorized_keys, tsnet* tailnet keys and state) from
// src to dst, which must not hold one yet. Files keep their permission bits
// (without group/other write); dirs become 0700; symlinks are skipped. The
// source stays as it was: the caller tells the admin to remove it.
func MigrateEnrollment(src, dst string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if DataDirInUse(dst) {
		return fmt.Errorf("%s already holds an enrollment; not copying %s over it", dst, src)
	}
	if fi, err := os.Lstat(dst); err == nil && (fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir()) {
		return fmt.Errorf("%s exists and is not a plain directory", dst)
	}
	if err := EnsureSystemDataDir(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	names := append([]string(nil), enrollmentFiles...)
	if m, _ := filepath.Glob(filepath.Join(src, hubclient.TSNetBase+"*")); len(m) > 0 {
		for _, p := range m {
			names = append(names, filepath.Base(p))
		}
	}
	copied := 0
	for _, n := range names {
		s, d := filepath.Join(src, n), filepath.Join(dst, n)
		fi, err := os.Lstat(s)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			fmt.Fprintf(out, "  skipped %s (a symlink)\n", s)
			continue
		}
		if _, err := os.Lstat(d); err == nil {
			fmt.Fprintf(out, "  skipped %s (%s exists)\n", s, d)
			continue
		}
		if err := copyTree(s, d); err != nil {
			return fmt.Errorf("copy %s to %s: %w", s, d, err)
		}
		fmt.Fprintf(out, "  copied %s -> %s\n", s, d)
		copied++
	}
	if copied == 0 {
		return fmt.Errorf("%s: nothing to copy", src)
	}
	return nil
}

// copyTree copies a file or a dir tree (regular files and dirs only).
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			return os.Chmod(target, 0o700)
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			return copyFile(p, target, fi.Mode().Perm()&^0o022)
		}
		return nil // symlinks, sockets, ...: not part of an enrollment
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm)
}
