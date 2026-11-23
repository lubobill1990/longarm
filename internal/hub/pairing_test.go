package hub

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"longarm/internal/hub/store"
	"longarm/internal/hubclient"
	"longarm/internal/nodeid"
	"longarm/internal/pki"
	pb "longarm/proto/longarm/v1"
)

func pairingKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, *pki.Pairing) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := pki.DerivePairing(pki.NewPairSecret())
	if err != nil {
		t.Fatal(err)
	}
	return pub, key, pair
}

func workerPairing(t *testing.T) (*pb.TrustPairing, ed25519.PrivateKey, *pki.Pairing) {
	t.Helper()
	pub, key, pair := pairingKeys(t)
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	return &pb.TrustPairing{Id: pair.ID, Kind: pki.KindWorker, User: "alice", UserKey: pub,
		UserTag: pair.UserTag(pki.KindWorker, "alice", pub, expires), ExpiresAt: timestamppb.New(expires)}, key, pair
}

func pairingConn(t *testing.T, h *Hub, resp *pb.JoinResponse, key []byte) *grpc.ClientConn {
	t.Helper()
	b := writeTestBundle(t, h, &hubclient.JoinResult{Resp: resp, KeyPEM: key})
	conn, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func pairingWhoami(t *testing.T, f pb.FleetClient, count int32, key []byte) {
	t.Helper()
	got, err := f.Whoami(tctx(t), &pb.WhoamiRequest{})
	if err != nil || got.PendingPairings != count || !bytes.Equal(got.UserKey, key) {
		t.Fatalf("whoami: %v, %v; want %d pending and key %x", got, err, count, key)
	}
}

func TestTrustPairingWorker(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	alice, bob := pb.NewFleetClient(th.alice), pb.NewFleetClient(th.bob)
	p, userKey, pair := workerPairing(t)
	u, err := alice.PublishUserKey(tctx(t), &pb.PublishUserKeyRequest{UserKey: p.UserKey})
	if err != nil || !bytes.Equal(u.UserKey, p.UserKey) {
		t.Fatalf("publish: %v, %v", u, err)
	}
	pairingWhoami(t, alice, 0, p.UserKey)
	pairingWhoami(t, bob, 0, nil)
	code, err := alice.CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Name: "worker-1", Pairing: p})
	if err != nil || code.PairingId != p.Id {
		t.Fatalf("create code: %v, %v", code, err)
	}
	_, nodeKey, _ := pairingKeys(t)
	key, csr, err := pki.NewKeyAndCSR("worker-1", &pki.CSRTrust{NodeKey: nodeKey})
	if err != nil {
		t.Fatal(err)
	}
	req := &pb.JoinRequest{Code: code.Code + "#local-secret", CsrPem: csr, Hostname: "worker-1",
		Os: "linux", Arch: "amd64", Version: "1.0.0", PairingId: p.Id, NodeProof: pair.NodeProof(p.Kind, nodeKey.Public().(ed25519.PublicKey))}
	enroll := &enrollServer{h: h}
	if _, err := enroll.Join(tctx(t), req); status.Code(err) != codes.InvalidArgument ||
		status.Convert(err).Message() != "the part after # is a local secret: pass it as --pair" {
		t.Fatalf("code with local secret: %v", err)
	}
	if _, err := h.store.PeekJoinCode(codeHash(code.Code), time.Now()); err != nil {
		t.Fatalf("code was consumed: %v", err)
	}
	req.Code = code.Code
	resp, err := enroll.Join(tctx(t), req)
	if err != nil || resp.Pairing == nil || resp.State != stateApproved {
		t.Fatalf("join: %v, %v", resp, err)
	}
	if err := pair.CheckUserTag(resp.Pairing.UserTag, p.Kind, "alice", resp.Pairing.UserKey, resp.Pairing.ExpiresAt.AsTime()); err != nil {
		t.Fatal(err)
	}
	list, err := alice.ListTrustPairings(tctx(t), &pb.ListTrustPairingsRequest{})
	if err != nil || len(list.Pairings) != 1 {
		t.Fatalf("list: %v, %v", list, err)
	}
	joined := list.Pairings[0]
	if joined.NodeId != resp.Id || joined.NodeName != "worker-1" || joined.Hostname != req.Hostname || joined.Os != "linux" || joined.State != "joined" {
		t.Fatalf("joined pairing: %v", joined)
	}
	if err := pair.CheckNodeProof(joined.NodeProof, p.Kind, joined.NodeKey); err != nil {
		t.Fatal(err)
	}
	pairingWhoami(t, alice, 1, p.UserKey)
	for _, f := range []pb.FleetClient{bob, pb.NewFleetClient(th.root)} {
		got, err := f.ListTrustPairings(tctx(t), &pb.ListTrustPairingsRequest{Ids: []string{p.Id}})
		// A userless admin has no tenant; admins never get another user's pairings.
		if err == nil && len(got.Pairings) != 0 {
			t.Fatalf("another user sees pairing: %v", got)
		}
	}
	e, err := pki.EndorseID(userKey, "alice", p.Kind, joined.NodeName, joined.NodeId, joined.NodeKey)
	if err != nil {
		t.Fatal(err)
	}
	complete := &pb.CompleteTrustPairingRequest{Id: p.Id, Endorsement: e.String()}
	if _, err := bob.CompleteTrustPairing(tctx(t), complete); status.Code(err) != codes.NotFound {
		t.Fatalf("another user completes: %v", err)
	}
	for _, change := range []string{"key", "id", "kind", "user", "name", "user-key", "signature"} {
		t.Run(change, func(t *testing.T) {
			bad := *e
			switch change {
			case "key":
				bad.NodeKey, _, _ = pairingKeys(t)
			case "id":
				bad.ID = nodeid.New()
			case "kind":
				bad.Kind = pki.KindDevice
			case "user":
				bad.User = "bob"
			case "name":
				bad.Name = "worker-2"
			case "signature":
				bad.Sig = make([]byte, ed25519.SignatureSize)
			}
			if change != "signature" {
				// Re-sign altered fields so target checks, not just parsing, reject them.
				signKey := userKey
				if change == "user-key" {
					bad.UserKey, signKey, _ = pairingKeys(t)
				}
				signed, err := pki.EndorseID(signKey, bad.User, bad.Kind, bad.Name, bad.ID, bad.NodeKey)
				if err != nil {
					t.Fatal(err)
				}
				bad = *signed
			}
			if _, err := alice.CompleteTrustPairing(tctx(t), &pb.CompleteTrustPairingRequest{Id: p.Id, Endorsement: bad.String()}); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("mismatched endorsement: %v", err)
			}
		})
	}
	out := make(chan *pb.HubCommand, 128)
	h.mu.Lock()
	h.nodes[resp.Id].sess = &nodeSession{id: 99, out: out, cancel: func() {}}
	h.mu.Unlock()
	got, err := alice.CompleteTrustPairing(tctx(t), complete)
	if err != nil || got.State != "endorsed" {
		t.Fatalf("complete: %v, %v", got, err)
	}
	select {
	case cmd := <-out:
		if cmd.GetTrustEndorsement().GetEndorsement() != e.String() {
			t.Fatalf("push: %v", cmd)
		}
	default:
		t.Fatal("no immediate endorsement push")
	}
	pairingWhoami(t, alice, 0, p.UserKey)
	if retry, err := alice.CompleteTrustPairing(tctx(t), complete); err != nil || !proto.Equal(retry, got) {
		t.Fatalf("completion retry: %v, %v", retry, err)
	}
	rec, err := h.store.Node(resp.Id)
	if err != nil || rec.Pairing == nil || rec.Pairing.Endorsement != e.String() {
		t.Fatalf("persistent pairing: %v, %v", rec, err)
	}

	conn := pairingConn(t, h, resp, key)
	checkConnect := func(conn *grpc.ClientConn, wantPush bool) {
		t.Helper()
		ctx, cancel := context.WithCancel(tctx(t))
		defer cancel()
		st, err := pb.NewHubNodeClient(conn).Connect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Send(&pb.NodeReport{Msg: &pb.NodeReport_Hello{Hello: &pb.NodeHello{Version: "1.0.0"}}}); err != nil {
			t.Fatal(err)
		}
		pushed := false
		for {
			cmd, err := st.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if trust := cmd.GetTrustEndorsement(); trust != nil {
				pushed = trust.PairingId == p.Id && trust.Endorsement == e.String()
			}
			if cmd.GetPortLinks() != nil {
				break
			}
		}
		if pushed != wantPush {
			t.Fatalf("endorsement pushed = %v, want %v", pushed, wantPush)
		}
	}
	checkConnect(conn, true)
	checkConnect(conn, true)
	newKey, csr, err := pki.NewKeyAndCSR("worker-1", &pki.CSRTrust{NodeKey: nodeKey, Endorsement: e})
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := pb.NewHubNodeClient(conn).Renew(tctx(t), &pb.RenewRequest{CsrPem: csr})
	if err != nil {
		t.Fatal(err)
	}
	checkConnect(pairingConn(t, h, &pb.JoinResponse{CertPem: renewed.CertPem}, newKey), false)
	rec, err = h.store.Node(resp.Id)
	if err != nil || rec.Pairing.State != "installed" {
		t.Fatalf("installed pairing: %v, %v", rec, err)
	}
	h.sweepTrustPairings(time.Now().Add(48 * time.Hour))
	rec, _ = h.store.Node(resp.Id)
	if rec.Pairing == nil || rec.Pairing.State != "installed" {
		t.Fatal("sweep dropped installed pairing")
	}
	audit, err := h.Audit(&pb.AuditRequest{Node: "hub", Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	steps := map[string]bool{}
	for _, line := range audit.Records {
		steps[line.Method] = true
		for _, mac := range [][]byte{p.UserTag, req.NodeProof} {
			if strings.Contains(line.Json, base64.StdEncoding.EncodeToString(mac)) {
				t.Fatal("pairing MAC appeared in audit")
			}
		}
	}
	for _, step := range []string{"created", "joined", "publish-key", "list", "endorsed", "installed"} {
		if !steps["trust/pairing-"+step] {
			t.Errorf("missing audit step %s", step)
		}
	}
}

func TestTrustPairingDevice(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	pub, userKey, pair := pairingKeys(t)
	_, nodeKey, _ := pairingKeys(t)
	key, csr, err := pki.NewKeyAndCSR("alice-laptop", &pki.CSRTrust{NodeKey: nodeKey})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := th.ca.SignCSR(csr, pki.IssueOptions{Role: pki.RoleClient, Name: "alice-laptop", User: "alice", ID: nodeid.New()})
	if err != nil {
		t.Fatal(err)
	}
	device := pb.NewFleetClient(pairingConn(t, h, &pb.JoinResponse{CertPem: cert}, key))
	alice := pb.NewFleetClient(th.alice)
	offer := &pb.TrustPairing{Id: pair.ID, Kind: pki.KindDevice, NodeProof: pair.NodeProof(pki.KindDevice, nodeKey.Public().(ed25519.PublicKey)),
		User: "bob", NodeKey: make([]byte, 32), NodeId: nodeid.New(), NodeName: "another-device", State: "installed"}
	p, err := device.OfferDevicePairing(tctx(t), offer)
	if err != nil || p.User != "alice" || p.NodeName != "alice-laptop" || p.NodeId == offer.NodeId ||
		!bytes.Equal(p.NodeKey, nodeKey.Public().(ed25519.PublicKey)) || p.State != "offered" ||
		time.Until(p.ExpiresAt.AsTime()) < 59*time.Minute || time.Until(p.ExpiresAt.AsTime()) > time.Hour {
		t.Fatalf("offer: %v, %v", p, err)
	}
	if err := pair.CheckNodeProof(p.NodeProof, p.Kind, p.NodeKey); err != nil {
		t.Fatal(err)
	}
	pairingWhoami(t, alice, 1, nil)
	e, err := pki.EndorseID(userKey, "alice", pki.KindDevice, p.NodeName, p.NodeId, p.NodeKey)
	if err != nil {
		t.Fatal(err)
	}
	req := &pb.CompleteTrustPairingRequest{Id: pair.ID, Endorsement: e.String(), UserKey: pub,
		UserTag: pair.UserTag(p.Kind, "alice", pub, p.ExpiresAt.AsTime())}
	for _, field := range []string{"key", "tag"} {
		bad := proto.Clone(req).(*pb.CompleteTrustPairingRequest)
		if field == "key" {
			bad.UserKey = make([]byte, 32)
		} else {
			bad.UserTag = nil
		}
		if _, err := alice.CompleteTrustPairing(tctx(t), bad); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("bad device %s: %v", field, err)
		}
	}
	if _, err := alice.CompleteTrustPairing(tctx(t), req); err != nil {
		t.Fatal(err)
	}
	again, err := device.OfferDevicePairing(tctx(t), offer)
	if err != nil || again.State != "endorsed" {
		t.Fatalf("offer retry: %v, %v", again, err)
	}
	h.sweepTrustPairings(time.Now().Add(2 * time.Hour))
	list, err := device.ListTrustPairings(tctx(t), &pb.ListTrustPairingsRequest{Ids: []string{pair.ID}})
	if err != nil || len(list.Pairings) != 1 || list.Pairings[0].State != "endorsed" {
		t.Fatalf("fetch device endorsement: %v, %v", list, err)
	}
	got := list.Pairings[0]
	if err := pair.CheckUserTag(got.UserTag, p.Kind, "alice", got.UserKey, got.ExpiresAt.AsTime()); err != nil {
		t.Fatal(err)
	}
	_, _, replacement := pairingKeys(t)
	offer.Id = replacement.ID
	offer.NodeProof = replacement.NodeProof(p.Kind, p.NodeKey)
	if _, err := device.OfferDevicePairing(tctx(t), offer); err != nil {
		t.Fatal(err)
	}
	list, err = alice.ListTrustPairings(tctx(t), &pb.ListTrustPairingsRequest{})
	if err != nil || len(list.Pairings) != 1 || list.Pairings[0].Id != replacement.ID {
		t.Fatalf("replace offer: %v, %v", list, err)
	}
	h.mu.Lock()
	n := h.nodes[p.NodeId]
	n.rec.Pairing.ExpiresAt = time.Now().Add(-time.Minute)
	h.mu.Unlock()
	pairingWhoami(t, alice, 0, nil)
	if _, err := alice.CompleteTrustPairing(tctx(t), &pb.CompleteTrustPairingRequest{
		Id: replacement.ID, Endorsement: e.String(), UserKey: pub, UserTag: make([]byte, 32),
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("complete expired offer: %v", err)
	}
	h.sweepTrustPairings(time.Now().Add(2 * time.Hour))
	list, err = alice.ListTrustPairings(tctx(t), &pb.ListTrustPairingsRequest{Ids: []string{replacement.ID}})
	if err != nil || len(list.Pairings) != 0 {
		t.Fatalf("expired offer: %v, %v", list, err)
	}
	if _, err := alice.OfferDevicePairing(tctx(t), offer); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("device without trust extension: %v", err)
	}
}

