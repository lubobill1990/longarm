package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/signin"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

func loginCmd() *cobra.Command {
	var name, fp, scope string
	var force, useAz, link, noBrowser bool
	c := &cobra.Command{
		Use:   "login <hub[:port]>",
		Short: "Sign in to longarm-hub with your Microsoft account and enroll this machine",
		Long: `Sign in to longarm-hub with your Microsoft account (Microsoft Entra ID). The
hub creates your Longarm user on first sign-in and issues this machine a
certificate (an admin certificate for the hub's Entra admins).

With the Azure CLI signed in (az login), its access token is used and
nothing else is asked. Without az, or when az has no token (and without
--az), or with --link: login prints the hub's /link address and a code;
open the page in any browser (another machine works too: phone, laptop),
sign in with Microsoft, type the code and approve. The page is opened
here when this machine has a desktop (--no-browser: never).

Signing in again (an expired certificate, another account) replaces this
machine's certificate; the device keeps its name.

CI: with az signed in as a service principal (az login --service-principal,
e.g. a GitHub Actions federated credential) the token is an app-only one
for api://<client id>/.default, which the hub accepts for the apps listed
in its [auth.entra] apps. Such a sign-in never falls back to the browser.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if useAz && link {
				return errors.New("--az and --link exclude each other")
			}
			addr := hubclient.NormalizeAddr(args[0])
			tlsDir := filepath.Join(configDir, client.TLSDir)
			if _, err := os.Stat(filepath.Join(tlsDir, pki.NodeCertFile)); err == nil && !force {
				// Signing in to the same hub again is what login is for;
				// replacing another hub's certificate needs --force.
				if hc, _ := hubclient.LoadFile(configDir); hc.Address != addr {
					return fmt.Errorf("%s already holds a certificate of another hub (use --force to replace it)", tlsDir)
				}
			}
			tr, err := client.TrustStore(configDir).CSRTrust()
			if err != nil {
				return fmt.Errorf("end-to-end trust: %w", err)
			}
			jo := hubclient.JoinOptions{Name: name, Fingerprint: fp, Trust: tr}
			var r *hubclient.JoinResult
			if !link {
				ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
				r, err = hubclient.Login(ctx, addr, hubclient.LoginOptions{JoinOptions: jo,
					Token: func(ctx context.Context, info *pb.AuthInfoResponse) (string, error) {
						return signin.AzToken(ctx, info, scope)
					},
				})
				cancel()
				if err != nil && !useAz && signin.Fallback(err) {
					reason := "no az sign-in for this hub"
					if errors.Is(err, hubclient.ErrNoEntra) {
						reason = "this hub has no Microsoft Entra sign-in for az"
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "%s: signing in through the browser instead (--az: az only)\n", reason)
					link, err = true, nil
				}
				if err != nil {
					return err
				}
			}
			if link {
				if r, err = signin.Pair(cmd.Context(), addr, "client", jo, cmd.ErrOrStderr(), !noBrowser); err != nil {
					return err
				}
			}
			if r.Resp.Message != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), r.Resp.Message)
			}
			return installEnrollment(addr, r, tr, "signed in to")
		},
	}
	c.Flags().StringVar(&name, "name", "", "device name (default: <user>-<hostname>)")
	c.Flags().StringVar(&fp, "ca-fingerprint", "", "expected sha256 fingerprint of the hub CA (longarm-hub ca show)")
	c.Flags().StringVar(&scope, "scope", "", "token scope to request (default: the hub's, or api://<client id>/.default for a service principal)")
	c.Flags().BoolVar(&force, "force", false, "replace a certificate of another hub")
	c.Flags().BoolVar(&useAz, "az", false, "sign in with az only (fail instead of falling back to the browser)")
	c.Flags().BoolVar(&link, "link", false, "sign in through the browser (a code to enter at the hub's /link page), without az")
	c.Flags().BoolVar(&link, "browser", false, "same as --link")
	c.Flags().BoolVar(&noBrowser, "no-browser", false, "only print the /link address and the code; don't open a browser here")
	return c
}
