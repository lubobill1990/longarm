package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/platform"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// forwardsDir holds one JSON file per running `longarm forward` process, so
// `longarm forward --list/--stop` can find them.
const forwardsDir = "forwards"

type forwardRecord struct {
	PID       int       `json:"pid"`
	PIDStart  uint64    `json:"pid_start"`
	Worker    string    `json:"worker"`
	Remote    string    `json:"remote"`
	Local     string    `json:"local"`
	StartedAt time.Time `json:"started_at"`
}

func forwardCmd() *cobra.Command {
	var list bool
	var stop string
	var bind string
	var bg bool
	c := &cobra.Command{
		Use:   "forward <worker> <[host:]remotePort> [localPort]",
		Short: "Forward a local port to a port on the worker (runs in the foreground; --list / --stop manage running forwards)",
		Long: `Listen on 127.0.0.1:<localPort> (default: same as the remote port; 0 = any free
port) and carry every accepted connection to <remotePort> on the worker's
127.0.0.1 (or the given host as seen from the worker), each one as its own
stream multiplexed over the worker's gRPC/mTLS connection. Runs until
interrupted.

  longarm forward w1 3000            # http://127.0.0.1:3000 -> w1's 127.0.0.1:3000
  longarm forward w1 5432 15432
  longarm forward --list             # forwards started by this user on this machine
  longarm forward --stop 15432       # stop by local port (or "all")`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if list {
				return listForwards()
			}
			if stop != "" {
				return stopForwards(stop)
			}
			if len(args) < 2 || len(args) > 3 {
				return errors.New("usage: longarm forward <worker> <[host:]remotePort> [localPort]")
			}
			remote := args[1]
			local := remote
			if i := strings.LastIndex(remote, ":"); i >= 0 {
				local = remote[i+1:]
			}
			if len(args) == 3 {
				local = args[2]
			}
			if _, err := strconv.Atoi(local); err != nil {
				return fmt.Errorf("bad local port %q", local)
			}
			if bg {
				return forwardInDaemon(cmd.Context(), args[0], remote, local, len(args) == 3, bind)
			}
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			conn, err := s.conn(args[0])
			if err != nil {
				return err
			}
			defer conn.Close()
			ln, err := net.Listen("tcp", net.JoinHostPort(bind, local))
			if err != nil {
				return err
			}
			defer ln.Close()
			ctx, stopSig := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stopSig()
			unreg := registerForward(args[0], remote, ln.Addr().String())
			defer unreg()
			fmt.Fprintf(os.Stderr, "longarm: forwarding %s -> %s:%s (Ctrl+C to stop)\n", ln.Addr(), args[0], remote)
			go func() {
				<-ctx.Done()
				ln.Close()
			}()
			var wg sync.WaitGroup
			for {
				c, err := ln.Accept()
				if err != nil {
					break
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := forwardConn(ctx, conn, remote, c); err != nil {
						fmt.Fprintf(os.Stderr, "longarm: forward %s: %s\n", c.RemoteAddr(), oneLine(err))
					}
				}()
			}
			wg.Wait()
			return nil
		},
	}
	c.Flags().BoolVar(&list, "list", false, "list running forwards")
	c.Flags().StringVar(&stop, "stop", "", `stop the running forward(s) on this local port ("all" for every one)`)
	c.Flags().StringVar(&bind, "bind", "127.0.0.1", "local address to listen on")
	c.Flags().BoolVar(&bg, "bg", false, "host the forward in the background longarm daemon (started if needed) and return")
	return c
}

// forwardConn carries one TCP connection over a Forward.Open stream with
// half-close in both directions.
func forwardConn(ctx context.Context, conn *client.Conn, target string, c net.Conn) error {
	defer c.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	st, err := conn.Forward().Open(ctx)
	if err != nil {
		return err
	}
	if err := st.Send(&pb.ForwardFrame{Target: target}); err != nil {
		return err
	}
	go func() { // local -> worker
		buf := make([]byte, 32<<10)
		for {
			n, err := c.Read(buf)
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
	for { // worker -> local
		m, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(m.Data) > 0 {
			if _, err := c.Write(m.Data); err != nil {
				return nil
			}
		}
		if m.Eof {
			if tc, ok := c.(*net.TCPConn); ok {
				tc.CloseWrite()
			}
		}
	}
}

func registerForward(worker, remote, local string) func() {
	dir := filepath.Join(configDir, forwardsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return func() {}
	}
	pid := os.Getpid()
	start, _ := platform.ProcessStartTime(pid)
	b, _ := json.Marshal(forwardRecord{PID: pid, PIDStart: start, Worker: worker, Remote: remote, Local: local, StartedAt: time.Now()})
	p := filepath.Join(dir, strconv.Itoa(pid)+".json")
	os.WriteFile(p, b, 0o600)
	return func() { os.Remove(p) }
}

// liveForwards returns the records of running forwards, deleting stale ones.
func liveForwards() []forwardRecord {
	dir := filepath.Join(configDir, forwardsDir)
	ents, _ := os.ReadDir(dir)
	var out []forwardRecord
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r forwardRecord
		if json.Unmarshal(b, &r) != nil || !platform.ProcessAlive(r.PID, r.PIDStart) {
			os.Remove(p)
			continue
		}
		out = append(out, r)
	}
	return out
}

func listForwards() error {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LOCAL\tWORKER\tREMOTE\tPID\tSINCE")
	for _, r := range liveForwards() {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", r.Local, r.Worker, r.Remote, r.PID, r.StartedAt.Local().Format("01-02 15:04:05"))
	}
	printDaemonForwards(tw)
	return tw.Flush()
}

func stopForwards(which string) error {
	n := 0
	for _, r := range liveForwards() {
		_, port, _ := net.SplitHostPort(r.Local)
		if which != "all" && which != port && which != r.Local {
			continue
		}
		if err := platform.KillProcess(r.PID); err != nil {
			fmt.Fprintf(os.Stderr, "longarm: stop pid %d: %v\n", r.PID, err)
			continue
		}
		os.Remove(filepath.Join(configDir, forwardsDir, strconv.Itoa(r.PID)+".json"))
		fmt.Printf("stopped %s -> %s:%s\n", r.Local, r.Worker, r.Remote)
		n++
	}
	if c, closeFn, err := daemonClient(false); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if r, err := c.StopForward(ctx, &pb.StopForwardRequest{LocalAddr: which}); err == nil {
			for _, f := range r.Forwards {
				fmt.Printf("stopped %s -> %s:%s (daemon)\n", f.LocalAddr, f.Worker, f.Target)
				n++
			}
		}
		cancel()
		closeFn()
	}
	if n == 0 {
		return &exitError{code: 1, msg: "no matching forward"}
	}
	return nil
}

// forwardInDaemon asks the longarm daemon to host a forward.
func forwardInDaemon(ctx context.Context, worker, remote, local string, explicit bool, bind string) error {
	c, closeFn, err := daemonClient(true)
	if err != nil {
		return err
	}
	defer closeFn()
	req := &pb.StartForwardRequest{Worker: worker, Target: remote, Bind: bind}
	if explicit {
		p, _ := strconv.Atoi(local)
		req.LocalPort = int32(p)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info, err := c.StartForward(ctx, req)
	if err != nil {
		return err
	}
	_, port, _ := net.SplitHostPort(info.LocalAddr)
	fmt.Fprintf(os.Stderr, "longarm: forwarding %s -> %s:%s in longarm daemon (stop: longarm forward --stop %s)\n", info.LocalAddr, worker, remote, port)
	fmt.Println(info.Url)
	return nil
}
