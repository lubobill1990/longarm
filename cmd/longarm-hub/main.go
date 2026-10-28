// Command longarm-hub is the Longarm control plane (DESIGN §3.3): CA and
// enrollment, node registry, job index, scheduler, events, audit, toolchain
// profiles, the longnet relay, and the HTTP API/panel with install scripts.
//
//	longarm-hub serve [--config hub.toml]
//	longarm-hub ca import <dir> [--force] | ca init | ca show
//	longarm-hub user add <name> [--password-stdin] [--reset] | user ls | user rm <name>
//	longarm-hub join-code [--role worker|client|admin] [--ttl 10m] [--label k=v] [--profile P] [--name N]
//	longarm-hub release add <dir> --version V [--channel C] | sign <dir> --version V --key K | keygen | ls
//	longarm-hub release verify <file> --version V [--name N]   (signature check, used by deploy/hub/longarm-hub-deploy)
//	longarm-hub issue --role admin --name root --out DIR      (local, for bootstrap; --role release for CI)
//	longarm-hub healthcheck                                   (container health check)
//	longarm-hub bootstrap [--admin-code-ttl 24h]              (CA + admin join code when missing; idempotent)
//	longarm-hub container                                     (image entrypoint: litestream restore, bootstrap, serve)
//
// Every subcommand takes --config (default $LONGARM_HUB_CONFIG) and/or
// --data-dir (default $LONGARM_HUB_DATA_DIR or ~/.longarm-hub).
package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/hub"
	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/sched"
	"github.com/lubobill1990/longarm/internal/update"
	"github.com/lubobill1990/longarm/internal/version"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

var configPath, dataDir string

func loadConfig() (*hub.Config, error) {
	cfg, err := hub.LoadConfig(configPath)
	if err != nil {
		return nil, err
	}
	if dataDir != "" {
		cfg.DataDir = dataDir
	}
	return cfg, nil
}

func main() {
	root := &cobra.Command{
		Use:           "longarm-hub",
		Short:         "Longarm control plane (registry, enrollment, scheduler, relay, panel API)",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", os.Getenv("LONGARM_HUB_CONFIG"), "hub.toml (env LONGARM_HUB_CONFIG)")
	root.PersistentFlags().StringVar(&dataDir, "data-dir", "", "data directory (overrides hub.toml data_dir; env LONGARM_HUB_DATA_DIR)")
	root.AddCommand(serveCmd(), caCmd(), userCmd(), joinCodeCmd(), releaseCmd(), issueCmd(), healthcheckCmd(), bootstrapCmd(), containerCmd())
	if args, err := aliasArgs(os.Args[0]); err != nil {
		fmt.Fprintln(os.Stderr, "longarm-hub:", err)
		os.Exit(1)
	} else if args != nil {
		root.SetArgs(args)
	}
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "longarm-hub:", err)
		os.Exit(1)
	}
}

func serveCmd() *cobra.Command {
	var rebuildJobs bool
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the hub",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			log, closer, err := hub.NewLogger(cfg)
			if err != nil {
				return err
			}
			defer closer.Close()
			h, err := hub.Open(cfg, log)
			if err != nil {
				return err
			}
			defer h.Close()
			if rebuildJobs {
				if err := h.Store().ClearJobs(); err != nil {
					return err
				}
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return h.Serve(ctx)
		},
	}
	c.Flags().BoolVar(&rebuildJobs, "rebuild-jobs", false, "drop the job cache at start (workers report their jobs again)")
	return c
}

func openHub() (*hub.Hub, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return hub.Open(cfg, nil)
}

func openStore() (*store.Store, *hub.Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DatabasePath()), 0o700); err != nil {
		return nil, nil, err
	}
	st, err := store.Open(cfg.DatabasePath())
	return st, cfg, err
}

func caCmd() *cobra.Command {
	c := &cobra.Command{Use: "ca", Short: "Manage the hub's certificate authority"}
	var force bool
	imp := &cobra.Command{
		Use:   "import <dir>",
		Short: "Import an existing CA (ca.crt + ca.key, e.g. ~/.config/longarm/ca), keeping issued certificates valid",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ca, err := hub.ImportCA(cfg.DataDir, args[0], force)
			if err != nil {
				return err
			}
			fmt.Printf("imported CA %q\nfingerprint %s\n", ca.Cert.Subject.CommonName, pki.Fingerprint(ca.Cert))
			return nil
		},
	}
	imp.Flags().BoolVar(&force, "force", false, "replace an existing CA")
	initc := &cobra.Command{
		Use:   "init",
		Short: "Create a new CA in the data dir",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ca, err := hub.InitCA(cfg.DataDir)
			if err != nil {
				return err
			}
			fmt.Printf("fingerprint %s\n", pki.Fingerprint(ca.Cert))
			return nil
		},
	}
	show := &cobra.Command{
		Use:   "show",
		Short: "Print the CA certificate's fingerprint (for --ca-fingerprint)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ca, err := pki.LoadCA(cfg.DataDir + string(os.PathSeparator) + hub.CADir)
			if err != nil {
				return err
			}
			fmt.Printf("subject     %s\nnot after   %s\nfingerprint %s\n", ca.Cert.Subject.CommonName, ca.Cert.NotAfter.Format(time.RFC3339), pki.Fingerprint(ca.Cert))
			return nil
		},
	}
	c.AddCommand(imp, initc, show)
	return c
}

