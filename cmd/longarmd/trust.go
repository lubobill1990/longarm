package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/longarmd"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/trust"
)

// trustCmd: end-to-end trust on the worker (docs/security.md "End-to-end
// trust"). Everything here edits <data>/trust, which only the worker's
// owner controls; the hub never writes it.
func trustCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "trust",
		Short: "End-to-end trust: which users' endorsed devices may use this worker, and this worker's own endorsement",
		Long: `With end-to-end trust this worker accepts only clients whose certificates carry
an endorsement by a user key you pinned here - a certificate the hub's CA
mints on its own is refused - and clients that require it accept this
worker only once one of their users endorsed it.

  longarmd trust add-user <longarm-user-key line>   # from: longarm trust init / show
  longarmd trust request                            # then on a device with the user key:
                                                    #   longarm trust endorse <request>
  longarmd trust accept <longarm-endorsement line>

The running longarmd picks up changes within seconds (a new endorsement:
it renews its certificate through the hub).`,
	}
	var dataDir string
	c.PersistentFlags().StringVar(&dataDir, "data-dir", longarmd.DefaultDataDir(), "data directory")
	store := func() trust.Store { return trust.Store{Dir: filepath.Join(dataDir, longarmd.TrustDir)} }
	workerName := func() (*pki.Bundle, error) {
		b, err := pki.LoadBundle(filepath.Join(dataDir, longarmd.TLSDir))
		if err != nil {
			return nil, fmt.Errorf("%w (join a hub first: longarmd join)", err)
		}
		return b, nil
	}

	request := &cobra.Command{
		Use: "request", Short: "Print this worker's endorsement request (for: longarm trust endorse)", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			b, err := workerName()
			if err != nil {
				return err
			}
			r, err := store().Request(pki.KindWorker, b.Identity.Name, "")
			if err != nil {
				return err
			}
			fmt.Println(r.String())
			fmt.Printf("\n# worker %s, node key %s\n# on a device holding your user key: longarm trust endorse <the line above>\n# then here: longarmd trust accept <the endorsement>\n",
				b.Identity.Name, pki.KeyID(r.NodeKey))
			return nil
		},
	}
	accept := &cobra.Command{
		Use: "accept <endorsement>", Short: "Store this worker's endorsement (the certificate is renewed to carry it)", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			b, err := workerName()
			if err != nil {
				return err
			}
			e, err := pki.ParseEndorsement(args[0])
			if err != nil {
				return err
			}
			if err := store().SetEndorsement(e, pki.KindWorker, b.Identity.Name); err != nil {
				return err
			}
			fmt.Printf("worker %s endorsed by %s (key %s); the running longarmd renews its certificate through the hub to carry it\n",
				b.Identity.Name, e.User, pki.KeyID(e.UserKey))
			return nil
		},
	}
	addUser := &cobra.Command{
		Use: "add-user <user-key>", Short: "Let the endorsed devices of a user use this worker (turns end-to-end trust on)", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			l, err := pki.ParseUserKeyLine(args[0])
			if err != nil {
				return err
			}
			s := store()
			first := false
			if p, err := s.Policy(false); err == nil && !p.Active() {
				first = true
			}
			if err := s.AddUser(l); err != nil {
				return err
			}
			fmt.Printf("trusting user %s (key %s)\n", l.User, pki.KeyID(l.Key))
			if first {
				fmt.Println("end-to-end trust is on: from now on only endorsed devices of trusted users can use this worker,\n" +
					"and the hub session can't reach jobs unless [hub] remote_jobs is set explicitly")
			}
			return nil
		},
	}
	removeUser := &cobra.Command{
		Use: "remove-user <user>", Short: "Stop accepting a user's devices (the last one turns end-to-end trust off)", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := store().RemoveUser(args[0]); err != nil {
				return err
			}
			if p, err := store().Policy(false); err == nil && !p.Active() {
				fmt.Println("no trusted users left: end-to-end trust is off (every certificate of the hub's CA is accepted again)")
			}
			return nil
		},
	}
	denyNode := &cobra.Command{
		Use: "deny-node <node-key>", Short: "Refuse a node key even when endorsed (a lost device; base64, from longarm trust show)", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error { return store().DenyNode(strings.TrimSpace(args[0])) },
	}
	status := &cobra.Command{
		Use: "status", Short: "Show the trust policy and this worker's endorsement", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			s := store()
			p, err := s.Policy(false)
			if err != nil {
				return err
			}
			fmt.Printf("policy       %s\n", trust.Summary(p))
			if k, err := s.NodeKey(); err == nil {
				fmt.Printf("node key     %s (%s)\n", base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey)), pki.KeyID(k.Public().(ed25519.PublicKey)))
			}
			if e, err := s.Endorsement(); err != nil {
				fmt.Printf("endorsement  unreadable: %v\n", err)
			} else if e == nil {
				fmt.Println("endorsement  none (longarmd trust request)")
			} else {
				fmt.Printf("endorsement  by %s (key %s) as %s %s\n", e.User, pki.KeyID(e.UserKey), e.Kind, e.Name)
			}
			if b, err := workerName(); err == nil {
				t, err := pki.TrustOf(b.Cert.Leaf)
				switch {
				case err != nil:
					fmt.Printf("certificate  invalid trust extension: %v\n", err)
				case t == nil:
					fmt.Println("certificate  no trust extension (renewed by the running longarmd after an endorsement; an old hub drops it)")
				case t.Endorsement == nil:
					fmt.Println("certificate  bound to the node key, not endorsed")
				default:
					fmt.Printf("certificate  endorsed by %s (key %s)\n", t.Endorsement.User, pki.KeyID(t.Endorsement.UserKey))
				}
				if s.RenewalRequested() {
					fmt.Println("             renewal pending (longarmd renews within seconds while connected to the hub)")
				}
			}
			return nil
		},
	}
	c.AddCommand(request, accept, addUser, removeUser, denyNode, status)
	return c
}