func TestTrustPairingValidation(t *testing.T) {
	th := newTenancyHub(t)
	h := th.h
	h.trustPairLimit = newIPLimiter(100, 100)
	alice := pb.NewFleetClient(th.alice)
	p, _, _ := workerPairing(t)
	for _, field := range []string{"id", "kind", "user", "key", "tag", "expiry-missing", "expiry-invalid", "expiry-past", "expiry-late"} {
		t.Run(field, func(t *testing.T) {
			bad := proto.Clone(p).(*pb.TrustPairing)
			switch field {
			case "id":
				bad.Id = "not-a-pairing"
			case "kind":
				bad.Kind = pki.KindDevice
			case "user":
				bad.User = "bob"
			case "key":
				bad.UserKey = nil
			case "tag":
				bad.UserTag = make([]byte, 31)
			case "expiry-missing":
				bad.ExpiresAt = nil
			case "expiry-invalid":
				bad.ExpiresAt = &timestamppb.Timestamp{Nanos: -1}
			case "expiry-past":
				bad.ExpiresAt = timestamppb.New(time.Now().Add(-time.Minute))
			case "expiry-late":
				bad.ExpiresAt = timestamppb.New(time.Now().Add(2 * time.Hour))
			}
			if _, err := alice.CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Pairing: bad, Ttl: durationpb.New(time.Hour)}); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("bad worker pairing: %v", err)
			}
		})
	}
	for _, key := range [][]byte{nil, make([]byte, 31), make([]byte, 33)} {
		if _, err := alice.PublishUserKey(tctx(t), &pb.PublishUserKeyRequest{UserKey: key}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("bad published key: %v", err)
		}
	}
	if _, err := alice.ListTrustPairings(tctx(t), &pb.ListTrustPairingsRequest{Ids: []string{"bad"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad list id: %v", err)
	}
	for _, offer := range []*pb.TrustPairing{{Id: "bad"}, {Id: p.Id, Kind: pki.KindWorker, NodeProof: make([]byte, 32)},
		{Id: p.Id, Kind: pki.KindDevice, NodeProof: make([]byte, 31)}} {
		if _, err := alice.OfferDevicePairing(tctx(t), offer); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("bad device offer: %v", err)
		}
	}
	if _, err := alice.CompleteTrustPairing(tctx(t), &pb.CompleteTrustPairingRequest{Id: p.Id, Endorsement: "bad"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad endorsement: %v", err)
	}
	if _, err := (&fleetServer{h: h}).PublishUserKey(context.WithValue(tctx(t), callerKey{}, caller{
		id: pki.Identity{Role: pki.RoleWorker, Name: "worker-1"}, user: "alice"}), &pb.PublishUserKeyRequest{UserKey: p.UserKey}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("worker publishes key: %v", err)
	}

	h.mu.Lock()
	for i := range maxTrustPairings {
		id := nodeid.New()
		n := &node{rec: &store.Node{ID: id, Name: fmt.Sprintf("device-%d", i), Role: "client", User: "alice", State: stateApproved,
			Pairing: &store.TrustPairing{ID: fmt.Sprintf("%024x", i), User: "alice", Kind: pki.KindDevice,
				NodeID: id, State: "offered", ExpiresAt: time.Now().Add(time.Hour)}}}
		if err := h.store.PutNode(n.rec); err != nil {
			t.Fatal(err)
		}
		h.nodes[id] = n
	}
	h.mu.Unlock()
	if _, err := alice.CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Pairing: p}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("pairing limit: %v", err)
	}
	h.sweepTrustPairings(time.Now().Add(2 * time.Hour))
	if _, err := alice.CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Pairing: p}); err != nil {
		t.Fatalf("slots not freed by expiry: %v", err)
	}
	if _, err := alice.CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Pairing: p}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate pairing id: %v", err)
	}
	h.trustPairLimit = newIPLimiter(0.001, 1)
	if _, err := alice.ListTrustPairings(tctx(t), &pb.ListTrustPairingsRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.ListTrustPairings(tctx(t), &pb.ListTrustPairingsRequest{}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("pairing rate limit: %v", err)
	}
}

