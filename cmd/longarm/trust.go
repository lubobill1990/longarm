package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/trust"
)

// End-to-end trust, the client side (docs/usage.md, docs/security.md): a
// user key on the user's devices endorses the user's devices and workers;
// workers demand endorsed devices, and with require_workers this client
// demands endorsed workers. Nothing here goes through the hub except the
// certificate renewal that makes the certificate carry the endorsement.
//
//	longarm trust init                       create this device's user key, endorse this device
//	longarm trust show
//	longarm trust request                    endorsement request of this device (a second device)
//	longarm trust endorse <request>          endorse a worker or a device (needs the user key)
//	longarm trust accept <endorsement>       install this device's endorsement
//	longarm trust export-key | import-key <file|->
//	longarm trust add-user <user-key> | remove-user <user>
//	longarm trust require on|off
//	longarm trust deny-node <node-key>

func trustCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "trust",
		Short: "End-to-end trust: user keys that endorse your devices and workers (the hub can't forge them)",
		Long: `End-to-end trust: the hub's CA can mint a certificate for any name; with
end-to-end trust a certificate must also carry an endorsement by a user key
that never leaves the user's devices. Workers can demand endorsed devices
(longarmd trust add-user), and this client can demand endorsed workers
(longarm trust require on).

First device:      longarm trust init            (prints your user key line)
On each worker:    longarmd trust add-user <your user key line>
                   longarmd trust request        -> longarm trust endorse <request>
                   longarmd trust accept <endorsement>
Second device:     longarm trust request         -> longarm trust endorse <request> (first device)
                   longarm trust accept <endorsement>
                   (or copy the key: longarm trust export-key | longarm trust import-key -)
Workers only:      longarm trust require on

State: <config dir>/trust (node key, endorsement, user key, trust.toml).`,
	}
	c.AddCommand(trustInitCmd(), trustShowCmd(), trustRequestCmd(), trustEndorseCmd(), trustAcceptCmd(),
		trustExportKeyCmd(), trustImportKeyCmd(), trustAddUserCmd(), trustRemoveUserCmd(), trustRequireCmd(),
		trustDenyNodeCmd())
	return c
}

func trustStore() trust.Store { return client.TrustStore(configDir) }

// deviceBundle is this device's certificate (identity and leaf).
func deviceBundle() (*pki.Bundle, error) {
	b, err := pki.LoadBundle(filepath.Join(configDir, client.TLSDir))
	if err != nil {
		return nil, fmt.Errorf("%w (longarm join <hub> <code> first)", err)
	}
	if b.Identity.Role == pki.RoleWorker {
		return nil, errors.New("this config dir holds a worker certificate; use longarmd trust on workers")
	}
	return b, nil
}

// ownUserKey is the user key held on this device.
func ownUserKey(ts trust.Store) (ed25519.PrivateKey, string, error) {
	k, user, err := ts.UserKey()
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", errors.New("this device holds no user key (longarm trust init, or longarm trust import-key)")
	}
	return k, user, err
}

// endorseSelf endorses this device with the user key k of user.
func endorseSelf(ts trust.Store, k ed25519.PrivateKey, user string, id pki.Identity) error {
	if user != id.Owner() {
		return fmt.Errorf("this device belongs to user %s, the user key to %s", id.Owner(), user)
	}
	nk, err := ts.NodeKey()
	if err != nil {
		return err
	}
	e, err := pki.EndorseID(k, user, pki.KindDevice, id.Name, id.ID, nk.Public().(ed25519.PublicKey))
	if err != nil {
		return err
	}
	return ts.SetEndorsementFor(e, pki.KindDevice, id)
}

// trustInit creates the user key of id's user on this device and endorses
// the device with it.
func trustInit(ts trust.Store, id pki.Identity) (pki.UserKeyLine, error) {
	user := id.Owner()
	if !pki.ValidUser(user) {
		return pki.UserKeyLine{}, fmt.Errorf("this device's user %q can't hold a user key (a-z 0-9 _ -, at most 17 characters)", user)
	}
	if _, u, err := ts.UserKey(); err == nil {
		return pki.UserKeyLine{}, fmt.Errorf("this device holds the user key of %s already (longarm trust show)", u)
	} else if !errors.Is(err, os.ErrNotExist) {
		return pki.UserKeyLine{}, err
	}
	k, err := ts.CreateUserKey(user)
	if err != nil {
		return pki.UserKeyLine{}, err
	}
	if err := endorseSelf(ts, k, user, id); err != nil {
		return pki.UserKeyLine{}, err
	}
	return pki.UserKeyLine{User: user, Key: k.Public().(ed25519.PublicKey)}, nil
}

