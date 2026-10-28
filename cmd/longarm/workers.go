package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/fwcheck"
	"github.com/lubobill1990/longarm/internal/machine"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// lsHubGrace: longarm ls asks the hub first; workers known only from the hub
// cache are probed when it hasn't answered within this time.
var lsHubGrace = 250 * time.Millisecond

func lsCmd() *cobra.Command {
	var asJSON, direct bool
	var output string
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "ls [worker...]",
		Short: "Show the workers: from the hub if reachable, else by querying workers directly (in parallel)",
		Long: `Show the workers: from the hub if reachable, else by querying workers
directly (in parallel). One row per machine: a worker's admin worker (runs
commands as SYSTEM, <worker>-admin) shows in its ADMIN column, an admin
worker alone keeps its own row. Offline workers say when they were last
seen.

The default table is WORKER STATE OS CPU% MEM JOBS LABELS (ADMIN, OWNER when
any worker has one); -o wide adds Claude Code / Copilot CLI sessions, free
disk, uptime, the top processes and the path this client reaches each
worker by. --json prints every worker's hub record (or metrics) by name.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			type row = lsRow
			var rows []row
			fromHub := map[string]bool{}

			// Ask the hub and, at the same time, the workers we already know
			// (workers.toml, the hub cache): with the hub down, longarm ls costs
			// one hub connect timeout at most, not hub timeout + worker probes.
			type hubAnswer struct {
				nodes []*pb.NodeInfo
				ok    bool
			}
			var hubCh chan hubAnswer
			if !direct && s.cfg.Hub.Enabled() && !noHub { // a known-down hub answers at once
				hubCh = make(chan hubAnswer, 1)
				go func() {
					nodes, ok := s.hubNodes(cmd.Context(), false)
					hubCh <- hubAnswer{nodes, ok}
				}()
			}
			type probeJob struct {
				r      row
				cancel context.CancelFunc
				done   chan struct{}
				toml   bool
			}
			var probes []*probeJob
			probing := map[string]bool{}
			startProbe := func(name string, w *client.Worker, toml bool) *probeJob {
				if probing[name] {
					return nil
				}
				probing[name] = true
				ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
				p := &probeJob{r: row{name: name, state: "ok"}, cancel: cancel, done: make(chan struct{}), toml: toml}
				probes = append(probes, p)
				go func() {
					defer close(p.done)
					defer cancel()
					var conn *client.Conn
					var err error
					if w != nil {
						conn, err = s.dialer.Dial(*w)
					} else {
						conn, err = s.conn(name)
					}
					if err != nil {
						p.r.err = err
						return
					}
					defer conn.Close()
					p.r.snap, p.r.err = conn.Metrics().Snapshot(ctx, &pb.MetricsRequest{})
					p.r.path = conn.Path().String()
					if legs := conn.Legs(); len(legs) > 1 {
						p.r.path += " [" + client.RaceSummary(legs) + "]"
					}
				}()
				return p
			}
			known := map[string]client.Worker{}
			isTOML := map[string]bool{}
			for _, w := range s.cfg.Workers {
				known[w.Name], isTOML[w.Name] = w, true
			}
			if direct {
				for _, w := range s.knownWorkers(cmd.Context()) { // the hub's live list (or its cache)
					if _, ok := known[w.Name]; !ok {
						known[w.Name] = w
					}
				}
			} else if s.cfg.Hub.Enabled() && !noHub {
				for _, n := range s.cachedNodes() {
					if _, ok := known[n.Name]; ok || n.State == "revoked" || n.State == "pending" {
						continue
					}
					if w, ok := workerFromNode(n); ok {
						known[n.Name] = w
					}
				}
			}
			// workers.toml entries are probed at once (the hub doesn't know
			// them all); workers known only from the hub cache once the hub
			// is slow to answer (lsHubGrace) - a hub that answers quickly
			// costs no worker dials.
			var unresolved []string // named workers we know nothing about yet
			var cached []string
			want := args
			if len(want) == 0 {
				for _, w := range s.cfg.Workers {
					want = append(want, w.Name)
				}
				var rest []string
				for n := range known {
					if !isTOML[n] {
						rest = append(rest, n)
					}
				}
				sort.Strings(rest)
				want = append(want, rest...)
			}
			for _, n := range want {
				w, ok := known[n]
				switch {
				case !ok:
					unresolved = append(unresolved, n)
				case isTOML[n] || hubCh == nil:
					startProbe(n, &w, isTOML[n])
				default:
					cached = append(cached, n)
				}
			}
			startCached := func() {
				for _, n := range cached {
					w := known[n]
					startProbe(n, &w, false)
				}
			}

			hubOK := false
			if hubCh == nil {
				startCached()
			} else {
				var a hubAnswer
				select {
				case a = <-hubCh:
				case <-time.After(lsHubGrace):
					startCached()
					a = <-hubCh
				}
				if a.ok {
					hubOK = true
					for _, n := range a.nodes {
						if len(args) > 0 && !contains(args, n.Name) {
							continue
						}
						st := "online"
						switch {
						case n.State != "approved":
							st = n.State
						case !n.Online:
							st = "offline"
						case n.Draining:
							st = "draining"
						}
						path := "hub"
						if c, ok := s.dialer.Sticky(n.Name); ok {
							path = "hub; last " + c.Path
						}
						rows = append(rows, row{name: n.Name, state: st, snap: n.Metrics, path: path, node: n})
						fromHub[n.Name] = true
					}
				} else {
					startCached()
				}
			}
			// With the hub's answer, only workers.toml entries (and named
			// workers) it doesn't list are probed; the rest is cancelled.
			var kept []*probeJob
			for _, p := range probes {
				if hubOK && (fromHub[p.r.name] || len(args) == 0 && !p.toml) {
					p.cancel()
					continue
				}
				kept = append(kept, p)
			}
			for _, n := range unresolved {
				if !fromHub[n] {
					if p := startProbe(n, nil, false); p != nil {
						kept = append(kept, p)
					}
				}
			}
			if len(rows) == 0 && len(kept) == 0 {
				return noWorkersError(s)
			}
			for _, p := range kept {
				<-p.done
				rows = append(rows, p.r)
			}
			if output != "" && output != "wide" {
				return usagef("-o %s: the only other format is -o wide (or --json)", output)
			}
			if asJSON {
				out := map[string]any{}
				for _, r := range rows {
					switch {
					case r.node != nil:
						b, _ := protojson.Marshal(r.node)
						out[r.name] = json.RawMessage(b)
					case r.err != nil:
						out[r.name] = map[string]string{"error": r.err.Error()}
					default:
						b, _ := protojson.Marshal(r.snap)
						out[r.name] = json.RawMessage(b)
					}
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			failed := writeLsTable(os.Stdout, rows, output == "wide", time.Now())
			var relayed []string
			for _, r := range rows {
				if m := relayedPathRE.FindStringSubmatch(r.path); m != nil && r.err == nil {
					relayed = append(relayed, r.name+" ("+m[0]+")")
				}
			}
			if len(relayed) > 0 { // checked only then: it reads the firewall rules
				if note := firewallNote(fwcheck.Self(), relayed); note != "" {
					fmt.Fprintln(os.Stderr, note)
				}
			}
			if failed == len(rows) {
				return &exitError{code: exitLongarmError}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	c.Flags().StringVarP(&output, "output", "o", "", "table format: wide (sessions, disk, uptime, top processes, path)")
	c.Flags().BoolVar(&direct, "direct", false, "query the workers directly even if the hub is reachable")
	c.Flags().DurationVar(&timeout, "timeout", 8*time.Second, "per-worker timeout")
	return c
}

// foldMachines is longarm ls' one row per machine: names without the
// admin workers that pair with a listed session worker (package machine),
// and adminOf: session worker -> its admin worker.
func foldMachines(names []string, nodes map[string]*pb.NodeInfo) (shown []string, adminOf map[string]string) {
	var list []*pb.NodeInfo
	for _, n := range names {
		if nodes[n] != nil {
			list = append(list, nodes[n])
		}
	}
	pairs := machine.Pairs(list) // admin -> session
	adminOf = map[string]string{}
	for a, s := range pairs {
		adminOf[s] = a
	}
	for _, n := range names {
		if _, ok := pairs[n]; !ok {
			shown = append(shown, n)
		}
	}
	return shown, adminOf
}

func oneLine(err error) string {
	if msg, ok := client.TrustRefusal(err); ok {
		return msg
	}
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func infoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info <worker>",
		Short: "Show worker info and the caller identity it sees",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			info, err := conn.Node().Info(ctx, &pb.InfoRequest{})
			if err != nil {
				return err
			}
			b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(info)
			fmt.Println(string(b))
			fmt.Println("path:", conn.Path())
			fmt.Println("policy:", s.dialer.Policy())
			fmt.Println("legs:", client.RaceSummary(conn.Legs()))
			return nil
		},
	}
}

// relayedPathRE finds a relayed path in longarm ls' PATH column
// ("longnet/relay 2.4ms (...)", "hub; last tailscale/derp ...").
var relayedPathRE = regexp.MustCompile(`\b(longnet|tailscale)/(relay|derp|peer-relay)\b`)
