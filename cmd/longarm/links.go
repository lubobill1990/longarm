package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/nodeid"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/portlink"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Port links (docs/usage.md "Port links"): processes on worker A reach a
// port of worker B as 127.0.0.1:<port> on A, without traffic through this
// machine. The hub keeps the links; A and B carry the data.
//
//	longarm link create B:8080 --on A [--port 18080] [--ttl 8h] [--job <id>]
//	longarm link ls [--all] [--json]
//	longarm link rm <id>...

func linkCmd() *cobra.Command {
	c := &cobra.Command{Use: "link", Short: "Port links between workers: reach worker B's port from worker A (joint debugging)"}
	c.AddCommand(linkCreateCmd(), linkListCmd(), linkRemoveCmd())
	return c
}

// parseLinkTarget parses "worker:port".
func parseLinkTarget(s string) (string, int32, error) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return "", 0, fmt.Errorf("target %q: want <worker>:<port>", s)
	}
	p, err := strconv.Atoi(s[i+1:])
	if err != nil || p <= 0 || p > 65535 {
		return "", 0, fmt.Errorf("target %q: bad port", s)
	}
	return s[:i], int32(p), nil
}

func linkCreateCmd() *cobra.Command {
	var on, job string
	var port int
	var ttl time.Duration
	var asJSON bool
	c := &cobra.Command{
		Use:   "create <worker>:<port> --on <worker>",
		Short: "Make <worker>:<port> reachable as 127.0.0.1:<port> on the --on worker (for your jobs there)",
		Long: `Make a port of one worker reachable on another: your processes on the --on worker
connect to 127.0.0.1:<--port> and reach 127.0.0.1:<port> on the target worker. Traffic goes
worker to worker (direct, or through the hub's relay), never through this machine.

Jobs you start on the --on worker get LONGARM_LINKS ("B:8080=127.0.0.1:18080,...") and
LONGARM_LINK_<WORKER>_<PORT> in their environment. The link ends after --ttl, when --job ends,
or with longarm link rm.`,
		Example: "  longarm link create backend-box:8080 --on frontend-box --port 18080",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			to, p, err := parseLinkTarget(args[0])
			if err != nil {
				return err
			}
			if on == "" {
				return fmt.Errorf("--on <worker>: the worker to listen on")
			}
			if port < 0 || port > 65535 {
				return fmt.Errorf("--port %d: want 1-65535", port)
			}
			req := &pb.CreatePortLinkRequest{From: on, To: to, Port: p, ListenPort: int32(port), JobId: job}
			if ttl > 0 {
				req.Ttl = durationpb.New(ttl)
			}
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				// targets (names, IDs, prefixes) -> the names the workers'
				// certificates have, which the signed consent and the
				// workers go by
				if nodes, ok := s.hubNodes(ctx, false); ok {
					for _, p := range []*string{&req.From, &req.To} {
						t, err := nodeid.ParseTarget(*p)
						if err != nil {
							return usagef("%s", err.Error())
						}
						n, err := resolveNode(t, nodes, "worker")
						if errors.Is(err, nodeid.ErrAmbiguous) {
							return usagef("%s", err.Error())
						}
						if err == nil {
							*p = cmp.Or(n.CertName, n.Name)
						}
					}
				}
				if err := signLink(s.cfg.Dir, req); err != nil {
					return err
				}
				l, err := h.Fleet().CreatePortLink(ctx, req)
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(l)
				}
				fmt.Printf("%s: %s:%d reachable on %s at 127.0.0.1:%d until %s\n", l.Id, l.To, l.Port, l.From, l.ListenPort,
					l.ExpiresAt.AsTime().Local().Format("2006-01-02 15:04"))
				fmt.Printf("jobs of yours on %s: %s=127.0.0.1:%d\n", l.From, linkEnvName(l), l.ListenPort)
				return nil
			})
		},
	}
	f := c.Flags()
	f.StringVar(&on, "on", "", "the worker whose processes use the link (listens on 127.0.0.1)")
	f.IntVar(&port, "port", 0, "port to listen on there (default: the target port)")
	f.DurationVar(&ttl, "ttl", 0, "lifetime (default: the hub's [links] default_ttl, 8h)")
	f.StringVar(&job, "job", "", "remove the link when this job on the --on worker ends")
	f.BoolVar(&asJSON, "json", false, "JSON output")
	return c
}

