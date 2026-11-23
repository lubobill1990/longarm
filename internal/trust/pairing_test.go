package trust

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"longarm/internal/nodeid"
	"longarm/internal/pki"
	pb "longarm/proto/longarm/v1"
)

func pairingTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".trust-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestPairingFiles(t *testing.T) {
	dir := pairingTestDir(t)
	p := Pairing{Secret: pki.NewPairSecret(), Kind: pki.KindWorker, Name: "worker-1", ExpiresAt: time.Now().Add(time.Hour).UTC()}
	pair, _ := pki.DerivePairing(p.Secret)
	if err := WritePairing(dir, p); err != nil {
		t.Fatal(err)
	}
	p.LastOfferedAt = time.Now().UTC()
	if err := WritePairing(dir, p); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPairing(dir, pair.ID)
	if err != nil || got != p {
		t.Fatalf("read: %+v %v", got, err)
	}
	path, _ := pairingPath(dir, pair.ID)
	fi, _ := os.Stat(path)
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("pairing permissions: %v", fi.Mode())
	}
	for _, id := range []string{"..", "..\\secret", "../secret", strings.Repeat("a", 24) + "\\x", strings.Repeat("A", 24)} {
		if _, err := ReadPairing(dir, id); err == nil {
			t.Errorf("read accepted id %q", id)
		}
		if err := DeletePairing(dir, id); err == nil {
			t.Errorf("delete accepted id %q", id)
		}
	}
	other, _ := pki.DerivePairing(pki.NewPairSecret())
	otherPath, _ := pairingPath(dir, other.ID)
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(otherPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPairing(dir, other.ID); err == nil {
		t.Fatal("accepted file stored under another id")
	}
	os.Remove(otherPath)
	p.ExpiresAt = time.Now().Add(-29 * 24 * time.Hour)
	WritePairing(dir, p)
	if ps, err := ListPairings(dir); err != nil || len(ps) != 1 {
		t.Fatalf("expired offer lost too early: %v %v", ps, err)
	}
	p.ExpiresAt = time.Now().Add(-31 * 24 * time.Hour)
	WritePairing(dir, p)
	if ps, err := ListPairings(dir); err != nil || len(ps) != 0 {
		t.Fatalf("old expired offer kept: %v %v", ps, err)
	}
	if err := DeletePairing(dir, pair.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedUserDoesNotEnforceOrReplace(t *testing.T) {
	dir := pairingTestDir(t)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32)).Public().(ed25519.PublicKey)
	line := pki.UserKeyLine{User: "alice", Key: key}
	if err := WritePinnedUser(dir, line); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPinnedUser(dir)
	if err != nil || got.User != line.User || !bytes.Equal(got.Key, key) {
		t.Fatalf("pin: %+v %v", got, err)
	}
	if err := WritePinnedUser(dir, line); err != nil {
		t.Fatal(err)
	}
	line.User = "bob"
	if err := WritePinnedUser(dir, line); err == nil {
		t.Fatal("replaced existing pin")
	}
	s := Store{Dir: filepath.Join(dir, "trust")}
	policy, err := s.Policy(false)
	if err != nil || policy.Active() {
		t.Fatalf("pin enabled enforcement: %+v %v", policy, err)
	}
}