func TestTrustPairingLegacyJoin(t *testing.T) {
	th := newTenancyHub(t)
	th.h.joinLimit = newIPLimiter(100, 100)
	for i, field := range []string{"missing-id", "wrong-id", "missing-proof", "odd-proof", "no-trust"} {
		t.Run(field, func(t *testing.T) {
			p, _, pair := workerPairing(t)
			code, err := pb.NewFleetClient(th.alice).CreateWorkerCode(tctx(t), &pb.CreateWorkerCodeRequest{Name: fmt.Sprintf("worker-%d", i), Pairing: p})
			if err != nil {
				t.Fatal(err)
			}
			_, nk, _ := pairingKeys(t)
			trust := &pki.CSRTrust{NodeKey: nk}
			if field == "no-trust" {
				trust = nil
			}
			_, csr, err := pki.NewKeyAndCSR("worker-1", trust)
			if err != nil {
				t.Fatal(err)
			}
			req := &pb.JoinRequest{Code: code.Code, CsrPem: csr, PairingId: p.Id,
				NodeProof: pair.NodeProof(p.Kind, nk.Public().(ed25519.PublicKey)), Hostname: "worker-1", Os: "linux", Arch: "amd64"}
			switch field {
			case "missing-id":
				req.PairingId = ""
			case "wrong-id":
				req.PairingId = strings.Repeat("0", 24)
			case "missing-proof":
				req.NodeProof = nil
			case "odd-proof":
				req.NodeProof = make([]byte, 31)
			}
			resp, err := (&enrollServer{h: th.h}).Join(tctx(t), req)
			if err != nil || resp.State != stateApproved || resp.Pairing != nil {
				t.Fatalf("legacy join: %v, %v", resp, err)
			}
			rec, err := th.h.store.Node(resp.Id)
			if err != nil || rec.Pairing != nil {
				t.Fatalf("unexpected stored pairing: %v, %v", rec, err)
			}
		})
	}
}