func readPassword(stdin bool) (string, error) {
	if stdin || !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, "password: ")
	a, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "again: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(a) != string(b) {
		return "", errors.New("passwords differ")
	}
	return string(a), nil
}

func userCmd() *cobra.Command {
	c := &cobra.Command{Use: "user", Short: "Manage web panel accounts (password + TOTP)"}
	var pwStdin, reset bool
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Create an account; prints the TOTP secret to add to an authenticator app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, _, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			pw, err := readPassword(pwStdin)
			if err != nil {
				return err
			}
			secret, err := hub.AddUser(st, args[0], pw, reset)
			if err != nil {
				return err
			}
			fmt.Printf("user %s created\ntotp_secret %s\notpauth %s\n", args[0], secret, hub.TOTPURI(args[0], secret))
			return nil
		},
	}
	add.Flags().BoolVar(&pwStdin, "password-stdin", false, "read the password from stdin")
	add.Flags().BoolVar(&reset, "reset", false, "replace an existing user's password and TOTP secret")
	ls := &cobra.Command{
		Use: "ls", Short: "List accounts", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, _, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			names, err := st.Users()
			for _, n := range names {
				fmt.Println(n)
			}
			return err
		},
	}
	rm := &cobra.Command{
		Use: "rm <name>", Short: "Delete an account (and its sessions)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, _, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			return st.DeleteUser(args[0])
		},
	}
	c.AddCommand(add, ls, rm)
	return c
}

func parseKV(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	m := map[string]string{}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%q: want KEY=VALUE", kv)
		}
		m[k] = v
	}
	return m, nil
}

func joinCodeCmd() *cobra.Command {
	var role, profile, name string
	var ttl time.Duration
	var labels []string
	c := &cobra.Command{
		Use:   "join-code",
		Short: "Create a one-time join code locally (bootstrap; normally `longarm admin join-code`)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lm, err := sched.ParseLabels(labels, false)
			if err != nil {
				return err
			}
			h, err := openHub()
			if err != nil {
				return err
			}
			defer h.Close()
			jc, err := h.CreateJoinCode("local", &pb.CreateJoinCodeRequest{Role: role, Ttl: durationpb.New(ttl), Labels: lm, Profile: profile, Name: name})
			if err != nil {
				return err
			}
			fmt.Printf("%s\n", jc.Code)
			fmt.Fprintf(os.Stderr, "role %s, expires %s, CA fingerprint %s\n", jc.Role, jc.ExpiresAt.AsTime().Local().Format(time.RFC3339), pki.Fingerprint(h.CA().Cert))
			return nil
		},
	}
	c.Flags().StringVar(&role, "role", "worker", "worker | client | admin")
	c.Flags().DurationVar(&ttl, "ttl", 10*time.Minute, "validity")
	c.Flags().StringArrayVar(&labels, "labels", nil, "labels for the node: KEY=VALUE[,KEY=VALUE...] (repeatable)")
	c.Flags().StringArrayVar(&labels, "label", nil, "same as --labels")
	c.Flags().MarkHidden("label")
	c.Flags().StringVar(&profile, "profile", "", "toolchain profile for the worker")
	c.Flags().StringVar(&name, "name", "", "fixed node name")
	return c
}