// linkEnvName is the per-link job variable.
func linkEnvName(l *pb.PortLink) string { return portlink.EnvName(l.To, l.Port) }

func linkListCmd() *cobra.Command {
	var all, asJSON bool
	c := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List your port links with their state (listening / error), connections and bytes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, _ *session, h *hubclient.Conn) error {
				r, err := h.Fleet().ListPortLinks(ctx, &pb.ListPortLinksRequest{All: all})
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(r)
				}
				if len(r.Links) == 0 {
					fmt.Println("no port links")
					return nil
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tOWNER\tON\tLISTEN\tTARGET\tSTATE\tCONNS\tBYTES OUT/IN\tEXPIRES\tJOB")
				for _, l := range r.Links {
					s := l.Status
					state := s.GetState()
					if s.GetError() != "" {
						state += " (" + s.GetError() + ")"
					}
					if s.GetPath() != "" && s.GetState() == "listening" {
						state += " via " + s.GetPath()
					}
					job := l.JobId
					if job == "" {
						job = "-"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t127.0.0.1:%d\t%s:%d\t%s\t%d (%d open, %d refused)\t%s / %s\t%s\t%s\n",
						l.Id, l.Owner, l.From, l.ListenPort, l.To, l.Port, state, s.GetConnections(), s.GetActive(), s.GetRefused(),
						humanSize(s.GetBytesSent()), humanSize(s.GetBytesReceived()), time.Until(l.ExpiresAt.AsTime()).Round(time.Minute), job)
				}
				return tw.Flush()
			})
		},
	}
	c.Flags().BoolVar(&all, "all", false, "every user's links (admins)")
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	return c
}

func linkRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <id>...",
		Aliases: []string{"remove"},
		Short:   "Remove port links (open connections through them are cut)",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withHub(cmd, func(ctx context.Context, _ *session, h *hubclient.Conn) error {
				for _, id := range args {
					l, err := h.Fleet().RemovePortLink(ctx, &pb.RemovePortLinkRequest{Id: id})
					if err != nil {
						return fmt.Errorf("%s: %w", id, err)
					}
					fmt.Printf("removed %s (%s:%d on %s at 127.0.0.1:%d)\n", l.Id, l.To, l.Port, l.From, l.ListenPort)
				}
				return nil
			})
		},
	}
}

// defaultSignedLinkTTL is a signed link's lifetime without --ttl (the
// consent fixes the expiry; the hub's own default is 8h too).
const defaultSignedLinkTTL = 8 * time.Hour

// signLink adds this device's consent to req when the device is endorsed
// (end-to-end trust, docs/security.md "Port links"): workers that pin its
// user's key accept a link only with it, so the hub alone can't create one.
// Without an endorsement the request goes unsigned (workers without trust
// don't need it).
func signLink(cfgDir string, req *pb.CreatePortLinkRequest) error {
	st := client.TrustStore(cfgDir)
	e, err := st.Endorsement()
	if err != nil {
		return fmt.Errorf("end-to-end trust: %w", err)
	}
	if e == nil || e.Kind != pki.KindDevice {
		return nil
	}
	key, err := st.NodeKey()
	if err != nil {
		return fmt.Errorf("end-to-end trust: %w", err)
	}
	if req.ListenPort == 0 {
		req.ListenPort = req.Port
	}
	ttl := defaultSignedLinkTTL
	if req.Ttl != nil {
		ttl = req.Ttl.AsDuration()
	}
	c, err := portlink.SignConsent(key, e, &pb.PortLinkSpec{Id: portlink.NewID(), From: req.From, To: req.To, Port: req.Port,
		ListenPort: req.ListenPort, ExpiresUnix: time.Now().Add(ttl).Unix(), JobId: req.JobId})
	if err != nil {
		return err
	}
	req.Consent = c
	return nil
}
