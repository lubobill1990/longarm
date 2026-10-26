package longarmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lubobill1990/longarm/internal/loopguard"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/server"
	"github.com/lubobill1990/longarm/internal/sshd"
	"github.com/lubobill1990/longarm/internal/tenancy"
	"github.com/lubobill1990/longarm/internal/userspace"
)

// SFTPCommand is longarmd's subcommand serving sftp on stdin/stdout (run as
// a user's account by the built-in sshd with per-user isolation).
const SFTPCommand = "sftp-server"

// sshSession makes the built-in sshd run a caller's shells, commands and
// sftp as the caller's OS account and check its direct-tcpip targets like
// Forward.Open.
func sshSession(srv *server.Server, exe string) func(pki.Identity) (*sshd.Session, error) {
	return func(id pki.Identity) (*sshd.Session, error) {
		sp, err := srv.AccountFor(id)
		if err != nil {
			return nil, err
		}
		env, err := sp.Env()
		if err != nil {
			return nil, err
		}
		return &sshd.Session{
			Account: sp.Account, Env: env, Home: sp.Home(), Cgroup: sp.Cgroup,
			SFTP:    func(rw io.ReadWriteCloser) error { return runSFTP(sp, exe, env, rw) },
			Forward: func(host string, port int) error { return srv.ForwardCheck(id, host, port) },
		}, nil
	}
}

// runSFTP serves one sftp subsystem channel with `longarmd sftp-server` as
// the space's account.
func runSFTP(sp *userspace.Space, exe string, env []string, rw io.ReadWriteCloser) error {
	ctr, err := platform.NewContainer(platform.ContainerOptions{Name: "longarm-sftp", RunAs: sp.Account})
	if err != nil {
		return err
	}
	defer ctr.Close()
	inR, inW, err := os.Pipe()
	if err != nil {
		return err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return err
	}
	cmd := exec.Command(exe, SFTPCommand)
	cmd.Env, cmd.Dir = env, sp.Home()
	cmd.Stdin, cmd.Stdout = inR, outW
	err = ctr.Start(cmd)
	inR.Close()
	outW.Close()
	if err != nil {
		inW.Close()
		outR.Close()
		return err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		io.Copy(inW, rw) // the client's requests; ends when the channel closes
		inW.Close()
	}()
	io.Copy(rw, outR) // the server's answers; ends when it exits
	outR.Close()
	err = cmd.Wait()
	rw.Close()
	wg.Wait()
	return err
}

// SetIsolation writes [isolation] mode into the data dir's longarmd.toml
// (installers: MSI ISOLATION=per-user, longarmd service install --isolation).
// A file that already has an [isolation] section is left alone.
func SetIsolation(dataDir, mode string) error {
	if mode == "" || mode == tenancy.Shared {
		return nil
	}
	if mode != tenancy.PerUser {
		return fmt.Errorf("isolation %q: want %q or %q", mode, tenancy.Shared, tenancy.PerUser)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(dataDir, ConfigFile)
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.Contains(string(b), "[isolation]") {
		return nil
	}
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		b = append(b, '\n')
	}
	b = append(b, "\n# multi-tenancy (docs/security.md): every user's work runs as an OS account of its own\n[isolation]\nmode = \"per-user\"\n"...)
	return os.WriteFile(p, b, 0o600)
}

// deleteUserAccounts deletes the OS accounts of the users' spaces
// (per-user isolation; longarmd leave --purge, uninstall).
func deleteUserAccounts(cfg Config, out io.Writer) {
	if cfg.Isolation.Mode != tenancy.PerUser || platform.CanIsolate() != nil {
		return
	}
	if loopguard.Supported {
		if err := loopguard.Remove(); err != nil {
			fmt.Fprintf(out, "warning: remove the loopback isolation rules (nftables table inet %s): %v\n", loopguard.Table, err)
		}
	}
	usersDir := cfg.Isolation.UsersDir
	if usersDir == "" {
		usersDir = filepath.Join(cfg.DataDir, userspace.UsersDir)
	}
	ents, _ := os.ReadDir(usersDir)
	for _, e := range ents {
		if _, err := os.Stat(filepath.Join(usersDir, e.Name(), userspace.OwnerFile)); err != nil || !e.IsDir() {
			continue
		}
		if err := platform.DeleteAccount(e.Name()); err != nil {
			fmt.Fprintf(out, "warning: delete account %s: %v\n", e.Name(), err)
		} else {
			fmt.Fprintf(out, "deleted account %s\n", e.Name())
		}
		os.RemoveAll(filepath.Join(usersDir, e.Name()))
	}
}
