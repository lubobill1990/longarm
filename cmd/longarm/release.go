package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/update"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// releaseCmd is `longarm admin release`: the hub's releases and the rollout of
// a version to the workers (DESIGN §7.2).
func releaseCmd() *cobra.Command {
	c := &cobra.Command{Use: "release", Short: "Releases on the hub and rolling them out to workers (self-update)"}
	var asJSON bool
	ls := &cobra.Command{
		Use: "ls", Short: "List the hub's releases and the current rollout", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				resp, err := h.Admin().ListReleases(ctx, &pb.ListReleasesRequest{IncludeIncomplete: true})
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(resp)
				}
				tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(tw, "VERSION\tCHANNEL\tSIGNED\tFILES\tADDED")
				var lost []string
				for _, r := range resp.Releases {
					var names []string
					for _, f := range r.Files {
						if f.Missing {
							names = append(names, f.Name+"(missing)")
							if !slices.Contains(lost, r.Version) {
								lost = append(lost, r.Version)
							}
							continue
						}
						names = append(names, f.Name)
					}
					fmt.Fprintf(tw, "%s\t%s\t%v\t%s\t%s\n", r.Version, r.Channel, r.Signed, strings.Join(names, " "),
						r.AddedAt.AsTime().Local().Format("2006-01-02 15:04"))
				}
				tw.Flush()
				if len(lost) > 0 {
					fmt.Fprintf(os.Stderr, "the hub lost files of %s: install scripts and rollouts skip them until the same files are published again (longarm admin release publish <dir> --version V)\n", strings.Join(lost, ", "))
				}
				if r := resp.Rollout; r != nil && r.Version != "" {
					fmt.Println()
					printRollout(r)
				}
				return nil
			})
		},
	}
	ls.Flags().BoolVar(&asJSON, "json", false, "JSON output")

	var canary, batch int32
	var wait, batchWait time.Duration
	var downgrade, follow, allowNone bool
	var nodes []string
	push := &cobra.Command{
		Use:   "push <version>",
		Short: "Roll a version out: canary workers first, then the rest in batches",
		Long: `Roll <version> (added with 'longarm-hub release add', signed) out to the workers of
its channel. The --canary workers (online, least busy first) update first;
once they all run the new version and stay online for --wait, the others
follow in batches of --batch (0 = all at once) with --batch-wait between
batches. A worker whose new version fails its 60 s health check rolls back
by itself and halts the rollout. Example:

  longarm admin release push 1.4.0 --canary 1 --wait 30m

Workers that can't update themselves (MSI installs under Program Files,
user-session workers) are skipped: they get the version as a notice and
update with the MSI (the tray's "Update to" banner) or the install script.
When no worker of the channel can self-update, push fails - with
--allow-none it lists them and exits 79 instead (CI: a warning, not a
failure).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Admin().PushRelease(ctx, &pb.PushReleaseRequest{Version: args[0], Canary: canary,
					Wait: durationpb.New(wait), Batch: batch, BatchWait: durationpb.New(batchWait),
					AllowDowngrade: downgrade, Nodes: nodes, AllowNone: allowNone})
				if err != nil {
					return err
				}
				printRollout(r)
				if r.State == "none" {
					msg := "nothing rolled out: no approved worker of channel " + r.Channel
					if n := max(int(r.NotCapableCount), len(r.NotCapable)); n > 0 {
						which := strings.Join(r.NotCapable, ", ")
						if hidden := n - len(r.NotCapable); hidden > 0 { // other users' workers: counted, not named
							which = strings.TrimPrefix(fmt.Sprintf("%s, %d of other users", which, hidden), ", ")
						}
						msg = fmt.Sprintf("nothing rolled out: no worker can self-update %s (%d not capable: %s); they update with the MSI (the tray's banner) or the install script",
							r.Version, n, which)
					}
					return &exitError{code: exitNoneCapable, msg: msg}
				}
				if follow {
					return followRollout(cmd, h)
				}
				return nil
			})
		},
	}
	push.Flags().Int32Var(&canary, "canary", 1, "canary workers (0 = no canary)")
	push.Flags().DurationVar(&wait, "wait", 30*time.Minute, "how long the canaries must stay healthy")
	push.Flags().Int32Var(&batch, "batch", 0, "workers per batch after the canary (0 = all)")
	push.Flags().DurationVar(&batchWait, "batch-wait", 5*time.Minute, "pause between batches")
	push.Flags().BoolVar(&downgrade, "allow-downgrade", false, "also move workers that run a newer version")
	push.Flags().StringSliceVar(&nodes, "nodes", nil, "only these workers")
	push.Flags().BoolVarP(&follow, "follow", "f", false, "follow the rollout until it ends")
	push.Flags().BoolVar(&allowNone, "allow-none", false, "no worker can self-update: list them and exit 79 instead of failing")

	status := &cobra.Command{
		Use: "status", Short: "Show the current rollout", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Admin().GetRollout(ctx, &pb.GetRolloutRequest{})
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(r)
				}
				if r.Version == "" {
					fmt.Println("no rollout")
					return nil
				}
				printRollout(r)
				if follow {
					return followRollout(cmd, h)
				}
				return nil
			})
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "JSON output")
	status.Flags().BoolVarP(&follow, "follow", "f", false, "follow the rollout until it ends")
	abort := &cobra.Command{
		Use: "abort", Short: "Stop the current rollout (updated workers keep the new version)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				r, err := h.Admin().AbortRollout(ctx, &pb.GetRolloutRequest{})
				if err != nil {
					return err
				}
				printRollout(r)
				return nil
			})
		},
	}
	c.AddCommand(ls, releaseSignCmd(), releasePublishCmd(), push, status, abort)
	return c
}

func printJSON(m proto.Message) error {
	b, err := protojson.MarshalOptions{Multiline: true, Indent: "  ", UseProtoNames: true}.Marshal(m)
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func printRollout(r *pb.Rollout) {
	fmt.Printf("rollout %s (channel %s): %s", r.Version, r.Channel, r.State)
	if r.Message != "" {
		fmt.Printf(" - %s", r.Message)
	}
	fmt.Println()
	// The hub names only the workers this admin may see as a user (own,
	// shared, fleet); the counts include every worker.
	line := func(k string, v []string, n int32) {
		n = max(n, int32(len(v)))
		if n == 0 {
			return
		}
		names := strings.Join(v, " ")
		if hidden := int(n) - len(v); hidden > 0 {
			if names != "" {
				names += " "
			}
			names += fmt.Sprintf("+%d of other users", hidden)
		}
		fmt.Printf("  %-9s %s\n", k, names)
	}
	line("canary", r.Canary, r.CanaryCount)
	line("targeted", r.Targeted, r.TargetedCount)
	line("updated", r.Updated, r.UpdatedCount)
	line("failed", r.Failed, r.FailedCount)
	line("waiting", r.Waiting, r.WaitingCount)
	if r.PhaseSince != nil {
		fmt.Printf("  healthy since %s\n", r.PhaseSince.AsTime().Local().Format(time.TimeOnly))
	}
}

// followRollout prints the rollout whenever it changes, until it ends.
func followRollout(cmd *cobra.Command, h *hubclient.Conn) error {
	last := ""
	for {
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		r, err := h.Admin().GetRollout(ctx, &pb.GetRolloutRequest{})
		cancel()
		if err != nil {
			return err
		}
		b, _ := protojson.Marshal(r)
		if s := string(b); s != last {
			last = s
			printRollout(r)
		}
		switch r.State {
		case "done", "halted", "aborted":
			if r.State != "done" {
				return &exitError{code: 1, msg: "rollout " + r.State}
			}
			return nil
		}
		select {
		case <-cmd.Context().Done():
			return cmd.Context().Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// releaseKeyFlags are --key / --password-env of sign and publish: the
// private key stays on this (the admin's) machine.
func releaseKeyFlags(c *cobra.Command, keyFile, passEnv *string) {
	c.Flags().StringVar(keyFile, "key", "", "minisign private key (default: release/"+update.KeyFile+" in the config dir; create it with longarm-hub release keygen)")
	c.Flags().StringVar(passEnv, "password-env", "LONGARM_RELEASE_KEY_PASSWORD", "env var with the key's password (if encrypted)")
}

func releaseKey(keyFile string) string {
	if keyFile != "" {
		return keyFile
	}
	return filepath.Join(configDir, "release", update.KeyFile)
}

// releaseSignCmd is `longarm admin release sign <dir> --version V`: sign the
// release files locally (writes <file>.minisig).
func releaseSignCmd() *cobra.Command {
	var ver, keyFile, passEnv string
	c := &cobra.Command{
		Use:   "sign <dir>",
		Short: "Sign release files locally with the release key (writes <file>.minisig)",
		Long: `Sign every release file in <dir> (longarm-<os>-<arch>[.exe], longarmd-..., longarm-<os>-<arch>.msi|deb|...)
with the release key on this machine. The signature is bound to the file name
and the version. Upload with 'longarm admin release publish'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			files, err := update.SignDir(args[0], ver, releaseKey(keyFile), os.Getenv(passEnv))
			if err != nil {
				return err
			}
			if len(files) == 0 {
				return fmt.Errorf("no release files in %s", args[0])
			}
			for _, f := range files {
				fmt.Println("signed", f)
			}
			return nil
		},
	}
	c.Flags().StringVar(&ver, "version", "", "version the files belong to (required)")
	c.MarkFlagRequired("version")
	releaseKeyFlags(c, &keyFile, &passEnv)
	return c
}

