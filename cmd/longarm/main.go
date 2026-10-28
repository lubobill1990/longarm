// Command longarm is the Longarm client CLI: it runs commands, jobs and
// interactive sessions on remote workers, copies files and forwards ports.
//
// `longarm --help` groups the commands (Start here, Jobs, Sessions, Files &
// ports, Your workers, Account, Admin & troubleshooting); docs/usage.md is
// the reference. A worker is named by its name (workers.toml or the hub's
// list: `longarm ls`) or, ad hoc, "certname@host:port"; a job by its id, a
// unique prefix or suffix of it, or "last".
package main

import (
	"context"
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
	"github.com/lubobill1990/longarm/internal/telemetry"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

var configDir, transportPolicy string

func main() {
	root := newRootCmd()
	// One operation per run (telemetry.go): its trace context goes with
	// every call to workers and the hub.
	cli.start, cli.span = time.Now(), telemetry.NewSpan()
	telemetry.SetFallback(cli.span)
	root.SetContext(telemetry.WithSpan(context.Background(), cli.span))
	root.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		noticeOK = allowUpdateNotice(cmd)
		startTelemetry(cmd)
	}
	defer telemetry.Guard()
	var validated bool
	markUsageErrors(root, &validated)
	err := root.Execute()
	code := exitCodeFor(err, validated)
	var ee *exitError
	errors.As(err, &ee)
	finishTelemetry(code, err)
	switch {
	case err == nil:
		return
	case ee != nil:
		if ee.msg != "" {
			fmt.Fprintln(os.Stderr, "longarm:", ee.msg+traceNote())
		}
		os.Exit(ee.code)
	}
	if msg, ok := client.TrustRefusal(err); ok {
		fmt.Fprintln(os.Stderr, "longarm:", msg+traceNote())
	} else if s, ok := status.FromError(err); ok {
		fmt.Fprintf(os.Stderr, "longarm: %s: %s%s\n", s.Code(), dialErrorMessage(s.Message()), traceNote())
	} else {
		fmt.Fprintln(os.Stderr, "longarm:", err.Error()+traceNote())
	}
	os.Exit(code)
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
	nodesLive bool           // nodes is the hub's answer (not the cache)
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

func openSession() (*session, error) { return openSessionWith(false) }

// openSessionWith opens a session; allowExpired keeps going with an expired
// certificate (leave, doctor), which otherwise fails at once.
func openSessionWith(allowExpired bool) (*session, error) {
	cfg, err := client.LoadConfig(configDir)
	if err != nil {
		return nil, err
	}
	d, err := client.NewDialer(cfg)
	if err != nil {
		return nil, err
	}
	if leaf := d.Bundle().Leaf(); !allowExpired && leaf != nil && time.Now().After(leaf.NotAfter) {
		d.Close()
		return nil, signInExpiredError(leaf.NotAfter)
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
	if s.hubConn != nil && !keepHubForExit(s.hubConn) {
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
			return nil, usagef("--env %q: want KEY=VALUE", kv)
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
			return nil, usagef("--secret-env %s: not set in the local environment", n)
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
	return "", nil, usagef("usage: [<worker>] -- command [args...]")
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
