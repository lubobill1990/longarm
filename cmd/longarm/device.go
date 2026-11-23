package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"longarm/internal/client"
	"longarm/internal/hubclient"
	"longarm/internal/pki"
	"longarm/internal/trust"
	pb "longarm/proto/longarm/v1"
)

func pairingWarning(dir string, w io.Writer, err error) {
	fmt.Fprintf(w, "longarm: automatic trust: %s\n", oneLine(err))
	if auditErr := trust.PairFailed(dir, err); auditErr != nil {
		fmt.Fprintf(w, "longarm: recording trust/pair-failed: %s\n", oneLine(auditErr))
	}
}

func deviceCmd() *cobra.Command {
	c := &cobra.Command{Use: "device", Short: "Trust another device of yours"}
	var yes bool
	approve := &cobra.Command{
		Use: "approve <secret>", Short: "Endorse a device using the secret it showed you", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withHub(cmd, func(ctx context.Context, s *session, h *hubclient.Conn) error {
				err := approveDevice(ctx, h.Fleet(), client.TrustStore(s.cfg.Dir), args[0], func(p *pb.TrustPairing) (bool, error) {
					fmt.Fprintf(cmd.ErrOrStderr(), "Trust device %s (%s, %s)? [y/N] ", p.NodeName, p.Hostname, p.Os)
					if yes {
						fmt.Fprintln(cmd.ErrOrStderr(), "yes")
						return true, nil
					}
					line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
					if err != nil && line == "" {
						return false, err
					}
					answer := strings.ToLower(strings.TrimSpace(line))
					return answer == "y" || answer == "yes", nil
				})
				if err != nil {
					pairingWarning(s.cfg.Dir, cmd.ErrOrStderr(), err)
					return nil
				}
				return nil
			})
		},
	}
	approve.Flags().BoolVar(&yes, "yes", false, "approve without prompting")
	c.AddCommand(approve)
	return c
}

func approveDevice(ctx context.Context, fleet pb.FleetClient, ts trust.Store, secret string, confirm func(*pb.TrustPairing) (bool, error)) error {
	pair, err := pki.DerivePairing(secret)
	if err != nil {
		return err
	}
	key, user, err := ownUserKey(ts)
	if err != nil {
		return err
	}
	resp, err := fleet.ListTrustPairings(ctx, &pb.ListTrustPairingsRequest{Ids: []string{pair.ID}})
	if err != nil {
		return err
	}
	var found *pb.TrustPairing
	for _, p := range resp.Pairings {
		if p.Id != pair.ID {
			continue
		}
		if found != nil && !bytes.Equal(found.NodeKey, p.NodeKey) {
			return errors.New("two node keys for one pairing: the code may have leaked; endorsing neither")
		}
		found = p
	}
	if found == nil {
		return errors.New("no device pairing found for this secret")
	}
	p := found
	if p.Kind != pki.KindDevice || p.User != user || p.NodeId == "" || p.NodeName == "" ||
		p.ExpiresAt == nil || !time.Now().Before(p.ExpiresAt.AsTime()) {
		return errors.New("device pairing has a different user, invalid identity or expired code")
	}
	if err := pair.CheckNodeProof(p.NodeProof, pki.KindDevice, p.NodeKey); err != nil {
		return err
	}
	ok, err := confirm(p)
	if err != nil || !ok {
		return err
	}
	e, err := pki.EndorseID(key, user, pki.KindDevice, p.NodeName, p.NodeId, p.NodeKey)
	if err != nil {
		return err
	}
	pub := key.Public().(ed25519.PublicKey)
	_, err = fleet.CompleteTrustPairing(ctx, &pb.CompleteTrustPairingRequest{
		Id: pair.ID, Endorsement: e.String(), UserKey: pub,
		UserTag: pair.UserTag(pki.KindDevice, user, pub, p.ExpiresAt.AsTime()),
	})
	return err
}

func prepareWorkerPairing(dir string, ts trust.Store, name string, ttl time.Duration) (*pb.TrustPairing, string, error) {
	key, user, err := ownUserKey(ts)
	if err != nil {
		return nil, "", err
	}
	secret := pki.NewPairSecret()
	pair, err := pki.DerivePairing(secret)
	if err != nil {
		return nil, "", err
	}
	expires := time.Now().Add(ttl).Truncate(time.Second)
	if err := trust.WritePairing(dir, trust.Pairing{Secret: secret, Kind: pki.KindWorker, Name: name, ExpiresAt: expires}); err != nil {
		return nil, "", err
	}
	pub := key.Public().(ed25519.PublicKey)
	return &pb.TrustPairing{Id: pair.ID, Kind: pki.KindWorker, User: user, UserKey: pub,
		UserTag: pair.UserTag(pki.KindWorker, user, pub, expires), ExpiresAt: timestamppb.New(expires)}, secret, nil
}

func pollWorkerPairings(ctx context.Context, dir string, fleet pb.FleetClient, pending int32, note func(string)) error {
	files, err := trust.ListPairings(dir)
	if err != nil {
		return err
	}
	if len(files) == 0 && pending == 0 {
		return nil
	}
	ids := make([]string, 0, len(files))
	for id := range files {
		ids = append(ids, id)
	}
	resp, err := fleet.ListTrustPairings(ctx, &pb.ListTrustPairingsRequest{Ids: ids})
	if err != nil {
		return err
	}
	ts := client.TrustStore(dir)
	key, user, keyErr := ts.UserKey()
	groups := map[string][]*pb.TrustPairing{}
	for _, p := range resp.Pairings {
		groups[p.Id] = append(groups[p.Id], p)
	}
	for id, local := range files {
		if time.Now().After(local.ExpiresAt.Add(30 * 24 * time.Hour)) {
			if err := trust.DeletePairing(dir, id); err != nil {
				return err
			}
			continue
		}
		if local.Kind != pki.KindWorker || len(groups[id]) == 0 {
			continue
		}
		ps := groups[id]
		p := ps[0]
		err := func() error {
			for _, other := range ps[1:] {
				if !bytes.Equal(p.NodeKey, other.NodeKey) {
					return errors.New("two node keys for one pairing: the code may have leaked; endorsing neither")
				}
			}
			if len(p.NodeKey) == 0 {
				return nil // the worker has not joined yet
			}
			if keyErr != nil {
				return keyErr
			}
			if p.Kind != pki.KindWorker || p.User != user || p.NodeId == "" || p.NodeName == "" ||
				(local.Name != "" && local.Name != p.NodeName) || !time.Now().Before(local.ExpiresAt) {
				return errors.New("worker pairing has a different user/name, invalid identity or expired code")
			}
			pair, err := pki.DerivePairing(local.Secret)
			if err != nil {
				return err
			}
			if err := pair.CheckNodeProof(p.NodeProof, pki.KindWorker, p.NodeKey); err != nil {
				return err
			}
			e, err := pki.EndorseID(key, user, pki.KindWorker, p.NodeName, p.NodeId, p.NodeKey)
			if err != nil {
				return err
			}
			if _, err := fleet.CompleteTrustPairing(ctx, &pb.CompleteTrustPairingRequest{Id: id, Endorsement: e.String()}); err != nil {
				return err
			}
			if err := trust.DeletePairing(dir, id); err != nil {
				return err
			}
			note(fmt.Sprintf("endorsed your worker %s (%s, %s)", p.NodeName, p.Hostname, p.Os))
			return nil
		}()
		if err != nil {
			pairingWarning(dir, os.Stderr, err)
		}
	}
	return nil
}
