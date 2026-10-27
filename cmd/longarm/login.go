package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/signin"
)

func loginCmd() *cobra.Command {
	var name, fp, scope string
	var force, useAz, link, noBrowser bool
	c := &cobra.Command{
		Use:   "login [hub]",
		Short: "Sign in to longarm-hub with your Microsoft account and enroll this machine",
		Long: `Sign in to longarm-hub with your Microsoft account (Microsoft Entra ID). The
hub creates your Longarm user on first sign-in and issues this machine a
certificate (an admin certificate for the hub's Entra admins).

The hub is its domain - longarm login longarm.example.com - or
https://longarm.example.com, or host:port (the gRPC port, default 7443).
Without --ca-fingerprint the hub's CA is fetched from
https://<host>/ca.crt, checked with this system's HTTPS roots like a
browser opening the hub's panel, and pinned (docs/security.md "First
contact"); a hub without a public HTTPS panel needs --ca-fingerprint.
Without a hub: the one this machine is signed in to already, else the
one the installer recorded.

With the Azure CLI signed in (az login), its access token is used and
nothing else is asked. Without az, or when az has no token (and without
--az), or with --link: login prints the hub's /link address and a code;
open the page in any browser (another machine works too: phone, laptop),
sign in with Microsoft, type the code and approve. The page is opened
here when this machine has a desktop (--no-browser: never).

To make this machine your worker too, with the same single approval:
longarmd login <hub> --role both.

Signing in again (an expired certificate, another account) replaces this
machine's certificate; the device keeps its name.

CI: with az signed in as a service principal (az login --service-principal,
e.g. a GitHub Actions federated credential) the token is an app-only one
for api://<client id>/.default, which the hub accepts for the apps listed
in its [auth.entra] apps. Such a sign-in never falls back to the browser.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if useAz && link {
				return errors.New("--az and --link exclude each other")
			}
			hub := ""
			if len(args) > 0 {
				hub = args[0]
			} else {
				var err error
				if hub, fp, err = defaultHub(fp); err != nil {
					return err
				}
			}
			o := signin.Options{Hub: hub, Fingerprint: fp, DeviceName: name, Scope: scope, OpenBrowser: !noBrowser,
				Client: client.DeviceTarget{ConfigDir: configDir, Force: force}, Method: signin.MethodAuto}
			switch {
			case useAz:
				o.Method = signin.MethodAz
			case link:
				o.Method = signin.MethodLink
			}
			r, err := signin.SignIn(cmd.Context(), o, signin.Console(cmd.ErrOrStderr()))
			if err != nil {
				return err
			}
			if r.Device.Message != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), r.Device.Message)
			}
			fmt.Printf("signed in to %s as %s/%s (config %s)\n", r.Hub, r.Device.Role, r.Device.Name, configDir)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "device name (default: <user>-<hostname>)")
	c.Flags().StringVar(&fp, "ca-fingerprint", "", "expected sha256 fingerprint of the hub CA (longarm-hub ca show); default: fetched from https://<hub>/ca.crt")
	c.Flags().StringVar(&scope, "scope", "", "token scope to request (default: the hub's, or api://<client id>/.default for a service principal)")
	c.Flags().BoolVar(&force, "force", false, "replace a certificate of another hub")
	c.Flags().BoolVar(&useAz, "az", false, "sign in with az only (fail instead of falling back to the browser)")
	c.Flags().BoolVar(&link, "link", false, "sign in through the browser (a code to enter at the hub's /link page), without az")
	c.Flags().BoolVar(&link, "browser", false, "same as --link")
	c.Flags().BoolVar(&noBrowser, "no-browser", false, "only print the /link address and the code; don't open a browser here")
	return c
}

// defaultHub is the hub to sign in to when none is given: the one this
// config dir is enrolled with (its CA pinned), else the installer's.
func defaultHub(fp string) (string, string, error) {
	if hc, _ := hubclient.LoadFile(configDir); hc.Address != "" {
		if fp == "" {
			if b, err := os.ReadFile(filepath.Join(configDir, client.TLSDir, pki.CACertFile)); err == nil {
				ca, _ := pki.ParseCertPEM(b)
				if ca != nil {
					fp = pki.Fingerprint(ca)
				}
			}
		}
		return hc.Address, fp, nil
	}
	if hub, ifp := signin.InstalledHub(); hub != "" {
		if fp == "" {
			fp = ifp
		}
		return hub, fp, nil
	}
	return "", "", fmt.Errorf("which hub? longarm login <hub>, e.g. longarm login longarm.example.com (no hub in %s)", filepath.Join(configDir, hubclient.File))
}
