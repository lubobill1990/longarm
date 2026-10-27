package portlink

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// The owner's consent (end-to-end trust, docs/security.md "Port links"): a
// grant is the hub's word, and with end-to-end trust the hub is not trusted
// to grant access. The device that creates a link signs the link's spec
// with its node key and attaches its endorsement by the owner's user key;
// a worker that pins the owner's key accepts the link only with a valid
// consent matching the link exactly. The hub can store, pass on, drop or
// replay a consent (until its expiry), not make one up or change it.

const consentContext = "longarm port-link consent v1\x00"

func consentMessage(spec []byte) []byte {
	return append([]byte(consentContext), spec...)
}

// SignConsent signs spec with the device's node key; e is the device's
// endorsement (kind device). spec.Owner and spec.Device are taken from e.
func SignConsent(nodeKey ed25519.PrivateKey, e *pki.Endorsement, spec *pb.PortLinkSpec) (*pb.PortLinkConsent, error) {
	if e == nil || e.Kind != pki.KindDevice {
		return nil, errors.New("port link consent: this device is not endorsed as a device")
	}
	if !bytes.Equal(e.NodeKey, nodeKey.Public().(ed25519.PublicKey)) {
		return nil, errors.New("port link consent: the endorsement is for another node key")
	}
	spec = proto.Clone(spec).(*pb.PortLinkSpec)
	spec.Owner, spec.Device = e.User, e.Name
	if spec.IssuedUnix == 0 {
		spec.IssuedUnix = time.Now().Unix()
	}
	b, err := proto.Marshal(spec)
	if err != nil {
		return nil, err
	}
	return &pb.PortLinkConsent{Spec: b, Signature: ed25519.Sign(nodeKey, consentMessage(b)), Endorsement: e.String()}, nil
}

// ParseConsent checks what needs no trusted key: the endorsement's own
// signature, that it endorses a device of the spec's owner under the spec's
// device name, and the device's signature over the spec. Whether the
// endorsing key is the owner's is VerifyConsent's question.
func ParseConsent(c *pb.PortLinkConsent) (*pb.PortLinkSpec, *pki.Endorsement, error) {
	if c == nil || len(c.Spec) == 0 || len(c.Signature) == 0 || c.Endorsement == "" {
		return nil, nil, errors.New("no owner's consent (the link was not signed by an endorsed device of its owner: create it with longarm link create on such a device)")
	}
	if len(c.Spec) > 2048 || len(c.Endorsement) > 4096 {
		return nil, nil, errors.New("port link consent too large")
	}
	e, err := pki.ParseEndorsement(c.Endorsement)
	if err != nil {
		return nil, nil, fmt.Errorf("port link consent: %w", err)
	}
	if e.Kind != pki.KindDevice {
		return nil, nil, fmt.Errorf("port link consent: signed by a %s, not by a device", e.Kind)
	}
	if !ed25519.Verify(e.NodeKey, consentMessage(c.Spec), c.Signature) {
		return nil, nil, errors.New("port link consent: bad signature")
	}
	s := &pb.PortLinkSpec{}
	if err := proto.Unmarshal(c.Spec, s); err != nil {
		return nil, nil, fmt.Errorf("port link consent: %w", err)
	}
	if s.Owner != e.User || s.Device != e.Name {
		return nil, nil, fmt.Errorf("port link consent: spec of %s/%s signed by %s's device %s", s.Owner, s.Device, e.User, e.Name)
	}
	if !ValidID(s.Id) || s.From == "" || s.To == "" || !ValidPort(s.Port) || !ValidPort(s.ListenPort) || s.ExpiresUnix == 0 {
		return nil, nil, errors.New("port link consent: incomplete spec")
	}
	return s, e, nil
}

// ConsentMatches reports how l differs from what its owner consented to
// (nil: it doesn't; it may end earlier).
func ConsentMatches(s *pb.PortLinkSpec, l *pb.PortLink) error {
	switch {
	case s.Id != l.Id, s.Owner != l.Owner, s.From != l.From, s.To != l.To, s.Port != l.Port, s.ListenPort != l.ListenPort,
		s.JobId != l.JobId:
		return fmt.Errorf("link %s is not what its owner signed (%s %s -> %s:%d on port %d)", l.Id, s.Id, s.From, s.To, s.Port, s.ListenPort)
	case l.ExpiresAt == nil || l.ExpiresAt.AsTime().Unix() > s.ExpiresUnix:
		return fmt.Errorf("link %s outlives what its owner signed", l.Id)
	}
	return nil
}

// VerifyConsent checks l's consent under the trust policy p: a valid
// consent, signed by a device endorsed by the key p pins for l's owner (not
// a denied node), for exactly this link.
func VerifyConsent(p *pki.TrustPolicy, l *pb.PortLink) error {
	s, e, err := ParseConsent(l.GetConsent())
	if err != nil {
		return err
	}
	key, ok := p.Users[l.Owner]
	if !ok || !bytes.Equal(key, e.UserKey) || e.User != l.Owner {
		return fmt.Errorf("link %s: its consent is not signed by a device endorsed by %s's pinned key", l.Id, l.Owner)
	}
	for _, d := range p.DenyNodes {
		if bytes.Equal(d, e.NodeKey) {
			return fmt.Errorf("link %s: signed by a denied device", l.Id)
		}
	}
	return ConsentMatches(s, l)
}