// releasePublishCmd is `longarm admin release publish <dir> --version V`: sign
// locally (unless --no-sign) and upload the files to the hub, which checks
// the signatures against its [releases] public_key and stores them.
func releasePublishCmd() *cobra.Command {
	var ver, channel, keyFile, passEnv string
	var noSign, replace, skipSame bool
	c := &cobra.Command{
		Use:   "publish <dir>",
		Short: "Sign release files locally and upload them to the hub (it never sees the private key)",
		Long: `Sign the release files in <dir> with the release key on this machine (see
'longarm admin release sign'; --no-sign uploads existing .minisig files as they
are) and upload them to the hub as a version on a channel - the same as
'longarm-hub release add' on the hub, without copying files there. Then roll it
out with 'longarm admin release push'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			channel, _ = update.NormalizeChannel(channel) // beta is preview (older hubs store what they get)
			if !noSign {
				if _, err := update.SignDir(dir, ver, releaseKey(keyFile), os.Getenv(passEnv)); err != nil {
					return fmt.Errorf("sign: %w (--no-sign uploads existing signatures)", err)
				}
			}
			files, err := update.ReleaseFiles(dir)
			if err != nil {
				return err
			}
			if len(files) == 0 {
				return fmt.Errorf("no release files in %s (want names like longarmd-linux-amd64, longarm-windows-amd64.exe, longarm-windows-amd64.msi)", dir)
			}
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				if skipSame && !replace {
					same, err := sameRelease(ctx, h, dir, ver, channel, files)
					if err != nil {
						return err
					}
					if same {
						fmt.Printf("%s is already on the hub with these %d files: nothing to upload\n", ver, len(files))
						return nil
					}
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), time.Hour)
				defer cancel()
				st, err := h.Admin().PublishRelease(ctx)
				if err != nil {
					return err
				}
				if err := st.Send(&pb.ReleaseUpload{Msg: &pb.ReleaseUpload_Header{Header: &pb.ReleaseUploadHeader{Version: ver, Channel: channel, Replace: replace}}}); err != nil {
					return uploadErr(st, err)
				}
				for _, name := range files {
					if err := uploadReleaseFile(st, dir, name); err != nil {
						return uploadErr(st, err)
					}
					fmt.Fprintln(os.Stderr, "uploaded", name)
				}
				v, err := st.CloseAndRecv()
				if err != nil {
					return err
				}
				fmt.Printf("%s (%s) on the hub: %d files, signed=%v\n", v.Version, v.Channel, len(v.Files), v.Signed)
				return nil
			})
		},
	}
	c.Flags().StringVar(&ver, "version", "", "version label (required)")
	c.Flags().StringVar(&channel, "channel", "stable", "release channel: stable, preview (beta) or dev (docs/ci-cd.md \"Release channels\")")
	c.Flags().BoolVar(&noSign, "no-sign", false, "don't sign; upload the existing .minisig files")
	c.Flags().BoolVar(&replace, "replace", false, "overwrite a version that exists on the hub")
	c.Flags().BoolVar(&skipSame, "skip-identical", false, "succeed without uploading when the hub has this version with exactly these files (same names and sha256; for re-runs); different files still fail")
	c.MarkFlagRequired("version")
	releaseKeyFlags(c, &keyFile, &passEnv)
	return c
}

// sameRelease reports whether the hub has version with exactly the files in
// dir (names and sha256). A version with other files is an error.
func sameRelease(ctx context.Context, h *hubclient.Conn, dir, version, channel string, files []string) (bool, error) {
	resp, err := h.Admin().ListReleases(ctx, &pb.ListReleasesRequest{})
	if err != nil {
		return false, err
	}
	want, _ := update.NormalizeChannel(channel) // beta is preview
	for _, r := range resp.Releases {
		if r.Version != version {
			continue
		}
		if have, _ := update.NormalizeChannel(r.Channel); have != want {
			return false, fmt.Errorf("%s exists on the hub on channel %s, not %s", version, r.Channel, channel)
		}
		have := map[string]string{}
		for _, f := range r.Files {
			have[f.Name] = f.Sha256
		}
		if len(have) != len(files) {
			return false, fmt.Errorf("%s exists on the hub with %d files, %d here (--replace overwrites)", version, len(have), len(files))
		}
		for _, name := range files {
			sum, err := fileSHA256(filepath.Join(dir, name))
			if err != nil {
				return false, err
			}
			if have[name] != sum {
				return false, fmt.Errorf("%s exists on the hub with a different %s (--replace overwrites)", version, name)
			}
		}
		return true, nil
	}
	return false, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// uploadErr prefers the hub's reason (the stream breaks with io.EOF on Send
// when the hub refused the upload).
func uploadErr(st pb.HubAdmin_PublishReleaseClient, err error) error {
	if errors.Is(err, io.EOF) {
		if _, rerr := st.CloseAndRecv(); rerr != nil {
			return rerr
		}
	}
	return err
}

func uploadReleaseFile(st pb.HubAdmin_PublishReleaseClient, dir, name string) error {
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	defer f.Close()
	sig, err := os.ReadFile(filepath.Join(dir, name+update.SigExt))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	buf := make([]byte, 1<<20)
	for {
		n, rerr := io.ReadFull(f, buf)
		last := rerr == io.EOF || rerr == io.ErrUnexpectedEOF
		if rerr != nil && !last {
			return rerr
		}
		c := &pb.ReleaseUploadChunk{Name: name, Data: buf[:n], Last: last}
		if last {
			c.Signature = sig
		}
		if err := st.Send(&pb.ReleaseUpload{Msg: &pb.ReleaseUpload_Chunk{Chunk: c}}); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}
