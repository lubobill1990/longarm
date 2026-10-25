package longarmd

import (
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/platform"
	"github.com/lubobill1990/longarm/internal/server"
	"github.com/lubobill1990/longarm/internal/sshd"
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
