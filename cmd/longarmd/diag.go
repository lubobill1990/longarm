package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/lubobill1990/longarm/internal/diag"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/pii"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/tenancy"
	"github.com/lubobill1990/longarm/internal/update"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func diagCmd() *cobra.Command {
	var dataDir, out, note string
	var since time.Duration
	var upload, yes, includeAudit bool
	var includeJobs, logFiles []string
	c := &cobra.Command{
		Use:   "diag",
		Short: "Collect a diagnostic bundle (logs, versions, configuration, state; personal data and secrets removed) for a bug report",
		Long: `Collect what a maintainer needs to look into a problem on this worker into a
zip: versions, OS, uptime, free disk and memory; longarmd's log of the last
--since (rotated files too, at most 20 MiB, newest first); longarmd.toml and
hub.toml without secret values; certificate metadata (never keys); trust,
access and isolation policy summaries; job and session metadata (ids,
states, times, exit codes - no command lines); update.json. Names of
people and machines are replaced by pseudonyms, IP and e-mail addresses
and anything that looks like a secret by tags. manifest.json lists every
file and why it is there.

The zip is always written locally (--out). --upload sends it to the hub,
which stores it for the fleet's maintainers and prints a report id to put
into the bug report.

--include-audit adds the audit log (it contains command lines);
--include-job ID adds that job's command line and the tail of its output.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dataDir, err := filepath.Abs(dataDir)
			if err != nil {
				return err
			}
			if len(includeJobs) > 0 {
				fmt.Fprintf(os.Stderr, "WARNING: --include-job adds the command line and the last %s of the output of %s.\n"+
					"Outputs can contain secrets the scrubber doesn't recognise: look at the zip before you upload it.\n",
					diag.HumanBytes(diag.JobOutputTail), strings.Join(includeJobs, ", "))
			}
			b, cfg := collectWorker(cmd.Context(), workerDiagOptions{dataDir: dataDir, since: since, includeAudit: includeAudit,
				includeJobs: includeJobs, logFiles: logFiles})
			if out == "" {
				out = diag.DefaultOut("longarmd", time.Now())
			}
			var dial func(ctx context.Context) (grpc.ClientConnInterface, func(), error)
			if cfg.Hub.Address != "" {
				dial = func(ctx context.Context) (grpc.ClientConnInterface, func(), error) {
					bundle, err := pki.LoadBundle(filepath.Join(dataDir, longarmd.TLSDir))
					if err != nil {
						return nil, nil, err
					}
					hc, err := hubclient.Dial(cfg.Hub.Address, bundle)
					if err != nil {
						return nil, nil, err
					}
					if err := hc.WaitReady(ctx, 15*time.Second); err != nil {
						hc.Close()
						return nil, nil, err
					}
					return hc.ClientConn, func() { hc.Close() }, nil
				}
			}
			_, err = diag.Finish(cmd.Context(), b, diag.FinishOptions{Out: out, Upload: upload, Yes: yes, Note: note, Dial: dial})
			return err
		},
	}
	c.Flags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory")
	c.Flags().StringVar(&out, "out", "", "zip to write (default longarmd-diag-<time>.zip in the current directory)")
	c.Flags().BoolVar(&upload, "upload", false, "upload the bundle to the hub and print its report id")
	c.Flags().BoolVar(&yes, "yes", false, "upload without asking")
	c.Flags().StringVar(&note, "note", "", "a short description sent with the upload (scrubbed)")
	c.Flags().DurationVar(&since, "since", diag.DefaultSince, "log window")
	c.Flags().BoolVar(&includeAudit, "include-audit", false, "include the audit log (it contains command lines)")
	c.Flags().StringArrayVar(&includeJobs, "include-job", nil, "include this job's / session's command line and output tail (repeatable)")
	c.Flags().StringArrayVar(&logFiles, "log-file", nil, "another log file of longarmd to include (e.g. the one given to longarmd run --log-file); repeatable")
	return c
}

type workerDiagOptions struct {
	dataDir      string
	since        time.Duration
	includeAudit bool
	includeJobs  []string
	logFiles     []string
}

// collectWorker builds a worker's bundle.
func collectWorker(ctx context.Context, o workerDiagOptions) (*diag.Builder, longarmd.Config) {
	dd := o.dataDir
	cfg, cfgErr := longarmd.LoadConfig(dd, nil)
	tlsDir := filepath.Join(dd, longarmd.TLSDir)
	id, caDER := diag.ReadIdentity(tlsDir)

	// names the scrubber replaces wherever they appear
	names := diag.Names{}
	names.Add(pii.KindNode, id.Name)
	names.Add(pii.KindUser, id.User)
	names.Add(pii.KindUser, diag.TrustNames(filepath.Join(dd, longarmd.TrustDir))...)
	if cfg.Access != nil {
		names.Add(pii.KindUser, cfg.Access.Users...)
		names.Add(pii.KindUser, cfg.Access.Owners...)
	}
	policy, polErr := tenancy.LoadPolicy(filepath.Join(dd, tenancy.PolicyFile))
	var tp *pb.TenancyPolicy
	if polErr == nil {
		tp = policy.Get()
		for _, u := range tp.GetUsers() {
			names.Add(pii.KindUser, u.Name)
			names.Add(pii.KindUser, u.Teams...)
			names.Add(pii.KindNode, u.Devices...)
		}
	}
	usersDir := cfg.Isolation.UsersDir
	if usersDir == "" {
		usersDir = filepath.Join(dd, "users")
	}
	roots := []diag.JobRoot{{Kind: "job", Dir: filepath.Join(dd, longarmd.JobsDir), Label: "jobs"},
		{Kind: "session", Dir: filepath.Join(dd, longarmd.SessionsDir), Label: "sessions"}}
	accts, _ := os.ReadDir(usersDir)
	for _, a := range accts {
		if !a.IsDir() {
			continue
		}
		names.Add(pii.KindUser, a.Name())
		names.Add(pii.KindUser, diag.ReadLines(filepath.Join(usersDir, a.Name(), "owner"))...)
	}
	names.Add(pii.KindUser, diag.JobOwners(roots)...)
	if h := hubHost(cfg.Hub.Address); h != "" {
		names.Add(pii.KindHost, h)
	}
	sc := names.Scrubber(caDER)
	for _, a := range accts {
		if a.IsDir() {
			for _, k := range []string{"jobs", "sessions"} {
				roots = append(roots, diag.JobRoot{Kind: strings.TrimSuffix(k, "s"), Dir: filepath.Join(usersDir, a.Name(), k),
					Label: "users/" + sc.Hash(pii.KindUser, a.Name()) + "/" + k})
			}
		}
	}

	b := diag.NewBuilder("longarmd", sc)
	b.Since = o.since
	if id.Name != "" {
		b.Node = sc.Hash(pii.KindNode, id.Name)
	}
	b.Options = map[string]any{"include_audit": o.includeAudit, "include_jobs": len(o.includeJobs)}
	if cfgErr != nil {
		b.Errorf("longarmd.toml: %v", cfgErr)
	}
	if polErr != nil {
		b.Errorf("tenancy policy: %v", polErr)
	}

	b.AddJSON("system.json", "versions, OS, uptime, memory, free disk", diag.SystemInfo("longarmd", map[string]string{"data_dir": dd}))
	b.AddJSON("status.json", "the running longarmd's status (local API)", workerStatus(ctx, dd))

	logs := &diag.LogSet{B: b, Since: o.since, Budget: diag.DefaultLogCap}
	logs.Add(filepath.Join(dd, longarmd.LogFileName), "logs", "longarmd's log (--since window, newest first)")
	for i, f := range o.logFiles {
		logs.AddAs(f, "logs", fmt.Sprintf("log-file-%d.log", i+1), "longarmd's log (--log-file)")
	}
	logs.Add(filepath.Join(dd, "update-watch.log"), "logs", "self-update watcher log")
	if o.includeAudit {
		al := &diag.LogSet{B: b, Since: o.since, Budget: diag.AuditCap}
		al.Add(filepath.Join(dd, longarmd.AuditFile), "audit", "--include-audit: the audit log (contains command lines)")
	} else if _, err := os.Stat(filepath.Join(dd, longarmd.AuditFile)); err == nil {
		b.Exclude(longarmd.AuditFile, "contains command lines; add it with --include-audit")
	}

	b.AddTOML(filepath.Join(dd, longarmd.ConfigFile), "config/"+longarmd.ConfigFile, "longarmd's configuration")
	b.AddTOML(filepath.Join(dd, hubclient.File), "config/"+hubclient.File, "where the hub is")
	b.AddCertDir(tlsDir, "tls", "certs.json")
	b.AddJSON("trust.json", "end-to-end trust: policy summary, endorsement (no keys)", b.TrustSummary(filepath.Join(dd, longarmd.TrustDir), false))
	b.AddJSON("policy.json", "effective access, isolation and hub settings; tenancy policy summary", workerPolicy(cfg, tp, sc))
	b.AddJSON("jobs.json", "job and session metadata: ids, states, times, exit codes (no command lines)", b.JobList(roots, o.since, 0))
	for _, id := range o.includeJobs {
		if !b.IncludeJob(roots, id) {
			b.Errorf("--include-job %s: no such job or session", id)
		}
	}
	for _, f := range []string{update.StateFile, "update-skip.json", longarmd.DrainFile, longarmd.LimitsFile, longarmd.EndpointsFile} {
		b.AddSmallFile(filepath.Join(dd, f), "state/"+f, "worker state")
	}
	if rl, err := pki.LoadRevocationList(filepath.Join(dd, longarmd.RevokedFile)); err == nil {
		b.AddJSON("state/revoked.json", "revocation list summary", map[string]any{"version": rl.Version(), "certificates": rl.Len()})
	}
	b.AddCrashFiles(filepath.Join(dd, "telemetry"), "crash")

	for _, f := range []string{longarmd.SSHHostKeyFile, longarmd.AuthorizedKeysFile, hubclient.PendingKeyFile} {
		if _, err := os.Stat(filepath.Join(dd, f)); err == nil {
			b.Exclude(f, "key material: never collected")
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dd, hubclient.TSNetBase+"*")); len(m) > 0 {
		b.Exclude(hubclient.TSNetBase+"*/", "tailnet node state (keys): never collected")
	}
	return b, cfg
}

// hubHost is the hub's host name when it is a name (IPs are scrubbed
// anyway).
func hubHost(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		h = addr
	}
	if h == "" || net.ParseIP(h) != nil || h == "localhost" {
		return ""
	}
	return h
}

func workerStatus(ctx context.Context, dataDir string) any {
	c, closeFn, err := localClient(dataDir)
	if err != nil {
		return map[string]string{"running": "unknown", "error": err.Error()}
	}
	defer closeFn()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	s, err := c.Status(ctx, &pb.LocalStatusRequest{})
	if err != nil {
		return map[string]string{"running": "no answer", "error": err.Error()}
	}
	raw, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(s)
	return jsonRaw(raw)
}

type jsonRaw []byte

func (j jsonRaw) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return j, nil
}

func workerPolicy(cfg longarmd.Config, tp *pb.TenancyPolicy, sc *pii.Scrubber) map[string]any {
	hashAll := func(kind string, l []string) []string {
		out := make([]string, len(l))
		for i, s := range l {
			out[i] = sc.Hash(kind, s)
		}
		return out
	}
	acc := map[string]any{"active": cfg.Access != nil, "hub_job_details": cfg.HubJobDetails}
	if a := cfg.Access; a != nil {
		acc["users"], acc["owners"], acc["admins"] = hashAll(pii.KindUser, a.Users), hashAll(pii.KindUser, a.Owners), a.Admins
	}
	m := map[string]any{
		"access":    acc,
		"isolation": map[string]any{"mode": cfg.Isolation.Mode, "users_dir_set": cfg.Isolation.UsersDir != "", "account_prefix": cfg.Isolation.AccountPrefix, "loopback": cfg.Isolation.Loopback},
		"hub": map[string]any{"address": cfg.Hub.Address, "disabled": cfg.Hub.Disabled, "remote_jobs": cfg.Hub.RemoteJobs,
			"labels": cfg.Hub.Labels, "advertise": cfg.Hub.Advertise, "pending_enrollment": cfg.HubPollToken != ""},
		"listen":  cfg.Listen,
		"gc":      map[string]any{"disabled": cfg.GC.Disabled, "keep": cfg.GC.Keep.String()},
		"ssh":     map[string]any{"disabled": cfg.SSH.Disabled, "listen": cfg.SSH.Listen},
		"audit":   map[string]any{"file": cfg.Audit.File != "", "syslog": cfg.Audit.Syslog != "", "webhook": cfg.Audit.Webhook != ""},
		"update":  map[string]any{"disabled": cfg.Update.Disabled, "channel": cfg.Update.Channel, "interval": cfg.Update.Interval.String()},
		"max_log": cfg.MaxLogBytes,
	}
	if cfg.Toolchain.Disabled || cfg.Toolchain.Backend != "" {
		m["toolchain"] = map[string]any{"disabled": cfg.Toolchain.Disabled, "backend": cfg.Toolchain.Backend}
	}
	if tp != nil {
		disabled, teams := 0, map[string]bool{}
		for _, u := range tp.Users {
			if u.Disabled {
				disabled++
			}
			for _, t := range u.Teams {
				teams[t] = true
			}
		}
		m["tenancy"] = map[string]any{"version": tp.Version, "enabled": tp.Enabled, "users": len(tp.Users), "disabled_users": disabled,
			"teams": len(teams), "purge": len(tp.Purge), "default_quota": tp.DefaultQuota != nil}
	}
	return m
}
