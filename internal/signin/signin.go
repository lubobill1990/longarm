// Package signin is the client side of signing in to longarm-hub with a
// Microsoft account, shared by `longarm login` and `longarmd login`: an
// access token from the Azure CLI (az) when it has one, else the hub's
// pairing flow - a code the user types into the hub's /link page.
package signin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/hubclient"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// EnvAz overrides where the Azure CLI is found (tests, odd installs).
const EnvAz = "LONGARM_AZ"

// EnvNoBrowser set: never open a browser (scripts, tests).
const EnvNoBrowser = "LONGARM_NO_BROWSER"

var (
	// ErrNoAz: the Azure CLI is not installed (or not where EnvAz says).
	ErrNoAz = errors.New("the Azure CLI (az) was not found")
	// ErrAzNoToken: az has no usable sign-in of a user for the hub.
	ErrAzNoToken = errors.New("az has no token for this hub")
)

type azError struct {
	kind error
	msg  string
}

func (e *azError) Error() string { return e.msg }
func (e *azError) Unwrap() error { return e.kind }

// azNeedsLogin matches the Azure CLI's "no usable sign-in" errors.
var azNeedsLogin = regexp.MustCompile(`(?i)az login|MSAL cache|AADSTS|not logged in|interaction_required|refresh token|consent`)

// AzToken gets an access token for the hub from the Azure CLI: for the
// hub's delegated scope when az has a user signed in, for
// api://<client id>/.default (app-only) when it has a service principal;
// scope overrides both. The token is passed on, never printed or stored.
// Errors wrap ErrNoAz / ErrAzNoToken when az is missing / has no user
// sign-in (a service principal's failures are neither: CI shouldn't fall
// back to a flow that waits for a person).
func AzToken(ctx context.Context, info *pb.AuthInfoResponse, scope string) (string, error) {
	tenant := info.EntraTenantId
	hint := fmt.Sprintf("sign in to Azure first:\n  az login --tenant %s --scope %s\nthen run login again", tenant, info.EntraScope)
	az, err := FindAz()
	if err != nil {
		return "", &azError{ErrNoAz, fmt.Sprintf("%v (install it, or set %s); then %s", ErrNoAz, EnvAz, hint)}
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
		return "", &azError{ErrAzNoToken, fmt.Sprintf("%v; %s", ErrAzNoToken, hint)}
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

// FindAz locates the Azure CLI: $LONGARM_AZ, PATH, and on Windows its
// default install directories (a service or a fresh shell may lack them
// on PATH).
func FindAz() (string, error) {
	if p := os.Getenv(EnvAz); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", err
		}
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
	return "", ErrNoAz
}

// Fallback reports whether a failed az sign-in should fall back to
// pairing: az missing or without a user's token, or a hub without Entra
// sign-in (its panel's users approve).
func Fallback(err error) bool {
	return errors.Is(err, ErrNoAz) || errors.Is(err, ErrAzNoToken) || errors.Is(err, hubclient.ErrNoEntra)
}

// Pair enrolls through the hub's pairing flow (kind "client" or
// "worker"): it shows the /link URL and the code on w, opens the URL
// (never with the code) in the browser if asked and a desktop is there,
// and waits until the user approves or denies it there, or it expires.
func Pair(ctx context.Context, addr, kind string, o hubclient.JoinOptions, w io.Writer, browser bool) (*hubclient.JoinResult, error) {
	return hubclient.Link(ctx, addr, hubclient.LinkOptions{JoinOptions: o, Kind: kind, Show: func(st *pb.StartLinkResponse) {
		what := "this device"
		if kind == "worker" {
			what = "this worker as yours"
		}
		fmt.Fprintf(w, "To link %s, sign in with your Microsoft account (or the hub's panel login) in a browser:\n\n", what)
		fmt.Fprintf(w, "  Open %s and enter code %s\n\n", st.VerificationUrl, st.UserCode)
		mins := int(time.Until(st.ExpiresAt.AsTime()).Round(time.Minute).Minutes())
		fmt.Fprintf(w, "Check that the page shows this machine (%s) before you approve. The code expires in %d minutes.\n", hostname(), mins)
		if browser && OpenBrowser(st.VerificationUrl) == nil {
			fmt.Fprintln(w, "(opened the page in your browser)")
		}
		fmt.Fprintln(w, "waiting for approval ...")
	}})
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// Desktop reports whether a browser can be opened here: not over SSH nor
// with $LONGARM_NO_BROWSER set, and on Linux / BSD in a graphical session.
func Desktop() bool {
	if os.Getenv(EnvNoBrowser) != "" {
		return false
	}
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	switch runtime.GOOS {
	case "windows", "darwin":
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

// OpenBrowser opens u in the default browser when there is a desktop.
func OpenBrowser(u string) error {
	if !Desktop() {
		return errors.New("no desktop session")
	}
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return errors.New("not a web URL")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
