package hubclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"longarm/internal/pki"
	pb "longarm/proto/longarm/v1"
)

func TestSplitPairCode(t *testing.T) {
	const fixture = "01234-56789-ABCDE-FGHJK"
	for _, tt := range []struct {
		name, code, secret string
		wantErr            bool
	}{
		{"plain", "ABC-DEF", "", false},
		{"flag", "ABC-DEF", fixture, false},
		{"combined", "ABC-DEF#" + fixture, "", false},
		{"same", "ABC-DEF#" + fixture, strings.ToLower(fixture), false},
		{"different", "ABC-DEF#" + fixture, "ZZZZZ-ZZZZZ-ZZZZZ-ZZZZZ", true},
		{"empty secret", "ABC-DEF#", "", true},
		{"empty code", "#" + fixture, "", true},
		{"extra separator", "ABC-DEF#" + fixture + "#", "", true},
		{"invalid flag", "ABC-DEF", "invalid", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code, s, err := SplitPairCode(tt.code, tt.secret)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v", err)
			}
			if tt.wantErr {
				if code != "" || s != "" {
					t.Fatal("invalid secret returned to caller")
				}
				return
			}
			if code != "ABC-DEF" {
				t.Fatalf("code = %q", code)
			}
			if tt.name == "plain" {
				if s != "" {
					t.Fatal("unpaired join acquired a secret")
				}
			} else if s != strings.ReplaceAll(fixture, "-", "") {
				t.Fatalf("secret was not normalized")
			}
		})
	}
}

type pairingEnrollServer struct {
	pb.UnimplementedEnrollServer
	ca       *pki.CA
	requests chan *pb.JoinRequest
}

func (s *pairingEnrollServer) Join(_ context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	s.requests <- req
	return &pb.JoinResponse{State: "pending", Role: "worker", Name: "worker-1", CaPem: s.ca.CertPEM}, nil
}

func TestJoinPairingProofAndSecretIsolation(t *testing.T) {
	ca := testCA(t)
	certPEM, keyPEM, err := ca.Issue(pki.IssueOptions{Role: pki.RoleHub, Name: "hub", Hosts: []string{"127.0.0.1"}, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(append(certPEM, ca.CertPEM...), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})))
	enroll := &pairingEnrollServer{ca: ca, requests: make(chan *pb.JoinRequest, 3)}
	pb.RegisterEnrollServer(s, enroll)
	go s.Serve(ln)
	t.Cleanup(s.Stop)
	_, nodeKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := pki.NewPairSecret()
	pairing, err := pki.DerivePairing(secret)
	if err != nil {
		t.Fatal(err)
	}
	for _, combined := range []bool{false, true} {
		code, flag := "ABC-DEF", secret
		if combined {
			code, flag = code+"#"+secret, ""
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := Join(ctx, ln.Addr().String(), code, JoinOptions{
			Name: "worker-1", Fingerprint: pki.Fingerprint(ca.Cert), PairSecret: flag, Trust: &pki.CSRTrust{NodeKey: nodeKey},
		})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		req := <-enroll.requests
		if req.Code != "ABC-DEF" || req.PairingId != pairing.ID || strings.Contains(req.String(), secret) {
			t.Fatalf("join did not isolate the secret: code=%q pairing=%q", req.Code, req.PairingId)
		}
		csr, err := pki.ParseCSR(req.CsrPem)
		if err != nil {
			t.Fatal(err)
		}
		tr, err := pki.TrustOfCSR(csr)
		if err != nil || tr == nil {
			t.Fatalf("CSR trust = %v, %v", tr, err)
		}
		if err := pairing.CheckNodeProof(req.NodeProof, pki.KindWorker, tr.NodeKey); err != nil {
			t.Fatal("proof does not bind the CSR node key:", err)
		}
	}
	if _, err := Join(context.Background(), ln.Addr().String(), "ABC-DEF", JoinOptions{PairSecret: secret}); err == nil {
		t.Fatal("pairing without a CSR node key accepted")
	}
	select {
	case <-enroll.requests:
		t.Fatal("invalid pairing reached the hub")
	default:
	}
}
