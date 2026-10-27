package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/diag"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pii"
	"github.com/lubobill1990/longarm/internal/platform"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func diagCmd() *cobra.Command {
	var out, note, worker string
	var since time.Duration
	var upload, yes, noProbe bool
	c := &cobra.Command{
		Use:   "diag",
		Short: "Collect a diagnostic bundle of this device (logs, versions, configuration, connectivity; personal data and secrets removed) for a bug report",
		Long: `Collect what a maintainer needs to look into a problem on this device into a
zip: versions, OS, uptime, free disk and memory; the log of longarm daemon
(and the tray) of the last --since; workers.toml and hub.toml without
secret values; certificate metadata (never keys); the trust policy summary;
a quick connectivity probe (is the hub reachable, which transport leg
reaches each known worker). Names of people and machines are replaced by
pseudonyms, IP and e-mail addresses and anything that looks like a secret
by tags. manifest.json lists every file and why it is there.

The zip is always written locally (--out). --upload sends it to the hub,
which stores it for the fleet's maintainers and prints a report id to put
into the bug report.

A worker's bundle is collected on the worker:
  longarm exec <worker> -- longarmd diag --upload --yes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if worker != "" {
				return fmt.Errorf("a worker's diagnostics are collected on the worker itself; run:\n  longarm exec %s -- longarmd diag --upload --yes\n(the report id it prints goes into the bug report)", worker)
			}
			if out == "" {
				out = diag.DefaultOut("longarm", time.Now())
			}
			b, s := collectClient(cmd.Context(), configDir, since, !noProbe)
			if s != nil {
				defer s.close()
			}
			var dial func(ctx context.Context) (grpc.ClientConnInterface, func(), error)
			if s != nil && s.cfg.Hub.Enabled() && !noHub {
				dial = func(ctx context.Context) (grpc.ClientConnInterface, func(), error) {
					h := s.hub()
					if h == nil {
						return nil, nil, errors.New("hub unavailable")
					}
					if err := h.WaitReady(ctx, 15*time.Second); err != nil {
						return nil, nil, err
					}
					return h.ClientConn, func() {}, nil
				}
			}
			_, err := diag.Finish(cmd.Context(), b, diag.FinishOptions{Out: out, Upload: upload, Yes: yes, Note: note, Dial: dial})
			return err
		},
	}
	c.Flags().StringVar(&out, "out", "", "zip to write (default longarm-diag-<time>.zip in the current directory)")
	c.Flags().BoolVar(&upload, "upload", false, "upload the bundle to the hub and print its report id")
	c.Flags().BoolVar(&yes, "yes", false, "upload without asking")
	c.Flags().StringVar(&note, "note", "", "a short description sent with the upload (scrubbed)")
	c.Flags().DurationVar(&since, "since", diag.DefaultSince, "log window")
	c.Flags().BoolVar(&noProbe, "no-probe", false, "skip the connectivity probe")
	c.Flags().StringVar(&worker, "worker", "", "a worker's diagnostics: prints how to collect them on the worker")
	return c
}

// probeResult is connectivity.json.
type probeResult struct {
	Hub     map[string]any   `json:"hub"`
	Workers []map[string]any `json:"workers"`
}

const maxProbeWorkers = 32

// probe checks the hub and every known worker with short timeouts.
func probe(ctx context.Context, s *session) (*probeResult, []client.Worker) {
	r := &probeResult{Hub: map[string]any{"configured": s.cfg.Hub.Enabled()}}
	if s.cfg.Hub.Enabled() && !noHub {
		r.Hub["address"] = s.cfg.Hub.Address
		start := time.Now()
		if h := s.hub(); h == nil {
			r.Hub["reachable"], r.Hub["error"] = false, fmt.Sprint(s.hubErr)
		} else if err := h.WaitReady(ctx, 5*time.Second); err != nil {
			r.Hub["reachable"], r.Hub["error"] = false, err.Error()
		} else {
			r.Hub["reachable"], r.Hub["connect"] = true, time.Since(start).Round(time.Millisecond).String()
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if w, err := h.Fleet().Whoami(cctx, &pb.WhoamiRequest{}); err != nil {
				r.Hub["whoami_error"] = err.Error()
			} else {
				r.Hub["hub_version"] = w.GetHubVersion()
			}
			cancel()
		}
	}
	quietHub = true
	ws := s.knownWorkers(ctx)
	if len(ws) > maxProbeWorkers {
		ws = ws[:maxProbeWorkers]
	}
	results := make([]map[string]any, len(ws))
	var wg sync.WaitGroup
	for i, w := range ws {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := map[string]any{"worker": w.Name, "transport": w.Transport, "legs": len(w.Alt) + 1}
			results[i] = m
			conn, err := s.dialer.Dial(w)
			if err != nil {
				m["error"] = err.Error()
				return
			}
			defer conn.Close()
			cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			start := time.Now()
			info, err := conn.Node().Info(cctx, &pb.InfoRequest{})
			if err != nil {
				m["error"] = oneLine(err)
			} else {
				m["took"] = time.Since(start).Round(time.Millisecond).String()
				m["version"] = info.GetVersion()
				m["path"] = conn.Path().String()
			}
			if legs := conn.Legs(); len(legs) > 0 {
				m["race"] = client.RaceSummary(legs)
			}
		}()
	}
	wg.Wait()
	r.Workers = results
	return r, ws
}

// collectClient builds a client device's bundle. The session (nil when the
// configuration doesn't load) is for the upload.
func collectClient(ctx context.Context, dir string, since time.Duration, doProbe bool) (*diag.Builder, *session) {
	tlsDir := filepath.Join(dir, client.TLSDir)
	id, caDER := diag.ReadIdentity(tlsDir)
	names := diag.Names{}
	names.Add(pii.KindNode, id.Name)
	names.Add(pii.KindUser, id.User)
	names.Add(pii.KindUser, diag.TrustNames(filepath.Join(dir, "trust"))...)

	s, sessErr := openSession()
	var pr *probeResult
	if sessErr == nil {
		for _, w := range s.cfg.Workers {
			names.Add(pii.KindNode, w.Name, w.CertName)
			if h := hostOf(w.Address); h != "" {
				names.Add(pii.KindHost, h)
			}
		}
		for _, n := range s.cachedNodes() {
			names.Add(pii.KindNode, n.Name)
		}
		if h := hostOf(s.cfg.Hub.Address); h != "" {
			names.Add(pii.KindHost, h)
		}
		if doProbe {
			var ws []client.Worker
			pr, ws = probe(ctx, s)
			for _, w := range ws {
				names.Add(pii.KindNode, w.Name, w.CertName)
			}
		}
	}
	sc := names.Scrubber(caDER)
	b := diag.NewBuilder("longarm", sc)
	b.Since = since
	if id.Name != "" {
		b.Node = sc.Hash(pii.KindNode, id.Name)
	}
	b.Options = map[string]any{"probe": doProbe}
	if sessErr != nil {
		b.Errorf("configuration: %v", sessErr)
	}

	b.AddJSON("system.json", "versions, OS, uptime, memory, free disk", diag.SystemInfo("longarm", map[string]string{"config_dir": dir}))
	logs := &diag.LogSet{B: b, Since: since, Budget: diag.DefaultLogCap}
	logs.Add(filepath.Join(dir, "daemon.log"), "logs", "longarm daemon's log (--since window, newest first)")
	logs.Add(filepath.Join(dir, "tray.log"), "logs", "the tray's log")
	b.AddTOML(filepath.Join(dir, client.WorkersFile), "config/"+client.WorkersFile, "workers and transport settings")
	b.AddTOML(filepath.Join(dir, hubclient.File), "config/"+hubclient.File, "where the hub is")
	for _, f := range []string{client.SettingsFile, client.CacheFile, hubStateFile, hubCacheFile} {
		b.AddSmallFile(filepath.Join(dir, f), "state/"+f, "client state (transport preferences and cache, hub state)")
	}
	b.AddCertDir(tlsDir, "tls", "certs.json")
	b.AddCertDir(filepath.Join(dir, client.CADir), "ca", "ca-certs.json")
	b.AddJSON("trust.json", "end-to-end trust: policy summary, endorsement (no keys)", b.TrustSummary(filepath.Join(dir, "trust"), true))
	if pr != nil {
		b.AddJSON("connectivity.json", "probe: hub reachable, transport leg per known worker", pr)
	}
	b.AddCrashFiles(filepath.Join(dir, "telemetry"), "crash")
	if m, _ := filepath.Glob(filepath.Join(dir, hubclient.TSNetBase+"*")); len(m) > 0 {
		b.Exclude(hubclient.TSNetBase+"*/", "tailnet node state (keys): never collected")
	}
	if _, err := os.Stat(filepath.Join(dir, hubclient.PendingKeyFile)); err == nil {
		b.Exclude(hubclient.PendingKeyFile, "key material: never collected")
	}
	if sessErr != nil {
		return b, nil
	}
	return b, s
}

// hostOf is the host of an address when it is a name (IPs are scrubbed
// anyway).
func hostOf(addr string) string {
	if i := strings.Index(addr, "://"); i >= 0 {
		addr = addr[i+3:]
	}
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		h = addr
	}
	if h == "" || net.ParseIP(h) != nil || h == "localhost" {
		return ""
	}
	return h
}

// adminDiagCmd is `longarm admin diag ls|get`.
func adminDiagCmd() *cobra.Command {
	c := &cobra.Command{Use: "diag", Short: "Diagnostic bundles uploaded with `longarm diag --upload` / `longarmd diag --upload`"}
	var since time.Duration
	var limit int
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List uploaded diagnostic bundles (newest first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				resp, err := pb.NewDiagnosticsClient(h.ClientConn).List(ctx, &pb.DiagListRequest{Since: durationpb.New(since), Limit: int32(limit)})
				if err != nil {
					return err
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "REPORT\tCREATED\tCOMPONENT\tVERSION\tOS\tNODE\tROLE\tSIZE\tNOTE")
				sort.SliceStable(resp.Reports, func(i, j int) bool {
					return resp.Reports[i].GetCreated().AsTime().After(resp.Reports[j].GetCreated().AsTime())
				})
				for _, r := range resp.Reports {
					created := "-"
					if r.Created != nil {
						created = r.Created.AsTime().Local().Format("2006-01-02 15:04")
					}
					note := strings.Join(strings.Fields(r.Note), " ")
					if len(note) > 60 {
						note = note[:57] + "..."
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s/%s\t%s\t%s\t%s\t%s\n", r.ReportId, created, r.Component, r.Version, r.Os, r.Arch,
						r.Node, r.UploaderRole, diag.HumanBytes(r.Size), note)
				}
				return tw.Flush()
			})
		},
	}
	ls.Flags().DurationVar(&since, "since", 7*24*time.Hour, "uploaded within")
	ls.Flags().IntVar(&limit, "limit", 100, "at most this many")

	var out string
	get := &cobra.Command{
		Use:   "get <report-id>",
		Short: "Download a diagnostic bundle (a zip)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := strings.ToUpper(strings.TrimSpace(args[0]))
			if _, err := diag.BlobPath(id); err != nil {
				return err
			}
			if out == "" {
				out = id + ".zip"
			}
			return withHub(cmd, func(_ context.Context, s *session, h *hubclient.Conn) error {
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
				defer cancel()
				tmp := out + ".part"
				f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
				if err != nil {
					return err
				}
				m, err := diag.Download(ctx, h.ClientConn, id, f)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					os.Remove(tmp)
					return err
				}
				if err := platform.ReplaceFile(tmp, out); err != nil {
					os.Remove(tmp)
					return err
				}
				fmt.Fprintf(os.Stderr, "%s: %s %s from %s (%s), %s\n", id, m.Component, m.Version, m.Node, m.UploaderRole, diag.HumanBytes(m.Size))
				if m.Note != "" {
					fmt.Fprintf(os.Stderr, "note: %s\n", m.Note)
				}
				fmt.Println(out)
				return nil
			})
		},
	}
	get.Flags().StringVar(&out, "out", "", "file to write (default <report-id>.zip)")
	c.AddCommand(ls, get)
	return c
}
