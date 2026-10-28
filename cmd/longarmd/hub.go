package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/localapi"
	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/sched"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func joinCmd() *cobra.Command {
	var dataDir, name, fp string
	var noWait, force bool
	c := &cobra.Command{
		Use:   "join <hub[:port]> <code>",
		Short: "Enroll this worker with longarm-hub using a one-time join code",
		Long: `Enroll this worker with longarm-hub. The key is generated here; the hub signs
a certificate. New workers stay pending until an admin runs
"longarm admin approve <name>" (unless the hub auto-approves); longarmd join waits
for that unless --no-wait is given, in which case longarmd run (the service)
finishes the enrollment once approved.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir, err := filepath.Abs(dataDir)
			if err != nil {
				return err
			}
			fmt.Printf("data dir %s\n", dataDir)
			if w := longarmd.ProtectedDataDirWarning(dataDir); w != "" {
				fmt.Fprintln(os.Stderr, "warning: "+w)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return longarmd.Join(ctx, dataDir, args[0], args[1], longarmd.JoinOptions{Name: name, Fingerprint: fp, NoWait: noWait, Force: force, Out: os.Stdout})
		},
	}
	c.Flags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory (root: /var/lib/longarmd, LocalSystem: C:\\ProgramData\\Longarm\\longarmd, else ~/.longarm); env LONGARM_DATA_DIR")
	c.Flags().StringVar(&name, "name", "", "worker name (default: hostname)")
	c.Flags().StringVar(&fp, "ca-fingerprint", "", "expected sha256 fingerprint of the hub CA (printed by longarm-hub ca show)")
	c.Flags().BoolVar(&noWait, "no-wait", false, "don't wait for approval")
	c.Flags().BoolVar(&force, "force", false, "enroll again although a certificate exists")
	return c
}

func loginCmd() *cobra.Command {
	var dataDir, name, fp, scope, role, configDir, deviceName string
	var labels []string
	var az, force, noBrowser bool
	c := &cobra.Command{
		Use:   "login [hub]",
		Short: "Enroll this worker as yours, signing in with your Microsoft account",
		Long: `Enroll this worker with longarm-hub as your own worker: usable by you only
until you share it (longarm worker share), approved right away.

The hub is its domain - longarmd login longarm.example.com - or
https://longarm.example.com, or host:port (the gRPC port, default 7443).
Without --ca-fingerprint the hub's CA is fetched from
https://<host>/ca.crt, checked with this system's HTTPS roots like a
browser opening the hub's panel, and pinned (docs/security.md "First
contact"); a hub without a public HTTPS panel needs --ca-fingerprint.
Without a hub: the one the data dir's hub.toml names (an installer's
sign-in to come, e.g. the MSI with LOGIN=1), else the one the MSI
recorded, else the one built into this release (if any: longarm version --json).

By default longarmd login prints the hub's /link address and a code: open
the page in any browser (your laptop or phone will do - this machine needs
none), sign in with Microsoft, type the code, check the details and
approve. The page is opened here when this machine has a desktop
(--no-browser: never).

--role both also signs this machine in as your client device (longarm's
config dir, --config-dir) with the same code and the same approval: the
page lists both. Run it as the user who uses longarm here.

--az uses the Azure CLI's sign-in on this machine instead (az login): no
browser, no code.

