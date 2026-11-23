package pki

import (
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Automatic endorsement (#173, docs/security.md "End-to-end trust"): the
// user's device and a joining node share a pairing secret the hub never
// sees (`longarmd join ... --pair S`, `longarm device approve S`). The
// hub routes the pairing by an id derived from the secret and carries two
// MACs keyed by it:
//
//   - user_tag, made by the user's device: "user U with user key K" - the
//     node checks it before it pins K, so the hub can't substitute a key;
//   - node_proof, made by the joining node: "my node key is N" - the
//     device checks it before it endorses N, so the hub can't slip in
//     another node.
//
// The secret has 100 bits: the hub can't guess it offline from the MACs.

const (
	pairSecretLen = 20 // Crockford base32 characters
	crockford     = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// NewPairSecret is a fresh pairing secret, XXXXX-XXXXX-XXXXX-XXXXX.
func NewPairSecret() string {
	b := make([]byte, pairSecretLen)
	rand.Read(b)
	var s strings.Builder
	for i, c := range b {
		if i > 0 && i%5 == 0 {
			s.WriteByte('-')
		}
		s.WriteByte(crockford[c&31])
	}
	return s.String()
}

// ParsePairSecret normalizes a typed secret: case, dashes and spaces don't
// matter, and I/L read as 1, O as 0 (Crockford).
func ParsePairSecret(s string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == '-' || r == ' ':
			continue
		case r == 'I' || r == 'L':
			r = '1'
		case r == 'O':
			r = '0'
		}
		if r > 127 || !strings.ContainsRune(crockford, r) {
			return "", fmt.Errorf("pairing secret: unexpected %q", r)
		}
		b.WriteRune(r)
	}
	if b.Len() != pairSecretLen {
		return "", fmt.Errorf("pairing secret: want %d characters, got %d", pairSecretLen, b.Len())
	}
	return b.String(), nil
}

// Pairing holds what a secret derives: the id the hub may see and the two
// MAC keys it must not.
type Pairing struct {
	ID      string // hex, the hub's handle for the pairing
	userKey []byte
	nodeKey []byte
}

// DerivePairing derives a pairing from a secret (any form ParsePairSecret
// accepts).
func DerivePairing(secret string) (*Pairing, error) {
	s, err := ParsePairSecret(secret)
	if err != nil {
		return nil, err
	}
	key := func(info string, n int) []byte {
		k, err := hkdf.Key(sha256.New, []byte(s), nil, info, n)
		if err != nil {
			panic(err) // only for lengths sha256 can't produce
		}
		return k
	}
	return &Pairing{ID: hex.EncodeToString(key("longarm-pair-v1 id", 12)),
		userKey: key("longarm-pair-v1 user", 32), nodeKey: key("longarm-pair-v1 node", 32)}, nil
}

// ValidPairID reports whether id looks like a pairing id.
func ValidPairID(id string) bool {
	if len(id) != 24 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

func (p *Pairing) mac(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// UserTag is the user's device's statement "user, with userKey, made
// this pairing of kind, valid until expires".
func (p *Pairing) UserTag(kind, user string, userKey ed25519.PublicKey, expires time.Time) []byte {
	b64 := base64.StdEncoding.EncodeToString
	return p.mac(p.userKey, fmt.Sprintf("longarm-pair-v1-user\n%s\n%s\n%s\n%s\n%d\n",
		kind, p.ID, user, b64(userKey), expires.Unix()))
}

// NodeProof is the joining node's statement "my node key is nodeKey".
func (p *Pairing) NodeProof(kind string, nodeKey ed25519.PublicKey) []byte {
	b64 := base64.StdEncoding.EncodeToString
	return p.mac(p.nodeKey, fmt.Sprintf("longarm-pair-v1-node\n%s\n%s\n%s\n", kind, p.ID, b64(nodeKey)))
}

// ErrPairMismatch: a MAC doesn't match - the hub (or whoever carried the
// pairing) changed it, or it belongs to another secret.
var ErrPairMismatch = errors.New("pairing does not match its secret")

// CheckUserTag verifies UserTag.
func (p *Pairing) CheckUserTag(tag []byte, kind, user string, userKey ed25519.PublicKey, expires time.Time) error {
	if len(userKey) != ed25519.PublicKeySize || !hmac.Equal(tag, p.UserTag(kind, user, userKey, expires)) {
		return ErrPairMismatch
	}
	return nil
}

// CheckNodeProof verifies NodeProof.
func (p *Pairing) CheckNodeProof(proof []byte, kind string, nodeKey ed25519.PublicKey) error {
	if len(nodeKey) != ed25519.PublicKeySize || !hmac.Equal(proof, p.NodeProof(kind, nodeKey)) {
		return ErrPairMismatch
	}
	return nil
}