// trustEndorse endorses a worker's or a device's request with this
// device's user key.
func trustEndorse(ts trust.Store, line string) (*pki.Endorsement, error) {
	k, user, err := ownUserKey(ts)
	if err != nil {
		return nil, err
	}
	r, err := pki.ParseEndorseRequest(line)
	if err != nil {
		return nil, err
	}
	if r.Kind == pki.KindDevice && r.User != user {
		return nil, fmt.Errorf("the request is for a device of user %q; this user key is %s's", r.User, user)
	}
	// a request with the node's ID gets an endorsement bound to it (valid
	// through renames); one without, from a node before IDs, to its name
	return pki.EndorseID(k, user, r.Kind, r.Name, r.ID, r.NodeKey)
}

// trustAccept installs this device's endorsement.
func trustAccept(ts trust.Store, line string, id pki.Identity) (*pki.Endorsement, error) {
	e, err := pki.ParseEndorsement(line)
	if err != nil {
		return nil, err
	}
	if e.User != id.Owner() {
		return nil, fmt.Errorf("endorsed by user %s, but this device belongs to %s (workers would refuse it)", e.User, id.Owner())
	}
	return e, ts.SetEndorsementFor(e, pki.KindDevice, id)
}

// trustImportKey installs a user key and, if this device is not endorsed
// yet, endorses it. It reports whether it endorsed the device.
func trustImportKey(ts trust.Store, b []byte, id pki.Identity) (string, bool, error) {
	user, err := ts.ImportUserKey(b)
	if err != nil {
		return "", false, err
	}
	if e, err := ts.Endorsement(); err != nil || e != nil {
		return user, false, err
	}
	if user != id.Owner() {
		return user, false, nil // a user key of someone else: only for endorsing
	}
	k, _, err := ts.UserKey()
	if err != nil {
		return user, false, err
	}
	return user, true, endorseSelf(ts, k, user, id)
}

// renewForTrust renews the certificate through the hub now so that it
// carries the new endorsement; when that's not possible the renewal request
// stays and the next longarm that reaches the hub (or longarm daemon) does it.
func renewForTrust(ctx context.Context) {
	later := "the certificate will be renewed with it the next time longarm reaches the hub"
	s, err := openSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "longarm: certificate not renewed (%s); %s\n", oneLine(err), later)
		return
	}
	defer s.close()
	h := s.hub()
	if h == nil {
		fmt.Fprintln(os.Stderr, "longarm: no hub configured: the certificate carries the endorsement only once renewed through a hub (longarm join)")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := h.WaitReady(ctx, 10*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "longarm: hub %s unreachable (%s); %s\n", s.cfg.Hub.Address, oneLine(err), later)
		return
	}
	leaf, err := s.renewCert(ctx, h)
	if err != nil {
		fmt.Fprintf(os.Stderr, "longarm: certificate renewal failed (%s); %s\n", oneLine(err), later)
		return
	}
	note := "it carries the endorsement"
	if t, err := pki.TrustOf(leaf); err != nil || t == nil || t.Endorsement == nil {
		note = "but the hub dropped the endorsement: workers that require it will refuse this device"
	}
	fmt.Printf("certificate renewed (valid until %s); %s\n", leaf.NotAfter.Local().Format(time.RFC3339), note)
}

func trustInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create this device's user key (for the user of its certificate) and endorse this device with it",
		Long: `Create a user key for the user this device's certificate belongs to, endorse
this device with it and renew the certificate through the hub so that it
carries the endorsement. Run it once, on your first device; other devices
get endorsed with longarm trust request/endorse/accept or import the key.
Prints the user key line to give to the owners of workers you want to use
(longarmd trust add-user <line>).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := deviceBundle()
			if err != nil {
				return err
			}
			l, err := trustInit(trustStore(), b.Identity)
			if err != nil {
				return err
			}
			fmt.Printf("created the user key of %s (%s) and endorsed this device (%s)\n", l.User, pki.KeyID(l.Key), b.Identity.Name)
			renewForTrust(cmd.Context())
			fmt.Printf("\nyour user key (public; give it to the owners of the workers you should be able to use:\n  longarmd trust add-user <line>):\n%s\n", l)
			fmt.Println("\nkeep a copy of the secret key (longarm trust export-key) somewhere safe")
			return nil
		},
	}
}

func trustShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show this device's user key, node key, endorsement and trust policy",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return trustShow(os.Stdout, trustStore())
		},
	}
}

func trustShow(w io.Writer, ts trust.Store) error {
	ownKey, ownUser, err := ts.UserKey()
	switch {
	case err == nil:
		fmt.Fprintf(w, "user key:        %s (%s)\n                 %s\n", ownUser, pki.KeyID(ownKey.Public().(ed25519.PublicKey)),
			pki.UserKeyLine{User: ownUser, Key: ownKey.Public().(ed25519.PublicKey)})
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintln(w, "user key:        none (longarm trust init | import-key)")
	default:
		return err
	}
	nk, err := ts.NodeKey()
	if err != nil {
		return err
	}
	np := nk.Public().(ed25519.PublicKey)
	fmt.Fprintf(w, "node key:        %s (%s)\n", base64.StdEncoding.EncodeToString(np), pki.KeyID(np))
	e, err := ts.Endorsement()
	if err != nil {
		return err
	}
	if e == nil {
		fmt.Fprintln(w, "endorsement:     none (longarm trust init | request + accept)")
	} else {
		fmt.Fprintf(w, "endorsement:     %s %s, by %s (%s)\n", e.Kind, e.Name, e.User, pki.KeyID(e.UserKey))
	}
	if b, err := deviceBundle(); err != nil {
		fmt.Fprintf(w, "certificate:     %s\n", oneLine(err))
	} else {
		fmt.Fprintf(w, "certificate:     %s (user %s), %s\n", b.Identity, b.Identity.Owner(), certTrustState(b, np))
	}
	if ts.RenewalRequested() {
		fmt.Fprintln(w, "                 renewal pending (the next time longarm reaches the hub)")
	}
	f, err := ts.LoadPolicyFile()
	if err != nil {
		return err
	}
	p, err := ts.Policy(false)
	if err != nil {
		return err
	}
	req := "off (any worker with a certificate of the hub's CA)"
	if f.RequireWorkers {
		req = "on (only workers endorsed by a trusted user key)"
	}
	fmt.Fprintf(w, "require_workers: %s\n", req)
	var users []string
	if ownKey != nil {
		users = append(users, fmt.Sprintf("%s (%s, this device's key)", ownUser, pki.KeyID(ownKey.Public().(ed25519.PublicKey))))
	}
	for u, k := range p.Users {
		users = append(users, fmt.Sprintf("%s (%s)", u, pki.KeyID(k)))
	}
	sort.Strings(users)
	if len(users) == 0 {
		users = []string{"none"}
	}
	fmt.Fprintf(w, "trusted users:   %s\n", strings.Join(users, ", "))
	if len(f.DenyNodes) > 0 {
		fmt.Fprintf(w, "denied nodes:    %s\n", strings.Join(f.DenyNodes, ", "))
	}
	return nil
}

// certTrustState describes the trust extension of the device certificate.
func certTrustState(b *pki.Bundle, nodeKey ed25519.PublicKey) string {
	t, err := pki.TrustOf(b.Leaf())
	switch {
	case err != nil:
		return "trust extension invalid: " + err.Error()
	case t == nil:
		return "no trust extension (renewed through the hub after longarm trust init | accept, it gets one)"
	case !nodeKey.Equal(ed25519.PublicKey(t.NodeKey)):
		return "trust extension for another node key (renew it)"
	case t.Endorsement == nil:
		return "carries the node key, not endorsed"
	}
	e := t.Endorsement
	return fmt.Sprintf("carries the endorsement (%s %s by %s, %s)", e.Kind, e.Name, e.User, pki.KeyID(e.UserKey))
}

func trustRequestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "request",
		Short: "Print this device's endorsement request (for a device without the user key)",
		Long: `Print this device's endorsement request. On a device that holds your user key,
run longarm trust endorse <request>, then back here longarm trust accept
<endorsement>.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := deviceBundle()
			if err != nil {
				return err
			}
			r, err := trustStore().RequestFor(pki.KindDevice, b.Identity)
			if err != nil {
				return err
			}
			fmt.Println(r)
			fmt.Fprintf(os.Stderr, "on a device with %s's user key: longarm trust endorse <the line above>; then here: longarm trust accept <endorsement>\n", b.Identity.Owner())
			return nil
		},
	}
}

func trustEndorseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "endorse <request>",
		Short: "Endorse a worker's or a device's request with this device's user key",
		Long: `Endorse a request (longarmd trust request on a worker, longarm trust request on
another device of yours) with the user key held on this device, and print the
endorsement line for longarmd trust accept / longarm trust accept. Check the
kind, name and node key id printed on stderr against the request's origin:
an endorsement lets that node act as yours.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := trustEndorse(trustStore(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "endorsed %s %s (node key %s) as %s's\n", e.Kind, e.Name, pki.KeyID(e.NodeKey), e.User)
			fmt.Println(e)
			return nil
		},
	}
}

func trustAcceptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "accept <endorsement>",
		Short: "Install this device's endorsement and renew the certificate so it carries it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := deviceBundle()
			if err != nil {
				return err
			}
			e, err := trustAccept(trustStore(), args[0], b.Identity)
			if err != nil {
				return err
			}
			fmt.Printf("this device (%s) is endorsed by %s (%s)\n", e.Name, e.User, pki.KeyID(e.UserKey))
			renewForTrust(cmd.Context())
			return nil
		},
	}
}

func trustExportKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export-key",
		Short: "Print this device's user key (secret!) for longarm trust import-key on another device",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := trustStore().ExportUserKey()
			if errors.Is(err, os.ErrNotExist) {
				return errors.New("this device holds no user key")
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "longarm: this is your SECRET user key: whoever has it can endorse devices and workers as you; move it only to your own devices")
			os.Stdout.Write(b)
			return nil
		},
	}
}

func trustImportKeyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import-key <file|->",
		Short: "Install a user key exported with longarm trust export-key (and endorse this device with it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var data []byte
			var err error
			if args[0] == "-" {
				data, err = io.ReadAll(os.Stdin)
			} else {
				data, err = os.ReadFile(args[0])
			}
			if err != nil {
				return err
			}
			b, err := deviceBundle()
			if err != nil {
				return err
			}
			user, endorsed, err := trustImportKey(trustStore(), data, b.Identity)
			if err != nil {
				return err
			}
			fmt.Printf("imported the user key of %s\n", user)
			if endorsed {
				fmt.Printf("endorsed this device (%s) with it\n", b.Identity.Name)
				renewForTrust(cmd.Context())
			}
			return nil
		},
	}
}

func trustAddUserCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add-user <user-key>",
		Short: "Accept workers endorsed by this user key (with longarm trust require on)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ts := trustStore()
			l, err := pki.ParseUserKeyLine(args[0])
			if err != nil {
				return err
			}
			if k, u, err := ts.UserKey(); err == nil && u == l.User && !k.Public().(ed25519.PublicKey).Equal(ed25519.PublicKey(l.Key)) {
				return fmt.Errorf("this device holds another user key of %s", u)
			}
			if err := ts.AddUser(l); err != nil {
				return err
			}
			fmt.Printf("workers endorsed by %s (%s) are accepted\n", l.User, pki.KeyID(l.Key))
			if f, err := ts.LoadPolicyFile(); err == nil && !f.RequireWorkers {
				fmt.Println("note: require_workers is off, so any worker is accepted (longarm trust require on)")
			}
			return nil
		},
	}
}

func trustRemoveUserCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove-user <user>",
		Short: "Stop accepting workers endorsed by this user (added with add-user)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ts := trustStore()
			if err := ts.RemoveUser(args[0]); err != nil {
				return err
			}
			if f, err := ts.LoadPolicyFile(); err == nil && f.RequireWorkers {
				if _, err := ts.Policy(true); err != nil {
					fmt.Fprintf(os.Stderr, "longarm: warning: %v: every worker is refused now (longarm trust require off)\n", err)
				}
			}
			fmt.Printf("workers endorsed by %s are no longer accepted\n", args[0])
			return nil
		},
	}
}

func trustRequireCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "require on|off",
		Short: "Only talk to workers endorsed by a trusted user key (on), or to any worker of the hub's CA (off)",
		Long: `With require on, longarm (and longarm daemon / longarm mcp) refuses workers whose
certificate does not carry an endorsement by this device's user key or a user
added with longarm trust add-user, even if the hub's CA issued it.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			var on bool
			switch args[0] {
			case "on":
				on = true
			case "off":
			default:
				return fmt.Errorf("require %q: on or off", args[0])
			}
			if err := trustStore().SetRequireWorkers(on); err != nil {
				return fmt.Errorf("%w (longarm trust init, or longarm trust add-user <user-key>)", err)
			}
			if !on {
				fmt.Println("require_workers off: any worker with a certificate of the hub's CA is accepted")
				return nil
			}
			p, err := trustStore().Policy(true)
			if err != nil {
				return err
			}
			fmt.Printf("require_workers on: only workers endorsed by %s\n", trust.Summary(p))
			return nil
		},
	}
}

func trustDenyNodeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deny-node <node-key>",
		Short: "Refuse a node key (base64, longarm trust show / longarmd trust show) even when endorsed",
		Long: `Refuse a worker's node key even when it is endorsed, e.g. a worker that was
lost or handed over before its endorsement could be withdrawn. Takes effect
with longarm trust require on.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := trustStore().DenyNode(args[0]); err != nil {
				return err
			}
			fmt.Println("denied node key", args[0])
			return nil
		},
	}
}
