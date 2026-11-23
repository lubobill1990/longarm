package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"longarm/internal/client"
	"longarm/internal/pki"
	"longarm/internal/trust"
	pb "longarm/proto/longarm/v1"
)

type pairingFleet struct {
	pb.FleetClient
	pairings []*pb.TrustPairing
	complete []*pb.CompleteTrustPairingRequest
}

func (f *pairingFleet) ListTrustPairings(context.Context, *pb.ListTrustPairingsRequest, ...grpc.CallOption) (*pb.ListTrustPairingsResponse, error) {
	return &pb.ListTrustPairingsResponse{Pairings: f.pairings}, nil
}

func (f *pairingFleet) CompleteTrustPairing(_ context.Context, r *pb.CompleteTrustPairingRequest, _ ...grpc.CallOption) (*pb.TrustPairing, error) {
	f.complete = append(f.complete, r)
	return &pb.TrustPairing{Id: r.Id}, nil
}

func TestApproveDevice(t *testing.T) {
	for _, test := range []string{"valid", "bad proof", "other user", "expired", "conflict", "cancel"} {
		t.Run(test, func(t *testing.T) {
			ts := trust.Store{Dir: filepath.Join(t.TempDir(), "trust")}
			if _, err := ts.CreateUserKey("alice"); err != nil {
				t.Fatal(err)
			}
			secret := pki.NewPairSecret()
			pair, err := pki.DerivePairing(secret)
			if err != nil {
				t.Fatal(err)
			}
			node, _, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			p := &pb.TrustPairing{Id: pair.ID, Kind: pki.KindDevice, User: "alice", NodeId: "11111111-1111-4111-8111-111111111111", NodeName: "device-1",
				NodeKey: node, NodeProof: pair.NodeProof(pki.KindDevice, node), ExpiresAt: timestamppb.New(time.Now().Add(time.Hour))}
			f := &pairingFleet{pairings: []*pb.TrustPairing{p}}
			switch test {
			case "bad proof":
				p.NodeProof[0] ^= 1
			case "other user":
				p.User = "bob"
			case "expired":
				p.ExpiresAt = timestamppb.New(time.Now().Add(-time.Hour))
			case "conflict":
				f.pairings = append(f.pairings, &pb.TrustPairing{Id: pair.ID, NodeKey: make([]byte, 32)})
			}
			asked := false
			err = approveDevice(context.Background(), f, ts, secret, func(*pb.TrustPairing) (bool, error) {
				asked = true
				return test != "cancel", nil
			})
			if test != "valid" && test != "cancel" {
				if err == nil || asked || len(f.complete) != 0 {
					t.Fatalf("invalid pairing: err=%v asked=%v completed=%d", err, asked, len(f.complete))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test == "cancel" {
				if len(f.complete) != 0 {
					t.Fatal("endorsed cancelled device")
				}
				return
			}
			if len(f.complete) != 1 {
				t.Fatal("device not endorsed")
			}
			r := f.complete[0]
			e, err := pki.ParseEndorsement(r.Endorsement)
			if err != nil {
				t.Fatal(err)
			}
			if e.User != "alice" || e.Kind != pki.KindDevice || e.ID != p.NodeId || !node.Equal(ed25519.PublicKey(e.NodeKey)) {
				t.Fatal("incorrect endorsement")
			}
			if err := pair.CheckUserTag(r.UserTag, pki.KindDevice, "alice", r.UserKey, p.ExpiresAt.AsTime()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWorkerPairing(t *testing.T) {
	for _, test := range []string{"valid", "bad proof", "wrong name", "expired", "conflict"} {
		t.Run(test, func(t *testing.T) {
			dir := t.TempDir()
			ts := client.TrustStore(dir)
			if _, err := ts.CreateUserKey("alice"); err != nil {
				t.Fatal(err)
			}
			p, secret, err := prepareWorkerPairing(dir, ts, "worker-1", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			pair, err := pki.DerivePairing(secret)
			if err != nil {
				t.Fatal(err)
			}
			node, _, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			p.NodeKey, p.NodeProof = node, pair.NodeProof(pki.KindWorker, node)
			p.NodeName, p.NodeId = "worker-1", "11111111-1111-4111-8111-111111111111"
			f := &pairingFleet{pairings: []*pb.TrustPairing{p}}
			switch test {
			case "bad proof":
				p.NodeProof[0] ^= 1
			case "wrong name":
				p.NodeName = "worker-2"
			case "expired":
				local, err := trust.ReadPairing(dir, p.Id)
				if err != nil {
					t.Fatal(err)
				}
				local.ExpiresAt = time.Now().Add(-time.Hour)
				if err := trust.WritePairing(dir, local); err != nil {
					t.Fatal(err)
				}
			case "conflict":
				f.pairings = append(f.pairings, &pb.TrustPairing{Id: p.Id, NodeKey: make([]byte, 32)})
			}
			var notes []string
			if err := pollWorkerPairings(context.Background(), dir, f, 1, func(s string) { notes = append(notes, s) }); err != nil {
				t.Fatal(err)
			}
			files, err := trust.ListPairings(dir)
			if err != nil {
				t.Fatal(err)
			}
			if test == "valid" {
				if len(f.complete) != 1 || len(files) != 0 || len(notes) != 1 || !strings.Contains(notes[0], "endorsed your worker worker-1") {
					t.Fatalf("completion=%d files=%d notes=%v", len(f.complete), len(files), notes)
				}
			} else if len(f.complete) != 0 || len(files) != 1 {
				t.Fatalf("invalid pairing completed or removed: complete=%d files=%d", len(f.complete), len(files))
			}
		})
	}
}

func TestWorkerAddPairingHelp(t *testing.T) {
	secret := pki.NewPairSecret()
	help := workerAddHelp("hub.example.com:7443", "CODE", "fingerprint", "worker-1", "alice", "tomorrow", secret)
	if !strings.Contains(help, "--name worker-1 --pair "+secret) {
		t.Fatalf("missing paired join: %s", help)
	}
	if strings.Contains(workerAddHelp("hub.example.com", "CODE", "fp", "", "alice", "tomorrow", ""), "--pair") {
		t.Fatal("legacy fallback contains secret flag")
	}
}