func TestRecoveryRoundTripAndImport(t *testing.T) {
	seed := bytes.Repeat([]byte{11}, 32)
	code, err := EncodeRecovery(seed)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRecovery(strings.ToLower(code))
	if err != nil || !bytes.Equal(got, seed) {
		t.Fatalf("decode: %x %v", got, err)
	}
	for _, invalid := range []string{"LAR2" + code[4:], "bad", code + "0", code[:6] + "Z" + code[7:]} {
		if _, err := DecodeRecovery(invalid); err == nil {
			t.Errorf("accepted corrupt code %q", invalid)
		}
	}
	if _, err := EncodeRecovery(seed[:31]); err == nil {
		t.Fatal("encoded short seed")
	}
	s := Store{Dir: filepath.Join(pairingTestDir(t), "trust")}
	if err := s.ImportRecovery(code, "alice"); err != nil {
		t.Fatal(err)
	}
	k, user, err := s.UserKey()
	if err != nil || user != "alice" || !bytes.Equal(k.Seed(), seed) {
		t.Fatalf("import: %s %v", user, err)
	}
	if err := s.ImportRecovery(code, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportRecovery(code, "bob"); err == nil {
		t.Fatal("changed key owner")
	}
	other, _ := EncodeRecovery(bytes.Repeat([]byte{12}, 32))
	if err := s.ImportRecovery(other, "alice"); err == nil {
		t.Fatal("replaced user key")
	}
}

type automaticFleet struct {
	pb.FleetClient
	published  []byte
	publishErr error
	offers     []*pb.TrustPairing
	list       []*pb.TrustPairing
	listCalls  int
	onOffer    func(*pb.TrustPairing)
}

func (f *automaticFleet) PublishUserKey(_ context.Context, req *pb.PublishUserKeyRequest, _ ...grpc.CallOption) (*pb.TenantUser, error) {
	f.published = bytes.Clone(req.UserKey)
	return &pb.TenantUser{}, f.publishErr
}

func (f *automaticFleet) OfferDevicePairing(_ context.Context, p *pb.TrustPairing, _ ...grpc.CallOption) (*pb.TrustPairing, error) {
	if f.onOffer != nil {
		f.onOffer(p)
	}
	f.offers = append(f.offers, proto.Clone(p).(*pb.TrustPairing))
	return p, nil
}

func (f *automaticFleet) ListTrustPairings(_ context.Context, _ *pb.ListTrustPairingsRequest, _ ...grpc.CallOption) (*pb.ListTrustPairingsResponse, error) {
	f.listCalls++
	return &pb.ListTrustPairingsResponse{Pairings: f.list}, nil
}

func automaticIdentity() pki.Identity {
	return pki.Identity{Role: pki.RoleClient, User: "alice", Name: "laptop", ID: nodeid.New()}
}

func TestAutomaticBootstrapRetryAndConflict(t *testing.T) {
	dir := pairingTestDir(t)
	id := automaticIdentity()
	fleet := &automaticFleet{publishErr: errors.New("offline")}
	var notes []string
	note := func(s string) { notes = append(notes, s) }
	who := &pb.WhoamiResponse{Tenancy: true, User: "alice"}
	if err := Automatic(context.Background(), dir, id, fleet, who, note); err == nil {
		t.Fatal("publish failure lost")
	}
	s := Store{Dir: filepath.Join(dir, "trust")}
	k, _, _ := s.UserKey()
	e, err := s.Endorsement()
	if err != nil || e == nil || !bytes.Equal(e.UserKey, k.Public().(ed25519.PublicKey)) || !s.RenewalRequested() {
		t.Fatalf("bootstrap endorsement: %+v %v", e, err)
	}
	policy, _ := s.LoadPolicyFile()
	if policy.RequireWorkers || policy.Users["alice"] == "" {
		t.Fatalf("policy: %+v", policy)
	}
	fleet.publishErr = nil
	if err := Automatic(context.Background(), dir, id, fleet, who, note); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "LAR1-") ||
		!strings.Contains(notes[0], "keep it safe: it restores your user key if you lose all devices") ||
		!bytes.Equal(fleet.published, k.Public().(ed25519.PublicKey)) {
		t.Fatalf("recovery repeated or publish failed: %v", notes)
	}
	if fleet.listCalls != 0 {
		t.Fatal("listed without pairing files")
	}
	who.UserKey = bytes.Repeat([]byte{9}, 32)
	if err := Automatic(context.Background(), dir, id, fleet, who, note); err == nil {
		t.Fatal("accepted conflicting hub key")
	}
	still, _, _ := s.UserKey()
	if !still.Equal(k) {
		t.Fatal("replaced local key")
	}
}

func TestAutomaticDeviceOfferAndInstall(t *testing.T) {
	dir := pairingTestDir(t)
	id := automaticIdentity()
	k := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, 32))
	who := &pb.WhoamiResponse{Tenancy: true, User: "alice", UserKey: k.Public().(ed25519.PublicKey)}
	fleet := &automaticFleet{}
	expiry := time.Now().Add(time.Hour)
	fleet.onOffer = func(remote *pb.TrustPairing) {
		t.Helper()
		ps, err := ListPairings(dir)
		p, ok := ps[remote.Id]
		if err != nil || !ok {
			t.Fatalf("offer sent before secret persisted: %v", err)
		}
		pair, _ := pki.DerivePairing(p.Secret)
		if err := pair.CheckNodeProof(remote.NodeProof, pki.KindDevice, remote.NodeKey); err != nil {
			t.Fatal(err)
		}
		remote.ExpiresAt = timestamppb.New(expiry)
	}
	var notes []string
	note := func(s string) { notes = append(notes, s) }
	for range 2 {
		if err := Automatic(context.Background(), dir, id, fleet, who, note); err != nil {
			t.Fatal(err)
		}
	}
	if len(notes) != 1 || len(fleet.offers) != 2 || fleet.offers[0].Id != fleet.offers[1].Id {
		t.Fatalf("offer was not reused or notice repeated: %+v %v", fleet.offers, notes)
	}
	s := Store{Dir: filepath.Join(dir, "trust")}
	if _, _, err := s.UserKey(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new device created a user key")
	}
	remote := proto.Clone(fleet.offers[0]).(*pb.TrustPairing)
	ps, _ := ListPairings(dir)
	p := ps[remote.Id]
	if !p.ExpiresAt.After(expiry) {
		t.Fatalf("hub's one-hour expiry %s must shorten the local deadline %s", expiry, p.ExpiresAt)
	}
	pair, _ := pki.DerivePairing(p.Secret)
	remote.UserKey = k.Public().(ed25519.PublicKey)
	remote.UserTag = pair.UserTag(pki.KindDevice, "alice", remote.UserKey, remote.ExpiresAt.AsTime())
	e, _ := pki.EndorseID(k, "alice", pki.KindDevice, id.Name, id.ID, remote.NodeKey)
	remote.Endorsement = e.String()
	fleet.list = []*pb.TrustPairing{remote}
	if err := Automatic(context.Background(), dir, id, fleet, who, note); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Endorsement(); e == nil || !s.RenewalRequested() {
		t.Fatal("completed endorsement not installed")
	}
	policy, _ := s.LoadPolicyFile()
	if policy.RequireWorkers || policy.Users["alice"] == "" {
		t.Fatal("device approval changed enforcement or failed to pin self")
	}
}