func TestTrustPairingInstalledIdentity(t *testing.T) {
	th := newTenancyHub(t)
	pub, userKey, _ := pairingKeys(t)
	nodePub, nodeKey, _ := pairingKeys(t)
	id := nodeid.New()
	for _, field := range []string{"node-key", "node-id", "kind", "user", "user-key", "same-identity"} {
		t.Run(field, func(t *testing.T) {
			kind, user, targetID, targetKey, signingKey := pki.KindWorker, "alice", id, nodePub, userKey
			csrKey := nodeKey
			switch field {
			case "node-key":
				targetKey, csrKey, _ = pairingKeys(t)
			case "node-id":
				targetID = nodeid.New()
			case "kind":
				kind = pki.KindDevice
			case "user":
				user = "bob"
			case "user-key":
				_, signingKey, _ = pairingKeys(t)
			}
			e, err := pki.EndorseID(signingKey, user, kind, "worker-1-renamed", targetID, targetKey)
			if err != nil {
				t.Fatal(err)
			}
			_, csr, err := pki.NewKeyAndCSR("worker-1", &pki.CSRTrust{NodeKey: csrKey, Endorsement: e})
			if err != nil {
				t.Fatal(err)
			}
			certPEM, err := th.ca.SignCSR(csr, pki.IssueOptions{Role: pki.RoleWorker, Name: "worker-1", ID: id})
			if err != nil {
				t.Fatal(err)
			}
			cert, err := pki.ParseCertPEM(certPEM)
			if err != nil {
				t.Fatal(err)
			}
			n := &node{rec: &store.Node{ID: id, Name: "worker-1", Role: "worker", State: stateApproved,
				Pairing: &store.TrustPairing{ID: strings.Repeat("a", 24), Kind: pki.KindWorker, User: "alice",
					UserKey: pub, NodeKey: nodePub, NodeID: id, NodeName: "worker-1", State: "endorsed",
					Endorsement: "an earlier endorsement of this identity"}}}
			// A certificate with another valid endorsement of the same identity
			// is enough; byte-for-byte equality to the submitted line is not needed.
			th.h.mu.Lock()
			cmd := th.h.pairingConnectLocked(n, cert)
			th.h.mu.Unlock()
			installed := n.rec.Pairing.State == "installed"
			if installed != (field == "same-identity") || (cmd == nil) != installed {
				t.Fatalf("installed=%v, pushed=%v", installed, cmd != nil)
			}
		})
	}
}
