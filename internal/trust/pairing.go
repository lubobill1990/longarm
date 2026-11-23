package trust

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"longarm/internal/pki"
	"longarm/internal/platform"
	"longarm/internal/server"
	pb "longarm/proto/longarm/v1"
)

// Pairing is the local, secret half of an automatic trust exchange.
// The directory arguments below are config/data roots, not Store.Dir.
type Pairing struct {
	Secret        string    `json:"secret"`
	Kind          string    `json:"kind"`
	Name          string    `json:"name"`
	ExpiresAt     time.Time `json:"expires_at"`
	LastOfferedAt time.Time `json:"last_offered_at,omitempty"`
}

func pairingPath(dir, id string) (string, error) {
	if !pki.ValidPairID(id) {
		return "", errors.New("invalid pairing id")
	}
	return filepath.Join(dir, "trust", "pairings", id+".json"), nil
}

func (p Pairing) id() (string, error) {
	k, err := pki.DerivePairing(p.Secret)
	if err != nil {
		return "", err
	}
	if p.Kind != pki.KindWorker && p.Kind != pki.KindDevice {
		return "", errors.New("invalid pairing kind")
	}
	if !pki.ValidName(p.Name) || p.ExpiresAt.IsZero() {
		return "", errors.New("pairing needs a valid name and expiry")
	}
	return k.ID, nil
}

func writePrivate(path string, b []byte) error {
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".trust-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return platform.ReplaceFile(f.Name(), path)
}

// WritePairing stores p under the id derived from its secret, privately.
func WritePairing(dir string, p Pairing) error {
	id, err := p.id()
	if err != nil {
		return err
	}
	path, err := pairingPath(dir, id)
	if err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writePrivate(path, append(b, '\n'))
}