// TestAutomaticNeedsUsers: on a hub without Longarm users there is nowhere
// to record a user key, so no device makes one.
func TestAutomaticNeedsUsers(t *testing.T) {
	dir := pairingTestDir(t)
	fleet := &automaticFleet{}
	if err := Automatic(context.Background(), dir, automaticIdentity(), fleet, &pb.WhoamiResponse{User: "alice"}, nil); err != nil {
		t.Fatal(err)
	}
	if k, _, _ := (Store{Dir: filepath.Join(dir, "trust")}).UserKey(); k != nil || fleet.published != nil || fleet.offers != nil {
		t.Fatal("automatic trust ran on a hub without users")
	}
}

// TestAutomaticEndorsedDeviceWithoutKey: a device endorsed by hand, whose
// user key lives elsewhere, publishes that key instead of making a second.
func TestAutomaticEndorsedDeviceWithoutKey(t *testing.T) {
	dir := pairingTestDir(t)
	s := Store{Dir: filepath.Join(dir, "trust")}
	id := automaticIdentity()
	userPub, userKey, _ := ed25519.GenerateKey(nil)
	nk, err := s.NodeKey()
	if err != nil {
		t.Fatal(err)
	}
	e, err := pki.EndorseID(userKey, "alice", pki.KindDevice, id.Name, id.ID, nk.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEndorsementFor(e, pki.KindDevice, id); err != nil {
		t.Fatal(err)
	}
	fleet := &automaticFleet{}
	if err := Automatic(context.Background(), dir, id, fleet, &pb.WhoamiResponse{Tenancy: true, User: "alice"}, nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fleet.published, userPub) {
		t.Fatalf("published %x, want the endorsing key %x", fleet.published, userPub)
	}
	if k, _, err := s.UserKey(); k != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("made a second user key: %v %v", k, err)
	}
	if got, _ := s.Endorsement(); got == nil || !bytes.Equal(got.UserKey, userPub) {
		t.Fatal("replaced the endorsement")
	}
}

func TestAutomaticPreservesExistingPolicyAndOwner(t *testing.T) {
	dir := pairingTestDir(t)
	s := Store{Dir: filepath.Join(dir, "trust")}
	id := automaticIdentity()
	fleet := &automaticFleet{}
	who := &pb.WhoamiResponse{Tenancy: true, User: "alice"}
	if err := s.SavePolicyFile(&PolicyFileData{Users: map[string]string{}, RequireWorkers: true}); err != nil {
		t.Fatal(err)
	}
	if err := Automatic(context.Background(), dir, id, fleet, who, nil); err != nil {
		t.Fatal(err)
	}
	policy, err := s.LoadPolicyFile()
	if err != nil || !policy.RequireWorkers {
		t.Fatalf("changed existing enforcement preference: %+v %v", policy, err)
	}
	id.User = "bob"
	who.User = "bob"
	if err := Automatic(context.Background(), dir, id, fleet, who, nil); err == nil {
		t.Fatal("accepted another signed-in user")
	}
	_, owner, err := s.UserKey()
	if err != nil || owner != "alice" {
		t.Fatalf("changed user key owner: %s %v", owner, err)
	}
}