--label pool=devbox (repeatable) has the hub record labels for the worker,
like longarm worker add --label; its owner changes them later with
longarm worker label <worker> KEY=VALUE KEY-.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir, err := filepath.Abs(dataDir)
			if err != nil {
				return err
			}
			if role != "worker" && role != "both" {
				return fmt.Errorf("--role %q: want worker or both (a client only: longarm login)", role)
			}
			lm, err := sched.ParseLabels(labels, false)
			if err != nil {
				return err
			}
			hub := ""
			if len(args) > 0 {
				hub = args[0]
			} else if hub, fp, err = longarmd.PendingHub(dataDir, fp); err != nil {
				return err
			}
			fmt.Printf("data dir %s\n", dataDir)
			if w := longarmd.ProtectedDataDirWarning(dataDir); w != "" {
				fmt.Fprintln(os.Stderr, "warning: "+w)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return longarmd.Login(ctx, dataDir, hub, longarmd.LoginOptions{Name: name, Fingerprint: fp, Force: force,
				Az: az, Scope: scope, Browser: !noBrowser, Out: os.Stdout, Labels: lm,
				Client: role == "both", ConfigDir: configDir, DeviceName: deviceName})
		},
	}
	c.Flags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory (root: /var/lib/longarmd, LocalSystem: C:\\ProgramData\\Longarm\\longarmd, else ~/.longarm); env LONGARM_DATA_DIR")
	c.Flags().StringVar(&name, "name", "", "worker name (default: hostname)")
	c.Flags().StringVar(&fp, "ca-fingerprint", "", "expected sha256 fingerprint of the hub CA (longarm-hub ca show); default: fetched from https://<hub>/ca.crt")
	c.Flags().StringVar(&role, "role", "worker", "worker, or both: also this user's client device, with the same approval")
	c.Flags().StringArrayVar(&labels, "label", nil, "label for the worker, recorded by the hub: KEY=VALUE[,KEY=VALUE...] (repeatable; later: longarm worker label)")
	c.Flags().StringVar(&configDir, "config-dir", client.DefaultConfigDir(), "--role both: longarm's config dir; env LONGARM_CONFIG_DIR")
	c.Flags().StringVar(&deviceName, "device-name", "", "--role both: the device's name (default: <user>-<hostname>)")
	c.Flags().BoolVar(&az, "az", false, "sign in with the Azure CLI on this machine (az login) instead of a code")
	c.Flags().StringVar(&scope, "scope", "", "--az: token scope to request (default: the hub's)")
	c.Flags().BoolVar(&noBrowser, "no-browser", false, "only print the /link address and the code; don't open a browser here")
	c.Flags().BoolVar(&force, "force", false, "enroll again although a certificate exists")
	return c
}

// localClient connects to the running longarmd's local control API.
func localClient(dataDir string) (pb.LocalWorkerClient, func(), error) {
	cc, err := localapi.DialFile(filepath.Join(dataDir, longarmd.LocalAddrFile), "longarmd")
	if err != nil {
		return nil, nil, err
	}
	return pb.NewLocalWorkerClient(cc), func() { cc.Close() }, nil
}

