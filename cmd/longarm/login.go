package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// EnvAz overrides where `longarm login` finds the Azure CLI (tests, odd
// installs).
const EnvAz = "LONGARM_AZ"

func loginCmd() *cobra.Command {
	var name, fp, scope string
	var force bool
	c := &cobra.Command{
		Use:   "login <hub[:port]>",
		Short: "Sign in to longarm-hub with Microsoft Entra ID (your az login) and enroll this machine",
		Long: `Sign in to longarm-hub with Microsoft Entra ID: the access token of your
Azure CLI sign-in (az login) replaces the join code. The hub creates your
Longarm user on first sign-in and issues this machine a certificate right
away (an admin certificate for the hub's Entra admins).

Signing in again (an expired certificate, another account) replaces this
machine's certificate; the device keeps its name.

CI: with az signed in as a service principal (az login --service-principal,
e.g. a GitHub Actions federated credential) the token is an app-only one
for api://<client id>/.default, which the hub accepts for the apps listed
in its [auth.entra] apps.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			addr := hubclient.NormalizeAddr(args[0])
			tlsDir := filepath.Join(configDir, client.TLSDir)
			if _, err := os.Stat(filepath.Join(tlsDir, pki.NodeCertFile)); err == nil && !force {
				// Signing in to the same hub again is what login is for;
				// replacing another hub's certificate needs --force.
				if hc, _ := hubclient.LoadFile(configDir); hc.Address != addr {
					return fmt.Errorf("%s already holds a certificate of another hub (use --force to replace it)", tlsDir)
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			tr, err := client.TrustStore(configDir).CSRTrust()
			if err != nil {
				return fmt.Errorf("end-to-end trust: %w", err)
			}
			r, err := hubclient.Login(ctx, addr, hubclient.LoginOptions{
				JoinOptions: hubclient.JoinOptions{Name: name, Fingerprint: fp, Trust: tr},
				Token: func(ctx context.Context, info *pb.AuthInfoResponse) (string, error) {
					return azAccessToken(ctx, info, scope)
				},
			})
			if err != nil {
				return err
			}
			if r.Resp.Message != "" {
				fmt.Fprintln(os.Stderr, r.Resp.Message)
			}
			return installEnrollment(addr, r, tr, "signed in to")
		},
	}
	c.Flags().StringVar(&name, "name", "", "device name (default: <user>-<hostname>)")
	c.Flags().StringVar(&fp, "ca-fingerprint", "", "expected sha256 fingerprint of the hub CA (longarm-hub ca show)")
	c.Flags().StringVar(&scope, "scope", "", "token scope to request (default: the hub's, or api://<client id>/.default for a service principal)")
	c.Flags().BoolVar(&force, "force", false, "replace a certificate of another hub")
	return c
}

// azNeedsLogin matches the Azure CLI's "no usable sign-in" errors.
var azNeedsLogin = regexp.MustCompile(`(?i)az login|MSAL cache|AADSTS|not logged in|interaction_required|refresh token|consent`)

// azAccessToken gets an access token for the hub from the Azure CLI: for
// the hub's delegated scope when az has a user signed in, for
// api://<client id>/.default (app-only) when it has a service principal;
// scope overrides both. The token is passed on, never printed or stored.
func azAccessToken(ctx context.Context, info *pb.AuthInfoResponse, scope string) (string, error) {
	tenant := info.EntraTenantId
	hint := fmt.Sprintf("sign in to Azure first:\n  az login --tenant %s --scope %s\nthen run longarm login again", tenant, info.EntraScope)
	az, err := findAz()
	if err != nil {
		return "", fmt.Errorf("the Azure CLI (az) was not found (install it, or set %s); then %s", EnvAz, hint)
	}
	// A service principal has no delegated permissions (and a managed
	// identity takes no --tenant); not signed in at all shows below.
	sp := azRun(ctx, az, "account", "show", "--query", "user.type", "-o", "tsv") == "servicePrincipal"
	if scope == "" {
		scope = info.EntraScope
		if sp {
			scope = "api://" + info.EntraClientId + "/.default"
		}
	}
	args := []string{"account", "get-access-token", "--scope", scope, "--query", "accessToken", "-o", "tsv"}
	if !sp {
		args = append(args, "--tenant", tenant)
	}
	cmd := exec.CommandContext(ctx, az, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	tok := strings.TrimSpace(out.String())
	if err == nil && tok != "" && !strings.ContainsAny(tok, " \t\r\n") {
		return tok, nil
	}
	msg := strings.TrimSpace(errb.String())
	if sp {
		hint = "the service principal needs a token for " + scope + " (and the hub must list its app in [auth.entra] apps)"
	}
	if !sp && azNeedsLogin.MatchString(msg) {
		return "", fmt.Errorf("az has no token for this hub; %s", hint)
	}
	if err == nil {
		err = errors.New("no token in its output")
	}
	if msg != "" {
		return "", fmt.Errorf("az account get-access-token: %v: %s\n(%s)", err, msg, hint)
	}
	return "", fmt.Errorf("az account get-access-token: %v (%s)", err, hint)
}

// azRun runs az and returns its trimmed output ("" on failure).
func azRun(ctx context.Context, az string, args ...string) string {
	out, err := exec.CommandContext(ctx, az, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// findAz locates the Azure CLI: $LONGARM_AZ, PATH, and on Windows its
// default install directories (a service or a fresh shell may lack them
// on PATH).
func findAz() (string, error) {
	if p := os.Getenv(EnvAz); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("az"); err == nil {
		return p, nil
	}
	if runtime.GOOS == "windows" {
		for _, root := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
			if root == "" {
				continue
			}
			p := filepath.Join(root, "Microsoft SDKs", "Azure", "CLI2", "wbin", "az.cmd")
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
		}
	}
	return "", errors.New("az not found")
}