func TestAutomaticConcurrentBootstrap(t *testing.T) {
	dir := pairingTestDir(t)
	id := automaticIdentity()
	const callers = 8
	type result struct {
		key   []byte
		notes []string
		err   error
	}
	results := make(chan result, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fleet := &automaticFleet{}
			var notes []string
			err := Automatic(context.Background(), dir, id, fleet, &pb.WhoamiResponse{Tenancy: true, User: "alice"},
				func(note string) { notes = append(notes, note) })
			results <- result{key: fleet.published, notes: notes, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	s := Store{Dir: filepath.Join(dir, "trust")}
	key, _, err := s.UserKey()
	if err != nil {
		t.Fatal(err)
	}
	recoveryNotes := 0
	for r := range results {
		if r.err != nil || !bytes.Equal(r.key, key.Public().(ed25519.PublicKey)) {
			t.Fatalf("concurrent caller used a different key: %x %v", r.key, r.err)
		}
		recoveryNotes += len(r.notes)
	}
	if recoveryNotes != 1 {
		t.Fatalf("recovery shown %d times", recoveryNotes)
	}
}

func TestAutomaticLockRespectsContext(t *testing.T) {
	dir := pairingTestDir(t)
	s := Store{Dir: filepath.Join(dir, "trust")}
	unlock, err := lockAutomatic(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := lockAutomatic(ctx, s); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock ignored caller's deadline: %v", err)
	}
}

func TestDeviceRejectsAlteredCompletion(t *testing.T) {
	dir := pairingTestDir(t)
	s := Store{Dir: filepath.Join(dir, "trust")}
	id := automaticIdentity()
	p := Pairing{Secret: pki.NewPairSecret(), Kind: pki.KindDevice, Name: id.Name, ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Second)}
	pair, _ := pki.DerivePairing(p.Secret)
	k := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32))
	nk, _ := s.NodeKey()
	e, _ := pki.EndorseID(k, "alice", pki.KindDevice, id.Name, id.ID, nk.Public().(ed25519.PublicKey))
	good := &pb.TrustPairing{Id: pair.ID, Kind: pki.KindDevice, User: "alice", NodeId: id.ID, NodeName: id.Name,
		NodeKey: e.NodeKey, UserKey: e.UserKey, Endorsement: e.String(), ExpiresAt: timestamppb.New(p.ExpiresAt)}
	good.UserTag = pair.UserTag(good.Kind, good.User, good.UserKey, p.ExpiresAt)
	for name, alter := range map[string]func(*pb.TrustPairing){
		"tag":    func(p *pb.TrustPairing) { p.UserTag[0] ^= 1 },
		"owner":  func(p *pb.TrustPairing) { p.User = "bob" },
		"node":   func(p *pb.TrustPairing) { p.NodeId = nodeid.New() },
		"kind":   func(p *pb.TrustPairing) { p.Kind = pki.KindWorker },
		"expiry": func(p *pb.TrustPairing) { p.ExpiresAt = timestamppb.New(time.Now().Add(-time.Hour)) },
		"extended expiry": func(remote *pb.TrustPairing) {
			remote.ExpiresAt = timestamppb.New(p.ExpiresAt.Add(time.Hour))
			remote.UserTag = pair.UserTag(remote.Kind, remote.User, remote.UserKey, remote.ExpiresAt.AsTime())
		},
		"endorsement": func(p *pb.TrustPairing) {
			wrong, _ := pki.EndorseID(k, "bob", pki.KindDevice, id.Name, id.ID, e.NodeKey)
			p.Endorsement = wrong.String()
		},
		"endorsement key": func(p *pb.TrustPairing) {
			other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32))
			wrong, _ := pki.EndorseID(other, "alice", pki.KindDevice, id.Name, id.ID, e.NodeKey)
			p.Endorsement = wrong.String()
		},
		"endorsement name": func(p *pb.TrustPairing) {
			wrong, _ := pki.EndorseID(k, "alice", pki.KindDevice, "other-device", id.ID, e.NodeKey)
			p.Endorsement = wrong.String()
		},
	} {
		t.Run(name, func(t *testing.T) {
			remote := proto.Clone(good).(*pb.TrustPairing)
			alter(remote)
			if err := installDevice(s, id, p, remote); err == nil {
				t.Fatal("altered completion installed")
			}
		})
	}
	if e, _ := s.Endorsement(); e != nil {
		t.Fatal("rejected completion wrote endorsement")
	}
}

func TestPairFailedAuditRedactsSecrets(t *testing.T) {
	dir := pairingTestDir(t)
	err := errors.New("secret: " + pki.NewPairSecret())
	if got := PairFailed(dir, err); got != nil {
		t.Fatal(got)
	}
	b, readErr := os.ReadFile(filepath.Join(dir, "audit.log"))
	if readErr != nil || !bytes.Contains(b, []byte("trust/pair-failed")) || bytes.Contains(b, []byte(err.Error())) {
		t.Fatalf("audit missing or leaked secret: %s %v", b, readErr)
	}
	if err := PairFailed(filepath.Join(dir, "audit.log"), err); err == nil {
		t.Fatal("audit write failure was swallowed")
	}
}