func releaseCmd() *cobra.Command {
	c := &cobra.Command{Use: "release", Short: "Manage the release store (install scripts, self-update)"}
	var ver, channel string
	add := &cobra.Command{
		Use:   "add <dir>",
		Short: "Import release files (<tool>-<os>-<arch>[.exe], longarm-<os>-<arch>.msi|deb|rpm|pkg|tar.gz|zip, + .minisig) as a version",
		Long: `Import release files as a version on a channel. Signatures (<file>.minisig,
from 'longarm-hub release sign') are stored with them; workers only update to
signed binaries. With [releases] public_key in hub.toml every signature is
checked here and unsigned update binaries are refused. Rolling a version out
to workers is 'longarm admin release push'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, cfg, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			rels, err := hub.AddRelease(cmd.Context(), cfg, st, args[0], ver, channel)
			if err != nil {
				return err
			}
			for _, r := range rels {
				signed := ""
				if r.Sig != nil {
					signed = " signed"
				}
				fmt.Printf("%s %s %s %s%s\n", r.Version, r.Channel, r.File, r.SHA256, signed)
			}
			return nil
		},
	}
	add.Flags().StringVar(&ver, "version", "", "version label (required)")
	add.Flags().StringVar(&channel, "channel", "stable", "release channel")
	add.MarkFlagRequired("version")
	var keyFile, passEnv string
	sign := &cobra.Command{
		Use:   "sign <dir>",
		Short: "Sign the release files in <dir> with a minisign key (writes <file>.minisig)",
		Long: `Sign every release file in <dir>. The trusted comment binds each signature to
the file name and version, so it cannot be replayed for another file or an
older version. Run this on the build/admin machine, not on the hub: the
private key never needs to be on the hub ('longarm admin release publish' signs
and uploads in one step).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			signed, err := hub.SignRelease(args[0], ver, keyFile, os.Getenv(passEnv))
			if err != nil {
				return err
			}
			for _, f := range signed {
				fmt.Println("signed", f)
			}
			return nil
		},
	}
	sign.Flags().StringVar(&ver, "version", "", "version the files belong to (required)")
	sign.Flags().StringVar(&keyFile, "key", "", "minisign private key file (required)")
	sign.Flags().StringVar(&passEnv, "password-env", "LONGARM_RELEASE_KEY_PASSWORD", "env var with the key's password (if encrypted)")
	sign.MarkFlagRequired("version")
	sign.MarkFlagRequired("key")
	var outDir string
	keygen := &cobra.Command{
		Use:   "keygen",
		Short: "Create a release signing key pair (prints the public key to build into longarmd)",
		Long: `Create the minisign key pair that signs releases. Run it on the admin's
machine, not on the hub: by default the pair goes to release/ in longarm's config
dir (~/.config/longarm/release), where 'longarm admin release sign|publish' find
it. Put the public key line into the build (LONGARM_UPDATE_PUBKEY) and into
hub.toml [releases] public_key; the private key never leaves this machine.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pub, priv, err := update.GenerateKey(os.Getenv(passEnv))
			if err != nil {
				return err
			}
			if err := os.MkdirAll(outDir, 0o700); err != nil {
				return err
			}
			kf := filepath.Join(outDir, update.KeyFile)
			if _, err := os.Stat(kf); err == nil {
				return fmt.Errorf("%s exists", kf)
			}
			if err := os.WriteFile(kf, priv, 0o600); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(outDir, update.PubFile), []byte(pub+"\n"), 0o644); err != nil {
				return err
			}
			fmt.Println(pub)
			fmt.Fprintf(os.Stderr, "private key: %s (keep it off the hub)\nbuild with: -ldflags \"-X github.com/lubobill1990/longarm/internal/update.PublicKey=%s\"\n", kf, pub)
			return nil
		},
	}
	keygen.Flags().StringVar(&outDir, "out", update.DefaultKeyDir(), "directory for longarm-release.key / longarm-release.pub (on the admin's machine)")
	keygen.Flags().StringVar(&passEnv, "password-env", "LONGARM_RELEASE_KEY_PASSWORD", "env var with a password to encrypt the key (empty = unencrypted)")
	ls := &cobra.Command{
		Use: "ls", Short: "List release files", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, _, err := openStore()
			if err != nil {
				return err
			}
			defer st.Close()
			rels, err := st.Releases()
			if err != nil {
				return err
			}
			latest, _ := st.Meta("latest_release")
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "VERSION\tCHANNEL\tFILE\tSIZE\tSIGNED\tSHA256")
			for _, r := range rels {
				v := r.Version
				if v == latest {
					v += " (latest)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%v\t%s\n", v, r.Channel, r.File, r.Size, r.Sig != nil, r.SHA256)
			}
			return tw.Flush()
		},
	}
	var vName, vSig, vKey string
	verify := &cobra.Command{
		Use:   "verify <file>",
		Short: "Check a release file's signature (<file>.minisig) against [releases] public_key",
		Long: `Check that <file> carries a valid minisign signature by the release key in
hub.toml [releases] public_key (or --public-key), bound to --version and
the canonical file name (--name, default: the file's base name). Exit status
0 = valid. deploy/hub/longarm-hub-deploy runs it with the installed longarm-hub before
it installs a new one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := vKey
			if key == "" {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				key = cfg.Releases.PublicKey
			}
			if strings.TrimSpace(key) == "" {
				return errors.New("no public key: set [releases] public_key in hub.toml or pass --public-key")
			}
			name := cmp.Or(vName, filepath.Base(args[0]))
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			sig, err := os.ReadFile(cmp.Or(vSig, args[0]+update.SigExt))
			if err != nil {
				return err
			}
			if err := update.Verify(key, data, sig, name, ver); err != nil {
				return err
			}
			fmt.Printf("%s: valid signature for %s %s\n", args[0], name, ver)
			return nil
		},
	}
	verify.Flags().StringVar(&ver, "version", "", "version the signature must name (required)")
	verify.Flags().StringVar(&vName, "name", "", "release file name the signature must name (default: base name of <file>)")
	verify.Flags().StringVar(&vSig, "sig", "", "signature file (default <file>.minisig)")
	verify.Flags().StringVar(&vKey, "public-key", "", "minisign public key (default: [releases] public_key)")
	verify.MarkFlagRequired("version")
	c.AddCommand(add, sign, keygen, ls, verify)
	return c
}

