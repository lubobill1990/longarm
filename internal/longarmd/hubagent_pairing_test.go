package longarmd

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"longarm/internal/nodeid"
	"longarm/internal/pki"
	"longarm/internal/server"
	"longarm/internal/trust"
	pb "longarm/proto/longarm/v1"
)

func TestTrustEndorsementRequiresPinnedUserAndCertificateMatch(t *testing.T) {
	ca, err := pki.InitCA(filepath.Join(t.TempDir(), "ca"), "test hub CA", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"valid", "no pin", "user mismatch", "user key mismatch", "name mismatch", "id mismatch", "node key mismatch", "kind mismatch", "signature mismatch", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			store := trust.Store{Dir: filepath.Join(dir, TrustDir)}
			nodeKey, err := store.NodeKey()
			if err != nil {
				t.Fatal(err)
			}
			_, userKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			id := nodeid.New()
			certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{Role: pki.RoleWorker, Name: "worker-1", ID: id})
			if err != nil {
				t.Fatal(err)
			}
			live, err := pki.ParseBundle(ca.CertPEM, certPEM, keyPEM)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "no pin" {
				if err := trust.WritePinnedUser(dir, pki.UserKeyLine{User: "alice", Key: userKey.Public().(ed25519.PublicKey)}); err != nil {
					t.Fatal(err)
				}
			}
			user, name, kind := "alice", "worker-1", pki.KindWorker
			pub := nodeKey.Public().(ed25519.PublicKey)
			switch scenario {
			case "user mismatch":
				user = "bob"
			case "user key mismatch":
				_, userKey, err = ed25519.GenerateKey(rand.Reader)
			case "name mismatch":
				name = "worker-2"
			case "id mismatch":
				id = nodeid.New()
			case "node key mismatch":
				pub, _, err = ed25519.GenerateKey(rand.Reader)
			case "kind mismatch":
				kind = pki.KindDevice
			}
			if err != nil {
				t.Fatal(err)
			}
			e, err := pki.EndorseID(userKey, user, kind, name, id, pub)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "signature mismatch" {
				e.Sig[0] ^= 1
			}
			line := e.String()
			if scenario == "malformed" {
				line = "not an endorsement"
			}
			var logs bytes.Buffer
			a := &hubAgent{dataDir: dir, live: live, trustStore: store,
				log: slog.New(slog.NewTextHandler(&logs, nil)), audit: server.NewAudit(filepath.Join(dir, AuditFile))}
			a.handle(&pb.HubCommand{Msg: &pb.HubCommand_TrustEndorsement{TrustEndorsement: &pb.TrustEndorsement{PairingId: "pairing-id", Endorsement: line}}})
			got, err := store.Endorsement()
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "valid" {
				if got == nil || got.String() != line || !store.RenewalRequested() {
					t.Fatal("verified endorsement not installed and queued for renewal")
				}
				if logs.Len() != 0 {
					t.Fatalf("unexpected warning: %s", logs.String())
				}
			} else {
				if got != nil || store.RenewalRequested() {
					t.Fatal("unverified endorsement installed or renewal requested")
				}
				audit, err := os.ReadFile(filepath.Join(dir, AuditFile))
				if err != nil || !strings.Contains(string(audit), `"method":"trust/pair-failed"`) || !strings.Contains(logs.String(), "continuing without it") {
					t.Fatalf("missing warning/audit: %s / %s (%v)", logs.String(), audit, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, TrustDir, trust.PolicyFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("endorsement installation enabled trust policy enforcement")
			}
		})
	}
}
