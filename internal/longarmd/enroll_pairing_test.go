package longarmd

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"longarm/internal/pki"
	"longarm/internal/trust"
	pb "longarm/proto/longarm/v1"
)

func TestJoinPairingPinsOnlyAuthenticatedUser(t *testing.T) {
	for _, scenario := range []string{"valid", "tag mismatch", "key mismatch", "owner mismatch", "wrong kind", "wrong id", "expired", "missing expiry", "existing pin", "old hub", "old hub without secret"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			pub, _, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			secret := pki.NewPairSecret()
			pairing, err := pki.DerivePairing(secret)
			if err != nil {
				t.Fatal(err)
			}
			expiry := time.Now().Add(time.Hour)
			if scenario == "expired" {
				expiry = time.Now().Add(-time.Hour)
			}
			p := &pb.TrustPairing{Id: pairing.ID, Kind: pki.KindWorker, User: "alice", UserKey: pub, ExpiresAt: timestamppb.New(expiry),
				UserTag: pairing.UserTag(pki.KindWorker, "alice", pub, expiry)}
			resp := &pb.JoinResponse{Owner: "alice", Pairing: p}
			switch scenario {
			case "tag mismatch":
				p.UserTag[0] ^= 1
			case "key mismatch":
				p.UserKey = append([]byte(nil), pub...)
				p.UserKey[0] ^= 1
			case "owner mismatch":
				resp.Owner = "bob"
			case "wrong kind":
				p.Kind = pki.KindDevice
			case "wrong id":
				p.Id = strings.Repeat("0", 24)
			case "missing expiry":
				p.ExpiresAt = nil
			case "existing pin":
				if err := trust.WritePinnedUser(dir, pki.UserKeyLine{User: "bob", Key: pub}); err != nil {
					t.Fatal(err)
				}
			case "old hub":
				resp.Pairing = nil
			case "old hub without secret":
				resp.Pairing, secret = nil, ""
			}
			var out bytes.Buffer
			acceptJoinPairing(&out, dir, secret, resp)
			got, err := trust.ReadPinnedUser(dir)
			if scenario == "valid" {
				if err != nil || got.User != "alice" || !ed25519.PublicKey(got.Key).Equal(pub) {
					t.Fatalf("pinned user = %+v, %v", got, err)
				}
				if out.Len() != 0 {
					t.Fatalf("unexpected warning: %s", out.String())
				}
			} else if scenario == "existing pin" {
				if err != nil || got.User != "bob" {
					t.Fatalf("existing pin replaced: %+v, %v", got, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unverified user pinned: %+v, %v", got, err)
			}
			audit, auditErr := os.ReadFile(filepath.Join(dir, AuditFile))
			switch scenario {
			case "valid":
				if !errors.Is(auditErr, os.ErrNotExist) {
					t.Fatalf("successful pairing audited as a failure: %s, %v", audit, auditErr)
				}
			case "old hub":
				if !strings.Contains(out.String(), "doesn't support automatic trust yet") {
					t.Fatalf("missing compatibility warning: %s", out.String())
				}
			case "old hub without secret": // a plain join: nothing to say
				if out.Len() != 0 {
					t.Fatalf("warning on a join without --pair: %s", out.String())
				}
			default:
				if !strings.Contains(out.String(), "warning:") || !strings.Contains(string(audit), `"method":"trust/pair-failed"`) {
					t.Fatalf("missing warning/audit: %s / %s (%v)", out.String(), audit, auditErr)
				}
			}
			if secret != "" && (strings.Contains(out.String(), secret) || strings.Contains(string(audit), secret)) {
				t.Fatal("pairing secret leaked to diagnostics")
			}
			if _, err := os.Stat(filepath.Join(dir, TrustDir, trust.PolicyFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("automatic pairing enabled trust policy enforcement")
			}
		})
	}
}