func issueCmd() *cobra.Command {
	var role, name, out string
	var days int
	var b64 bool
	c := &cobra.Command{
		Use:   "issue",
		Short: "Issue a certificate bundle from the hub's CA locally (e.g. the first admin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ca, err := pki.LoadCA(cfg.DataDir + string(os.PathSeparator) + hub.CADir)
			if err != nil {
				return err
			}
			r, err := pki.ParseRole(role)
			if err != nil {
				return err
			}
			if name == "" && r == pki.RoleRelease {
				name = "ci-" + time.Now().UTC().Format("20060102")
			}
			if name == "" {
				return errors.New("--name is required")
			}
			if out == "" && !b64 {
				return errors.New("--out DIR or --b64")
			}
			certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{Role: r, Name: name, Validity: time.Duration(days) * 24 * time.Hour})
			if err != nil {
				return err
			}
			if b64 {
				// the LONGARM_RELEASE_TLS secret's format (docs/ci-cd.md):
				// base64 of a tar.gz with ca.crt, node.crt, node.key
				s, err := bundleTarB64(ca.CertPEM, certPEM, keyPEM)
				if err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "%s/%s bundle (base64 tar.gz of ca.crt node.crt node.key; it holds a private key):\n", r, name)
				fmt.Println(s)
				return nil
			}
			if err := pki.WriteBundle(out, ca.CertPEM, certPEM, keyPEM); err != nil {
				return err
			}
			fmt.Printf("wrote %s/%s bundle to %s\n", r, name, out)
			return nil
		},
	}
	c.Flags().StringVar(&role, "role", "admin", "admin | client | worker | release (the CI credential, docs/ci-cd.md)")
	c.Flags().StringVar(&name, "name", "", "node name (release: default ci-<date>)")
	c.Flags().StringVar(&out, "out", "", "output dir (ca.crt, node.crt, node.key)")
	c.Flags().BoolVar(&b64, "b64", false, "print the bundle as base64 of a tar.gz instead of writing files (CI secrets; az container exec)")
	c.Flags().IntVar(&days, "days", 0, "validity in days (default by role)")
	return c
}

// bundleTarB64 is base64 of a tar.gz with ca.crt, node.crt and node.key.
func bundleTarB64(caPEM, certPEM, keyPEM []byte) (string, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		mode int64
		data []byte
	}{{pki.CACertFile, 0o644, caPEM}, {pki.NodeCertFile, 0o644, certPEM}, {pki.NodeKeyFile, 0o600, keyPEM}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), ModTime: time.Now()}); err != nil {
			return "", err
		}
		if _, err := tw.Write(f.data); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

func healthcheckCmd() *cobra.Command {
	var url string
	c := &cobra.Command{
		Use:   "healthcheck",
		Short: "Exit 0 when the hub's /healthz answers (for container health checks)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if url == "" {
				cfg, err := loadConfig()
				if err != nil {
					return err
				}
				addr := cfg.HTTP
				if strings.HasPrefix(addr, ":") || strings.HasPrefix(addr, "0.0.0.0:") {
					addr = "127.0.0.1:" + addr[strings.LastIndex(addr, ":")+1:]
				}
				url = "http://" + addr + "/healthz"
			}
			c := &http.Client{Timeout: 5 * time.Second}
			resp, err := c.Get(url)
			if err != nil {
				return err
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("%s: %s", url, resp.Status)
			}
			return nil
		},
	}
	c.Flags().StringVar(&url, "url", "", "health URL (default: from the config's http listener)")
	return c
}
