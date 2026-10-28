package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// knownHostsFile (in the config dir) holds the workers' sshd host keys as
// learned over mTLS by `longarm ssh-config`.
const knownHostsFile = "known_hosts"

// sshHostPrefix prefixes worker names in ssh Host entries.
const sshHostPrefix = "longarm-"

func proxyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "proxy <worker>",
		Short: "Pipe stdin/stdout to the worker's built-in sshd over the mTLS channel (OpenSSH ProxyCommand)",
		Long: `Connect stdin/stdout to the built-in sshd of the worker through the
authenticated longarm channel. Used as an OpenSSH ProxyCommand (see longarm ssh-config):

  ssh -o ProxyCommand="longarm proxy w1" longarm-w1

The worker accepts the ssh connection without ssh-level authentication
because it arrives through the mTLS-authenticated channel. "longarm-<name>" is
accepted as an alias of <name>.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			if _, err := s.cfg.Worker(name); err != nil && strings.HasPrefix(name, sshHostPrefix) {
				name = strings.TrimPrefix(name, sshHostPrefix)
			}
			conn, err := s.conn(name)
			if err != nil {
				return err
			}
			defer conn.Close()
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			st, err := conn.Forward().Ssh(ctx)
			if err != nil {
				return err
			}
			if err := st.Send(&pb.ForwardFrame{}); err != nil {
				return err
			}
			go func() {
				buf := make([]byte, 32<<10)
				for {
					n, err := os.Stdin.Read(buf)
					if n > 0 {
						if st.Send(&pb.ForwardFrame{Data: buf[:n]}) != nil {
							return
						}
					}
					if err != nil {
						st.CloseSend()
						return
					}
				}
			}()
			for {
				m, err := st.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				if len(m.Data) > 0 {
					if _, err := os.Stdout.Write(m.Data); err != nil {
						return nil
					}
				}
				if m.Eof {
					return nil
				}
			}
		},
	}
}

// sshCmd runs OpenSSH to the worker's built-in sshd with longarm proxy as
// its ProxyCommand, built on the fly: no ~/.ssh/config entries needed (the
// tray's ssh buttons use it).
func sshCmd() *cobra.Command {
	var user string
	c := &cobra.Command{
		Use:   "ssh <worker> [-- ssh arguments...]",
		Short: "Open ssh to a worker's built-in sshd through longarm (no ssh config needed)",
		Long: `Run the OpenSSH client against the worker's built-in sshd through longarm's
mTLS channel (ProxyCommand longarm proxy), with the worker's host key fetched
over mTLS into <config-dir>/known_hosts - no ~/.ssh/config entries needed
(longarm ssh-config writes those for plain ssh, scp and VS Code):

  longarm ssh w1                    # interactive shell
  longarm ssh w1 -- git status      # a command`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			sshExe, err := exec.LookPath("ssh")
			if err != nil {
				return fmt.Errorf("no OpenSSH client (ssh) on this machine: %w (longarm attach %s opens a shell without it)", err, name)
			}
			s, err := openSession()
			if err != nil {
				return err
			}
			cfgDir, _ := filepath.Abs(s.cfg.Dir)
			keys, _ := fetchHostKeys(cmd.Context(), s, []string{name})
			s.close()
			khPath := filepath.Join(cfgDir, knownHostsFile)
			if err := updateKnownHosts(khPath, keys); err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			strict := "accept-new"
			if _, ok := keys[name]; ok {
				strict = "yes"
			}
			argv := []string{
				"-o", fmt.Sprintf("ProxyCommand=%s --config-dir %s proxy %s", sshQuote(exe), sshQuote(cfgDir), name),
				"-o", "HostKeyAlias=" + sshHostPrefix + name,
				"-o", "UserKnownHostsFile=" + sshQuote(khPath),
				"-o", "StrictHostKeyChecking=" + strict,
				"-o", "ServerAliveInterval=30",
				"-l", user, name,
			}
			sc := exec.Command(sshExe, append(argv, args[1:]...)...)
			sc.Stdin, sc.Stdout, sc.Stderr = os.Stdin, os.Stdout, os.Stderr
			if err := sc.Run(); err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					return &exitError{code: ee.ExitCode()}
				}
				return err
			}
			return nil
		},
	}
	c.Flags().StringVar(&user, "user", "longarm", "ssh user name (informational; sessions run as longarmd's account)")
	return c
}