// ReadPairing reads a pairing and verifies its secret agrees with its filename.
func ReadPairing(dir, id string) (Pairing, error) {
	var p Pairing
	path, err := pairingPath(dir, id)
	if err != nil {
		return p, err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return p, err
	}
	if !fi.Mode().IsRegular() {
		return p, errors.New("pairing is not a regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, err
	}
	got, err := p.id()
	if err != nil {
		return p, err
	}
	if got != id {
		return p, errors.New("pairing secret does not match its id")
	}
	return p, nil
}

// ListPairings returns local pairings, retaining expired offers for 30 days
// so an offline device does not lose pending exchanges immediately.
func ListPairings(dir string) (map[string]Pairing, error) {
	out := map[string]Pairing{}
	es, err := os.ReadDir(filepath.Join(dir, "trust", "pairings"))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range es {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		p, err := ReadPairing(dir, id)
		if err != nil {
			return nil, fmt.Errorf("pairing %s: %w", id, err)
		}
		if time.Now().After(p.ExpiresAt.Add(30 * 24 * time.Hour)) {
			if err := DeletePairing(dir, id); err != nil {
				return nil, err
			}
			continue
		}
		out[id] = p
	}
	return out, nil
}

// DeletePairing removes a completed pairing (a missing file is harmless).
func DeletePairing(dir, id string) error {
	path, err := pairingPath(dir, id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// WritePinnedUser records the worker's owner without activating trust.toml.
// An existing different pin must be changed explicitly, never by the hub.
func WritePinnedUser(dir string, user pki.UserKeyLine) error {
	if _, err := pki.ParseUserKeyLine(user.String()); err != nil {
		return err
	}
	if old, err := ReadPinnedUser(dir); err == nil {
		if old.User == user.User && bytes.Equal(old.Key, user.Key) {
			return nil
		}
		return errors.New("another user key is already pinned")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writePrivate(filepath.Join(dir, "trust", "pinned-user"), []byte(user.String()+"\n"))
}

// ReadPinnedUser reads the owner pin, independent of the enforcement policy.
func ReadPinnedUser(dir string) (pki.UserKeyLine, error) {
	b, err := os.ReadFile(filepath.Join(dir, "trust", "pinned-user"))
	if err != nil {
		return pki.UserKeyLine{}, err
	}
	return pki.ParseUserKeyLine(strings.TrimSpace(string(b)))
}

var recoveryEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

func recoveryChecksum(seed []byte) []byte {
	h := sha256.Sum256(append([]byte("longarm-recovery-v1\n"), seed...))
	return h[:4]
}

// EncodeRecovery encodes an Ed25519 seed with a version and typo checksum.
func EncodeRecovery(seed []byte) (string, error) {
	if len(seed) != ed25519.SeedSize {
		return "", errors.New("recovery requires a 32-byte Ed25519 seed")
	}
	raw := recoveryEncoding.EncodeToString(append(bytes.Clone(seed), recoveryChecksum(seed)...))
	groups := []string{"LAR1"}
	for len(raw) > 5 {
		groups = append(groups, raw[:5])
		raw = raw[5:]
	}
	return strings.Join(append(groups, raw), "-"), nil
}

// DecodeRecovery accepts Crockford case/ambiguous letters and verifies the checksum.
func DecodeRecovery(code string) ([]byte, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if !strings.HasPrefix(code, "LAR1-") {
		return nil, errors.New("not an LAR1 recovery code")
	}
	raw := strings.NewReplacer("-", "", " ", "", "I", "1", "L", "1", "O", "0").Replace(code[5:])
	b, err := recoveryEncoding.DecodeString(raw)
	if err != nil || len(b) != ed25519.SeedSize+4 || recoveryEncoding.EncodeToString(b) != raw {
		return nil, errors.New("invalid recovery code")
	}
	if !bytes.Equal(b[32:], recoveryChecksum(b[:32])) {
		return nil, errors.New("recovery checksum mismatch")
	}
	return bytes.Clone(b[:32]), nil
}

// ImportRecovery imports like ImportUserKey, refusing to replace another key.
func (s Store) ImportRecovery(code, user string) error {
	if !pki.ValidUser(user) {
		return errors.New("invalid recovery user")
	}
	seed, err := DecodeRecovery(code)
	if err != nil {
		return err
	}
	k := ed25519.NewKeyFromSeed(seed)
	if old, owner, err := s.UserKey(); err == nil {
		if owner == user && old.Equal(k) {
			return nil
		}
		return errors.New("this device holds another user key")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b, err := encodeKey(k, "LONGARM USER KEY", map[string]string{"User": user})
	if err != nil {
		return err
	}
	return writePrivate(s.path(UserKeyFile), b)
}

// Automatic bootstraps trust on a signed-in client, or offers/polls its device
// pairing. It never enables require_workers. Callers warn on errors and keep
// sign-in usable; the hub's user key is only a hint, never a trust anchor.
func Automatic(ctx context.Context, dir string, id pki.Identity, fleet pb.FleetClient, who *pb.WhoamiResponse, note func(string)) (err error) {
	defer func() {
		if err != nil {
			err = errors.Join(err, PairFailed(dir, err))
		}
	}()
	if note == nil {
		note = func(string) {}
	}
	if who == nil || fleet == nil {
		return errors.New("automatic trust needs an authenticated hub")
	}
	if !who.Tenancy {
		// No Longarm users on this hub: nowhere to record the user key, and
		// every device would take itself for the first and make its own.
		return nil
	}
	user := id.Owner()
	if (id.Role != pki.RoleClient && id.Role != pki.RoleAdmin) || !pki.ValidUser(user) || (who.User != "" && who.User != user) {
		return errors.New("automatic trust identity does not match the signed-in user")
	}
	s := Store{Dir: filepath.Join(dir, "trust")}
	unlock, err := lockAutomatic(ctx, s)
	if err != nil {
		return err
	}
	defer unlock()
	e, err := s.Endorsement()
	if err != nil {
		return err
	}
	if e != nil {
		if e.User != user {
			return errors.New("existing endorsement belongs to another user")
		}
		if err := checkDeviceEndorsement(s, e, id); err != nil {
			return err
		}
	}
	k, owner, err := s.UserKey()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if k == nil && len(who.UserKey) == 0 && e == nil {
		k, err = s.CreateUserKey(user)
		if err != nil {
			return err
		}
		owner = user
		code, err := EncodeRecovery(k.Seed())
		if err != nil {
			return err
		}
		note("save your recovery code (shown once): " + code + " — keep it safe: it restores your user key if you lose all devices")
	}
	if k != nil {
		pub := k.Public().(ed25519.PublicKey)
		if owner != user || (len(who.UserKey) > 0 && !bytes.Equal(who.UserKey, pub)) {
			return errors.New("the local user key conflicts with the signed-in user's key")
		}
		if e != nil && !bytes.Equal(e.UserKey, pub) {
			return errors.New("existing endorsement conflicts with the local user key")
		}
		if err := pinSelf(s, pki.UserKeyLine{User: user, Key: pub}); err != nil {
			return err
		}
		if e == nil {
			nk, err := s.NodeKey()
			if err != nil {
				return err
			}
			e, err = pki.EndorseID(k, user, pki.KindDevice, id.Name, id.ID, nk.Public().(ed25519.PublicKey))
			if err != nil {
				return err
			}
			if err := s.SetEndorsementFor(e, pki.KindDevice, id); err != nil {
				return err
			}
		}
		if len(who.UserKey) == 0 {
			_, err := fleet.PublishUserKey(ctx, &pb.PublishUserKeyRequest{UserKey: pub})
			return err
		}
		return nil
	}
	if e != nil {
		if err := pinSelf(s, pki.UserKeyLine{User: user, Key: e.UserKey}); err != nil {
			return err
		}
		// An endorsed device without the key (set up by hand, or the key
		// lives on another device) still knows the user's real key: publish
		// it, so the user's next device doesn't take itself for the first
		// and make a second key.
		if len(who.UserKey) == 0 {
			_, err := fleet.PublishUserKey(ctx, &pb.PublishUserKeyRequest{UserKey: e.UserKey})
			return err
		}
		return nil
	}
	return automaticDevice(ctx, dir, s, id, fleet, note)
}

// Keep the lock file: removing it would let another process lock a different
// inode. OS locks are released even if a CLI or daemon exits unexpectedly.
func lockAutomatic(ctx context.Context, s Store) (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.path("automatic.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, fmt.Errorf("waiting for automatic trust lock: %w", err)
		}
		if err := platform.LockFile(f); err == nil {
			return func() { f.Close() }, nil
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
}

// PairFailed records a failed local trust exchange, returning audit errors only.
// Error text is deliberately excluded: it may contain a
// pairing secret, recovery code, or untrusted text supplied by a hub.
func PairFailed(root string, err error) error {
	if err == nil {
		return nil
	}
	if e := os.MkdirAll(root, 0o700); e != nil {
		return fmt.Errorf("opening local trust audit: %w", e)
	}
	path := filepath.Join(root, "audit.log")
	f, e := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if e != nil {
		return fmt.Errorf("opening local trust audit: %w", e)
	}
	before, e := f.Stat()
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		return errors.Join(e, closeErr)
	}
	server.NewAudit(path).LogAs("local", "trust/pair-failed", map[string]any{"reason": "automatic trust exchange failed"})
	after, e := os.Stat(path)
	if e != nil {
		return fmt.Errorf("checking local trust audit: %w", e)
	}
	if after.Size() <= before.Size() {
		return errors.New("local trust audit could not append the failure")
	}
	return nil
}

func pinSelf(s Store, key pki.UserKeyLine) error {
	f, err := s.LoadPolicyFile()
	if err != nil {
		return err
	}
	if v, exists := f.Users[key.User]; exists {
		old, err := parseUserKey(key.User, v)
		if err != nil {
			return err
		}
		if !bytes.Equal(old, key.Key) {
			return errors.New("self policy already pins another user key")
		}
		return nil
	}
	f.Users[key.User] = key.String()
	return s.SavePolicyFile(f)
}

func checkDeviceEndorsement(s Store, e *pki.Endorsement, id pki.Identity) error {
	nk, err := s.NodeKey()
	if err != nil {
		return err
	}
	if e.Kind != pki.KindDevice || e.User != id.Owner() || !bytes.Equal(e.NodeKey, nk.Public().(ed25519.PublicKey)) ||
		(e.ID != "" && e.ID != id.ID) || (e.ID == "" && (id.ID != "" || e.Name != id.Name)) {
		return errors.New("endorsement does not identify this device and owner")
	}
	return e.Verify()
}

func automaticDevice(ctx context.Context, dir string, s Store, id pki.Identity, fleet pb.FleetClient, note func(string)) error {
	ps, err := ListPairings(dir)
	if err != nil {
		return err
	}
	var ids []string
	for pid, p := range ps {
		if p.Kind == pki.KindDevice && p.Name == id.Name && time.Now().Before(p.ExpiresAt) {
			ids = append(ids, pid)
		}
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		r, err := fleet.ListTrustPairings(ctx, &pb.ListTrustPairingsRequest{Ids: ids})
		if err != nil {
			return err
		}
		for _, remote := range r.GetPairings() {
			if remote == nil {
				continue
			}
			p, ok := ps[remote.Id]
			if !ok || p.Kind != pki.KindDevice || p.Name != id.Name || remote.Endorsement == "" {
				continue
			}
			if err := installDevice(s, id, p, remote); err != nil {
				return err
			}
			note("this device is now endorsed")
			return DeletePairing(dir, remote.Id)
		}
	}
	var p Pairing
	if len(ids) > 0 {
		p = ps[ids[0]]
	} else {
		p = Pairing{Secret: pki.NewPairSecret(), Kind: pki.KindDevice, Name: id.Name, ExpiresAt: time.Now().Add(24 * time.Hour).Truncate(time.Second)}
		if err := WritePairing(dir, p); err != nil {
			return err
		}
	}
	pair, err := pki.DerivePairing(p.Secret)
	if err != nil {
		return err
	}
	nk, err := s.NodeKey()
	if err != nil {
		return err
	}
	_, err = fleet.OfferDevicePairing(ctx, &pb.TrustPairing{
		Id: pair.ID, Kind: pki.KindDevice, User: id.Owner(), NodeId: id.ID, NodeName: id.Name,
		NodeKey: nk.Public().(ed25519.PublicKey), NodeProof: pair.NodeProof(pki.KindDevice, nk.Public().(ed25519.PublicKey)),
		ExpiresAt: timestamppb.New(p.ExpiresAt),
	})
	if err != nil {
		return err
	}
	if time.Since(p.LastOfferedAt) >= 24*time.Hour {
		p.LastOfferedAt = time.Now()
		if err := WritePairing(dir, p); err != nil {
			return err
		}
		note("on another device: longarm device approve " + p.Secret)
	}
	return nil
}

func installDevice(s Store, id pki.Identity, p Pairing, remote *pb.TrustPairing) error {
	pair, err := pki.DerivePairing(p.Secret)
	if err != nil {
		return err
	}
	if remote.ExpiresAt == nil || remote.ExpiresAt.CheckValid() != nil {
		return errors.New("device pairing has no valid expiry")
	}
	expiry := remote.ExpiresAt.AsTime()
	if pair.ID != remote.Id || remote.Kind != pki.KindDevice || remote.User != id.Owner() ||
		remote.NodeId != id.ID || remote.NodeName != id.Name || !time.Now().Before(expiry) ||
		!time.Now().Before(p.ExpiresAt) || expiry.Unix() != p.ExpiresAt.Unix() {
		return errors.New("device pairing identity or expiry does not match")
	}
	if err := pair.CheckUserTag(remote.UserTag, pki.KindDevice, id.Owner(), remote.UserKey, expiry); err != nil {
		return err
	}
	e, err := pki.ParseEndorsement(remote.Endorsement)
	if err != nil {
		return err
	}
	if !bytes.Equal(e.UserKey, remote.UserKey) || e.User != id.Owner() || e.Name != id.Name {
		return errors.New("device endorsement does not match the paired owner, key, and name")
	}
	if err := checkDeviceEndorsement(s, e, id); err != nil {
		return fmt.Errorf("paired device: %w", err)
	}
	if err := pinSelf(s, pki.UserKeyLine{User: id.Owner(), Key: remote.UserKey}); err != nil {
		return err
	}
	return s.SetEndorsementFor(e, pki.KindDevice, id)
}
