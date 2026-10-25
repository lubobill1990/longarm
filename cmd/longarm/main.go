// Command longarm is the Longarm client CLI (runs on the interactive machines).
//
//	longarm ls                                    metrics of all configured workers
//	longarm info <w>
//	longarm exec <w> [flags] -- cmd...            short synchronous command
//	longarm run  <w> [flags] -- cmd...            submit a job (prints its id)
//	longarm logs <w> <id> [-f] [--from N]
//	longarm wait <w> <id>                         exit code mirrors the job's
//	longarm kill <w> <id>
//	longarm jobs <w>
//	longarm attach <w> [--session S] [-- cmd...]  interactive PTY session (detach: Ctrl-] d)
//	longarm sessions <w>; longarm logs <w> <S> --session; longarm attach <w> -s S --kill
//	longarm forward <w> <port> [localPort]        port forward (--list, --stop)
//	longarm put <local>... <w>:<path>; longarm get <w>:<path> <local>   resumable copies
//	longarm sync <dir> <w>:<path>                 git checkout -> worktree on the worker
//	longarm run <w> --repo . [--ref R] -- cmd...  job in a fresh worktree
//	longarm proxy <w>; longarm ssh-config              built-in sshd for ssh/scp/sftp/VS Code
//	longarm admin ca init | issue | shutdown <w> | gc <w>
//
// <w> is a worker name from workers.toml, or an ad-hoc "certname@host:port".
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Exit codes of longarm itself. Job exit codes are mirrored by run --wait / wait.
const (
	exitLongarmError = 255 // longarm / connection / usage error
	exitKilled       = 137 // job was killed
	exitLost         = 254 // job was lost (supervisor vanished)
)

// exitError carries a specific process exit code.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

var configDir, transportPolicy string

func main() {
	root := &cobra.Command{
		Use:           "longarm",
		Short:         "Longarm client: run commands and jobs on remote workers",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}
	root.PersistentFlags().StringVar(&configDir, "config-dir", client.DefaultConfigDir(),
		"client config dir (workers.toml, tls/, ca/); env LONGARM_CONFIG_DIR")
	root.PersistentFlags().StringVar(&transportPolicy, "transport", "",
		"transport policy: race (default: all legs at once, first mTLS handshake wins) | tcp | tailscale | longnet (prefer); env LONGARM_TRANSPORT; persistent: longarm transport <policy>")
	root.PersistentFlags().BoolVar(&noHub, "no-hub", os.Getenv("LONGARM_NO_HUB") == "1", "ignore the hub; talk to workers directly (env LONGARM_NO_HUB=1)")
	root.AddCommand(poolCmd(), transportCmd(), firewallCmd())
	root.AddCommand(lsCmd(), infoCmd(), execCmd(), runCmd(), logsCmd(), waitCmd(), killCmd(), jobsCmd(), adminCmd(),
		attachCmd(), sessionsCmd(), forwardCmd(), putCmd(), getCmd(), syncCmd(), proxyCmd(), sshConfigCmd(),
		joinCmd(), leaveCmd(), daemonCmd(), mcpCmd())
	err := root.Execute()
	if err == nil {
		return
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			fmt.Fprintln(os.Stderr, "longarm:", ee.msg)
		}
		os.Exit(ee.code)
	}
	if s, ok := status.FromError(err); ok {
		fmt.Fprintf(os.Stderr, "longarm: %s: %s\n", s.Code(), s.Message())
	} else {
		fmt.Fprintln(os.Stderr, "longarm:", err)
	}
	os.Exit(exitLongarmError)
}

// session bundles the loaded config, a dialer and (lazily) the hub.
type session struct {
	cfg    *client.Config
	dialer *client.Dialer

	hubMu     sync.Mutex
	hubConn   *hubclient.Conn
	hubErr    error
	hubErrAt  time.Time
	nodesAt   time.Time
	nodes     []*pb.NodeInfo // hub workers (or the cache), for name lookups
	placed    *pb.NodeInfo   // the worker the hub just assigned
	renewOnce sync.Once

	hubStateOnce sync.Once
	hubStateSeen bool   // hub-state.json existed
	hubStateNote string // why the hub is skipped (from hub-state.json)
	hubNoteOnce  sync.Once
}

// noHub (--no-hub, LONGARM_NO_HUB=1) ignores the hub; quietHub silences the
// "hub unreachable" note.
var noHub, quietHub bool

func openSession() (*session, error) {
	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		return nil, err
	}
	d, err := client.NewDialer(cfg)
	if err != nil {
		return nil, err
	}
	if transportPolicy != "" {
		if err := d.SetPolicy(transportPolicy); err != nil {
			return nil, err
		}
	}
	s := &session{cfg: cfg, dialer: d}
	d.SkipLeg = s.skipLeg
	// tailscale / longnet legs through longarm daemon when it runs (its nodes are
	// up already); runDaemon and longarm mcp undo OneShot.
	lp := &client.LocalLegProxy{AddrFile: filepath.Join(configDir, daemonAddrFile)}
	d.Proxy, d.LiveLegs = lp, lp.LiveLegs
	d.OneShot = true
	return s, nil
}

func (s *session) close() {
	s.dialer.Close()
	s.hubMu.Lock()
	if s.hubConn != nil {
		s.hubConn.Close()
	}
	s.hubMu.Unlock()
}

func (s *session) conn(name string) (*client.Conn, error) {
	w, err := s.worker(name)
	if err != nil {
		return nil, err
	}
	return s.dialer.Dial(w)
}

// parseEnv turns K=V flags into a map.
func parseEnv(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--env %q: want KEY=VALUE", kv)
		}
		m[k] = v
	}
	return m, nil
}

// secretEnv reads the named variables from longarm's own environment; the values
// travel to the worker over mTLS and never touch its disk.
func secretEnv(names []string) (map[string]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	m := make(map[string]string, len(names))
	for _, n := range names {
		k, v, ok := strings.Cut(n, "=")
		if ok { // NAME=LOCALVAR: send local $LOCALVAR as NAME
			n = v
		}
		val, set := os.LookupEnv(n)
		if !set {
			return nil, fmt.Errorf("--secret-env %s: not set in the local environment", n)
		}
		m[k] = val
	}
	return m, nil
}

// commandArgs splits "[<worker>] -- cmd..." style arguments; worker is ""
// when only "-- cmd..." was given (automatic placement).
func commandArgs(cmd *cobra.Command, args []string) (worker string, argv []string, err error) {
	switch dash := cmd.ArgsLenAtDash(); {
	case dash == 0 && len(args) > 0:
		return "", args, nil
	case dash == 1 && len(args) > 1:
		return args[0], args[1:], nil
	case dash < 0 && len(args) >= 2:
		return args[0], args[1:], nil
	}
	return "", nil, errors.New("usage: [<worker>] -- command [args...]")
}

func humanBytes(b uint64) string {
	const gb = 1 << 30
	return fmt.Sprintf("%.1f", float64(b)/gb)
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