// fetchHostKeys fetches the workers' sshd host keys over mTLS (Node.Info);
// workers that can't be reached or have no sshd are reported on stderr and
// left out.
func fetchHostKeys(ctx context.Context, s *session, names []string) (map[string]string, error) {
	keys := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := s.conn(n)
			if err != nil {
				fmt.Fprintf(os.Stderr, "longarm: %s: %v\n", n, err)
				return
			}
			defer conn.Close()
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			info, err := conn.Node().Info(ctx, &pb.InfoRequest{})
			if err != nil {
				fmt.Fprintf(os.Stderr, "longarm: %s: host key not fetched: %s\n", n, oneLine(err))
				return
			}
			if info.SshHostKey == "" {
				fmt.Fprintf(os.Stderr, "longarm: %s: built-in sshd is disabled\n", n)
				return
			}
			mu.Lock()
			keys[n] = info.SshHostKey
			mu.Unlock()
		}()
	}
	wg.Wait()
	return keys, nil
}

func sshConfigCmd() *cobra.Command {
	var noFetch bool
	var user string
	c := &cobra.Command{
		Use:   "ssh-config [worker...]",
		Short: "Print OpenSSH Host entries (longarm-<worker>, ProxyCommand longarm proxy) and record the workers' host keys",
		Long: `Print ~/.ssh/config Host entries for the workers (all configured ones by
default), so that plain ssh / scp / sftp and VS Code Remote-SSH reach the
built-in sshd through longarm's mTLS channel:

  longarm ssh-config >> ~/.ssh/config
  ssh longarm-w1                    # interactive shell (pwsh on Windows workers)
  ssh longarm-w1 git status         # exec (cmd.exe /c on Windows workers)
  scp file.txt longarm-w1:C:/tmp/   # scp uses the sftp subsystem

Each worker's sshd host key is fetched over mTLS (Node.Info) and written to
<config-dir>/known_hosts, which the entries use with strict checking.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			names := args
			if len(names) == 0 {
				for _, w := range s.cfg.Workers {
					names = append(names, w.Name)
				}
			}
			if len(names) == 0 {
				return noWorkersError(s)
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			cfgDir, _ := filepath.Abs(s.cfg.Dir)
			khPath := filepath.Join(cfgDir, knownHostsFile)
			keys := map[string]string{}
			if !noFetch {
				keys, _ = fetchHostKeys(cmd.Context(), s, names)
				if err := updateKnownHosts(khPath, keys); err != nil {
					return err
				}
			}
			for _, n := range names {
				fmt.Printf("Host %s%s\n", sshHostPrefix, n)
				fmt.Printf("  HostName %s\n", n)
				fmt.Printf("  User %s\n", user)
				fmt.Printf("  ProxyCommand %s --config-dir %s proxy %s\n", sshQuote(exe), sshQuote(cfgDir), n)
				fmt.Printf("  HostKeyAlias %s%s\n", sshHostPrefix, n)
				fmt.Printf("  UserKnownHostsFile %s\n", sshQuote(khPath))
				if _, ok := keys[n]; ok || noFetch {
					fmt.Printf("  StrictHostKeyChecking yes\n")
				} else {
					fmt.Printf("  StrictHostKeyChecking accept-new\n")
				}
				fmt.Printf("  ServerAliveInterval 30\n\n")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&noFetch, "no-fetch", false, "do not contact the workers (keep known_hosts as is)")
	c.Flags().StringVar(&user, "user", "longarm", "ssh user name (informational; sessions run as longarmd's account)")
	return c
}

// sshQuote quotes a path for ssh_config (double quotes; forward slashes on
// Windows avoid backslash-escaping surprises).
func sshQuote(p string) string {
	if runtime.GOOS == "windows" {
		p = filepath.ToSlash(p)
	}
	if strings.ContainsAny(p, " \t\"") {
		return `"` + strings.ReplaceAll(p, `"`, `\"`) + `"`
	}
	return p
}

// updateKnownHosts replaces the lines for the given workers' aliases.
func updateKnownHosts(path string, keys map[string]string) error {
	if len(keys) == 0 {
		return nil
	}
	var kept []string
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
			host, _, _ := strings.Cut(l, " ")
			if l == "" {
				continue
			}
			if _, replaced := keys[strings.TrimPrefix(host, sshHostPrefix)]; replaced && strings.HasPrefix(host, sshHostPrefix) {
				continue
			}
			kept = append(kept, l)
		}
	}
	for n, k := range keys {
		kept = append(kept, sshHostPrefix+n+" "+k)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
}