func localCmds() []*cobra.Command {
	var dataDir string
	var asJSON bool
	var reason string
	withClient := func(fn func(ctx context.Context, c pb.LocalWorkerClient) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			c, closeFn, err := localClient(dataDir)
			if err != nil {
				return err
			}
			defer closeFn()
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			return fn(ctx, c)
		}
	}
	show := func(s *pb.WorkerStatus) error {
		if asJSON {
			b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(s)
			fmt.Println(string(b))
			return nil
		}
		fmt.Printf("worker      %s (longarmd %s, pid %d)\n", s.Name, s.Version, s.Pid)
		fmt.Printf("data dir    %s\nendpoints   %s\n", s.DataDir, strings.Join(s.Endpoints, " "))
		hub := "not configured"
		if s.Hub.GetAddress() != "" {
			hub = s.Hub.Address + " disconnected"
			if s.Hub.Connected {
				hub = s.Hub.Address + " connected (" + s.Hub.NodeState + ")"
			}
			if s.Hub.Error != "" {
				hub += ": " + s.Hub.Error
			}
		}
		fmt.Printf("hub         %s\n", hub)
		st := "accepting work"
		if s.State.GetDraining() {
			st = "draining: " + s.State.Reason
		}
		fmt.Printf("state       %s\njobs        %d running, %d sessions\n", st, s.JobsRunning, s.SessionsRunning)
		if s.CertNotAfter != nil {
			fmt.Printf("certificate expires %s\n", s.CertNotAfter.AsTime().Local().Format(time.RFC3339))
		}
		if tc := s.Toolchain; tc != nil && tc.State != "none" {
			b, _ := json.Marshal(tc.Versions)
			fmt.Printf("toolchain   %s r%d %s %s %s\n", tc.Profile, tc.Revision, tc.State, tc.Error, b)
		}
		if l := s.Limits; l.GetMaxJobs() > 0 || l.GetReservedMemGb() > 0 {
			fmt.Printf("limits      max jobs %d, reserved memory %.1f GB\n", l.MaxJobs, l.ReservedMemGb)
		}
		fmt.Printf("revoked     %d certificates\n", s.RevokedCerts)
		switch fw := s.Firewall; {
		case fw == nil: // not Windows
		case fw.Ok:
			fmt.Printf("firewall    ok (inbound allow rule for %s)\n", fw.Program)
		default:
			fmt.Printf("firewall    PROBLEM: %s\n            fix (elevated): %s\n", fw.Problem, fw.Fix)
		}
		return nil
	}
	status := &cobra.Command{
		Use: "status", Short: "Show the running longarmd's state (local control API)", Args: cobra.NoArgs,
		RunE: withClient(func(ctx context.Context, c pb.LocalWorkerClient) error {
			s, err := c.Status(ctx, &pb.LocalStatusRequest{})
			if err != nil {
				return err
			}
			return show(s)
		}),
	}
	drain := &cobra.Command{
		Use: "drain", Short: "Stop accepting new work from the scheduler (running jobs continue)", Args: cobra.NoArgs,
		RunE: withClient(func(ctx context.Context, c pb.LocalWorkerClient) error {
			s, err := c.SetDrain(ctx, &pb.LocalDrainRequest{Draining: true, Reason: reason})
			if err != nil {
				return err
			}
			return show(s)
		}),
	}
	undrain := &cobra.Command{
		Use: "undrain", Short: "Accept new work again", Args: cobra.NoArgs,
		RunE: withClient(func(ctx context.Context, c pb.LocalWorkerClient) error {
			s, err := c.SetDrain(ctx, &pb.LocalDrainRequest{Draining: false})
			if err != nil {
				return err
			}
			return show(s)
		}),
	}
	tc := &cobra.Command{Use: "toolchain", Short: "Toolchain profile status / apply now"}
	tcStatus := &cobra.Command{
		Use: "status", Args: cobra.NoArgs, Short: "Show the toolchain state",
		RunE: withClient(func(ctx context.Context, c pb.LocalWorkerClient) error {
			s, err := c.Status(ctx, &pb.LocalStatusRequest{})
			if err != nil {
				return err
			}
			b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(s.Toolchain)
			fmt.Println(string(b))
			return nil
		}),
	}
	tcApply := &cobra.Command{
		Use: "apply", Args: cobra.NoArgs, Short: "Reconcile the assigned profile now (still waits for running jobs)",
		RunE: withClient(func(ctx context.Context, c pb.LocalWorkerClient) error {
			r, err := c.ToolchainApply(ctx, &pb.ToolchainApplyRequest{})
			if err != nil {
				return err
			}
			fmt.Println(r.State, r.Error)
			return nil
		}),
	}
	tc.AddCommand(tcStatus, tcApply)
	events := &cobra.Command{
		Use: "events", Short: "Stream local events (jobs, hub link, drain, toolchain)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, closeFn, err := localClient(dataDir)
			if err != nil {
				return err
			}
			defer closeFn()
			st, err := c.Events(cmd.Context(), &pb.LocalEventsRequest{})
			if err != nil {
				return err
			}
			for {
				ev, err := st.Recv()
				if err != nil {
					return err
				}
				b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(ev)
				fmt.Println(string(b))
			}
		},
	}
	cmds := []*cobra.Command{status, drain, undrain, tc, events}
	for _, c := range []*cobra.Command{status, drain, undrain, tcStatus, tcApply, events} {
		c.Flags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory of the running longarmd; env LONGARM_DATA_DIR")
	}
	status.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	drain.Flags().StringVar(&reason, "reason", "", "why (shown to the hub)")
	return cmds
}
