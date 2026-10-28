package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/nodeid"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func adminCmd() *cobra.Command {
	c := &cobra.Command{Use: "admin", Short: "Hub administration (nodes, join codes, approval, revocation, drain, profiles, audit), CA and certificates, admin-only worker operations"}
	ca := &cobra.Command{Use: "ca", Short: "Certificate authority"}
	var caDir, caName string
	var caYears int
	caInit := &cobra.Command{
		Use:   "init",
		Short: "Create a new CA (ca.crt + ca.key) in --ca-dir",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			dir := caDir
			if dir == "" {
				dir = filepath.Join(configDir, client.CADir)
			}
			ca, err := pki.InitCA(dir, caName, time.Duration(caYears)*365*24*time.Hour)
			if err != nil {
				return err
			}
			fmt.Printf("CA %q created in %s (valid until %s)\n", caName, dir, ca.Cert.NotAfter.Format("2006-01-02"))
			fmt.Println("keep ca.key secret; distribute only ca.crt (it is included in every issued bundle)")
			return nil
		},
	}
	caInit.Flags().StringVar(&caDir, "ca-dir", "", "CA directory (default <config-dir>/ca)")
	caInit.Flags().StringVar(&caName, "name", "Longarm CA", "CA common name")
	caInit.Flags().IntVar(&caYears, "years", 10, "validity in years")
	ca.AddCommand(caInit)

	var role, name, out, issueUser, issueID string
	var hosts []string
	var days int
	var install bool
	issue := &cobra.Command{
		Use:   "issue --role worker|client|admin|relay|release --name NAME (--out DIR | --install)",
		Short: "Issue a certificate bundle (ca.crt, node.crt, node.key)",
		Long: `Issue a certificate bundle signed by the CA.

  worker: copy the bundle to <longarmd data dir>/tls on the worker (e.g. --out C:\Users\me\.longarm\tls)
  relay:  copy the bundle to the longarm-relay host (longarm-relay --tls-dir DIR); add --host <public name/IP>
  client/admin: --install writes it to <config-dir>/tls for this longarm
  release: the CI credential (docs/ci-cd.md): only longarm admin release publish|ls|status|push|abort
           on the hub; nothing on workers. 180 days by default.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			r, err := pki.ParseRole(role)
			if err != nil {
				return err
			}
			dir := caDir
			if dir == "" {
				dir = filepath.Join(configDir, client.CADir)
			}
			ca, err := pki.LoadCA(dir)
			if err != nil {
				return fmt.Errorf("load CA: %w (run `longarm admin ca init` first)", err)
			}
			var validity time.Duration
			if days > 0 {
				validity = time.Duration(days) * 24 * time.Hour
			}
			// the node ID: a new node's (fresh), or - re-issuing a node's
			// certificate - its own (--id): a hub maps certificates to nodes
			// by ID, never an unknown one by name
			id := issueID
			if id == "" {
				id = nodeid.New()
			} else if id = nodeid.Canonical(id); id == "" {
				return fmt.Errorf("--id %q: not a node id (longarm admin nodes shows them)", issueID)
			}
			certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{Role: r, Name: name, Hosts: hosts, Validity: validity, User: issueUser, ID: id})
			if err != nil {
				return err
			}
			if install {
				if r == pki.RoleWorker || r == pki.RoleRelay || r == pki.RoleRelease {
					return fmt.Errorf("--install is for client/admin bundles; use --out for workers, relays and release (CI) bundles")
				}
				out = filepath.Join(configDir, client.TLSDir)
			}
			if out == "" {
				return fmt.Errorf("need --out DIR or --install")
			}
			if _, err := os.Stat(filepath.Join(out, pki.NodeKeyFile)); err == nil {
				fmt.Fprintf(os.Stderr, "overwriting existing bundle in %s\n", out)
			}
			if err := pki.WriteBundle(out, ca.CertPEM, certPEM, keyPEM); err != nil {
				return err
			}
			fmt.Printf("issued %s/%s (node id %s) -> %s\n", r, name, id, out)
			return nil
		},
	}
	issue.Flags().StringVar(&role, "role", "", "worker | client | admin | relay | release")
	issue.Flags().StringVar(&name, "name", "", "node name (workers: must match the name in workers.toml / cert_name)")
	issue.Flags().StringArrayVar(&hosts, "host", nil, "extra DNS name / IP SAN for workers and relays (repeatable)")
	issue.Flags().IntVar(&days, "days", 0, "validity in days (default: worker/relay 90, client/admin 30, release 180)")
	issue.Flags().StringVar(&out, "out", "", "output directory")
	issue.Flags().BoolVar(&install, "install", false, "install as this longarm's own bundle (<config-dir>/tls)")
	issue.Flags().StringVar(&caDir, "ca-dir", "", "CA directory (default <config-dir>/ca)")
	issue.Flags().StringVar(&issueUser, "user", "", "client/admin: the Longarm user the certificate belongs to (multi-tenancy; default: the name)")
	issue.Flags().StringVar(&issueID, "id", "", "re-issue for an existing node: its node ID (default: a new node, a fresh ID); a hub refuses an unknown certificate of a name it knows")
	issue.MarkFlagRequired("role")
	issue.MarkFlagRequired("name")

	shutdown := &cobra.Command{
		Use:   "shutdown <worker>",
		Short: "Stop longarmd gracefully (admin role; running jobs continue in their shims)",
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
			if _, err := conn.Admin().Shutdown(ctx, &pb.ShutdownRequest{}); err != nil {
				return err
			}
			fmt.Printf("%s: shutdown requested\n", args[0])
			return nil
		},
	}
	var gcKeep time.Duration
	var gcMinFree float64
	var gcDry bool
	gcc := &cobra.Command{
		Use:   "gc <worker>",
		Short: "Remove old job/session dirs, worktrees, staging data and unused mirrors on a worker now (admin role)",
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
			req := &pb.GcRequest{DryRun: gcDry}
			if cmd.Flags().Changed("keep") {
				req.Keep = durationpb.New(gcKeep)
			}
			if cmd.Flags().Changed("min-free") {
				req.MinFreePercent = gcMinFree
				if gcMinFree == 0 {
					req.MinFreePercent = -1
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			resp, err := conn.Admin().Gc(ctx, req)
			if err != nil {
				return err
			}
			for _, r := range resp.Removed {
				fmt.Println(r)
			}
			verb := "freed"
			if gcDry {
				verb = "would free"
			}
			fmt.Printf("%d removed, %s %s\n", len(resp.Removed), verb, humanSize(resp.FreedBytes))
			return nil
		},
	}
	gcc.Flags().DurationVar(&gcKeep, "keep", 0, "keep terminal jobs/sessions that ended within this long (default: worker's [gc] keep)")
	gcc.Flags().Float64Var(&gcMinFree, "min-free", 0, "remove more, oldest first, while less than this % of the disk is free (default: worker's [gc]; 0 disables)")
	gcc.Flags().BoolVar(&gcDry, "dry-run", false, "only list what would be removed")
	c.AddCommand(ca, issue, shutdown, gcc)
	c.AddCommand(hubAdminCmds()...)
	c.AddCommand(tailnetAdminCmd())
	c.AddCommand(hubUICmds()...)
	c.AddCommand(userAdminCmd(), teamAdminCmd())
	c.AddCommand(adminDiagCmd(), pseudonymCmd())
	return c
}
