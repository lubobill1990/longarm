// Package portlink holds what the hub and the workers share about port
// links (worker-to-worker forwards for joint debugging, docs/usage.md
// "Port links"): the hub-signed grant a worker presents to the link's
// target, link ids, and the environment jobs get.
//
// The hub signs a grant with the key of its hub certificate, which the
// fleet CA issued; every worker trusts that CA already (it verifies client
// certificates with it), so no new key has to be distributed. A grant alone
// is not enough: the target worker also requires the link in the list of
// active links the hub pushed to it (removal = revocation).
package portlink

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// signContext separates grant signatures from anything else the hub's key
// signs (TLS handshakes).
const signContext = "longarm port-link grant v1\x00"

// ClockSkew tolerated between the hub and a worker for issued_unix.
const ClockSkew = 5 * time.Minute

var idRE = regexp.MustCompile(`^lk-[0-9a-f]{12}$`)

// NewID returns a fresh link id ("lk-" + 12 hex digits).
func NewID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "lk-" + hex.EncodeToString(b)
}

// ValidID reports whether id looks like a link id.
func ValidID(id string) bool { return idRE.MatchString(id) }

func digest(grant []byte) []byte {
	h := sha256.New()
	h.Write([]byte(signContext))
	h.Write(grant)
	return h.Sum(nil)
}

// Sign signs g with cert's private key (the hub certificate) and attaches
// the certificate chain.
func Sign(cert *tls.Certificate, g *pb.PortLinkGrant) (*pb.SignedPortLinkGrant, error) {
	if cert == nil || len(cert.Certificate) == 0 {
		return nil, errors.New("no signing certificate")
	}
	signer, ok := cert.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, errors.New("signing key is not a crypto.Signer")
	}
	b, err := proto.Marshal(g)
	if err != nil {
		return nil, err
	}
	d := digest(b)
	var sig []byte
	switch signer.Public().(type) {
	case ed25519.PublicKey:
		sig, err = signer.Sign(rand.Reader, d, crypto.Hash(0))
	default:
		sig, err = signer.Sign(rand.Reader, d, crypto.SHA256)
	}
	if err != nil {
		return nil, err
	}
	return &pb.SignedPortLinkGrant{Grant: b, Signature: sig, Chain: append([][]byte(nil), cert.Certificate...)}, nil
}

// Verify checks a signed grant: the chain leads to a hub certificate of the
// fleet CA (roots), the signature is the hub's, and the grant has not
// expired at now. It returns the grant; the caller checks what it names.
func Verify(roots *x509.CertPool, s *pb.SignedPortLinkGrant, now time.Time) (*pb.PortLinkGrant, error) {
	if s == nil || len(s.Grant) == 0 || len(s.Signature) == 0 || len(s.Chain) == 0 {
		return nil, errors.New("no link grant")
	}
	if len(s.Chain) > 4 || len(s.Grant) > 4096 {
		return nil, errors.New("link grant too large")
	}
	leaf, err := x509.ParseCertificate(s.Chain[0])
	if err != nil {
		return nil, fmt.Errorf("link grant certificate: %w", err)
	}
	inter := x509.NewCertPool()
	for _, der := range s.Chain[1:] {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, fmt.Errorf("link grant chain: %w", err)
		}
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return nil, fmt.Errorf("link grant certificate: %w", err)
	}
	id, err := pki.IdentityFromCert(leaf)
	if err != nil {
		return nil, fmt.Errorf("link grant certificate: %w", err)
	}
	if id.Role != pki.RoleHub {
		return nil, fmt.Errorf("link grant signed by %s, not by the hub", id)
	}
	d := digest(s.Grant)
	switch pub := leaf.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(pub, d, s.Signature) {
			return nil, errors.New("link grant: bad signature")
		}
	case ed25519.PublicKey:
		if !ed25519.Verify(pub, d, s.Signature) {
			return nil, errors.New("link grant: bad signature")
		}
	default:
		return nil, fmt.Errorf("link grant: unsupported key type %T", leaf.PublicKey)
	}
	g := &pb.PortLinkGrant{}
	if err := proto.Unmarshal(s.Grant, g); err != nil {
		return nil, fmt.Errorf("link grant: %w", err)
	}
	if !ValidID(g.Id) || g.Owner == "" || g.From == "" || g.To == "" || !ValidPort(g.Port) || !ValidPort(g.ListenPort) {
		return nil, errors.New("link grant: incomplete")
	}
	if exp := time.Unix(g.ExpiresUnix, 0); !now.Before(exp) {
		return nil, fmt.Errorf("link grant %s expired at %s", g.Id, exp.UTC().Format(time.RFC3339))
	}
	if time.Unix(g.IssuedUnix, 0).After(now.Add(ClockSkew)) {
		return nil, fmt.Errorf("link grant %s issued in the future (clock skew?)", g.Id)
	}
	return g, nil
}

// ValidPort reports whether p is a TCP port.
func ValidPort(p int32) bool { return p > 0 && p <= 65535 }

// Link is what a job's environment needs of a link.
type Link struct {
	To         string // target worker
	Port       int32  // target port on To
	ListenPort int32  // 127.0.0.1:<ListenPort> here
}

// EnvLinks is the job environment variable listing the owner's links on
// this worker: "B:8080=127.0.0.1:18080,C:5432=127.0.0.1:15432".
const EnvLinks = "LONGARM_LINKS"

// EnvName is the per-link variable LONGARM_LINK_<WORKER>_<PORT> (worker name
// upper-cased, other characters than A-Z0-9 as "_").
func EnvName(to string, port int32) string {
	n := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - ('a' - 'A')
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, to)
	return "LONGARM_LINK_" + n + "_" + strconv.Itoa(int(port))
}

// Env is the environment for jobs given the owner's links (nil if none).
func Env(links []Link) map[string]string {
	if len(links) == 0 {
		return nil
	}
	sort.Slice(links, func(i, j int) bool {
		if links[i].To != links[j].To {
			return links[i].To < links[j].To
		}
		return links[i].Port < links[j].Port
	})
	env := map[string]string{}
	clash := map[string]bool{}
	parts := make([]string, 0, len(links))
	for _, l := range links {
		addr := "127.0.0.1:" + strconv.Itoa(int(l.ListenPort))
		parts = append(parts, fmt.Sprintf("%s:%d=%s", l.To, l.Port, addr))
		n := EnvName(l.To, l.Port)
		if old, taken := env[n]; taken && old != addr {
			clash[n] = true // workers "a-b" and "a_b": only LONGARM_LINKS tells them apart
		}
		env[n] = addr
	}
	for n := range clash {
		delete(env, n)
	}
	env[EnvLinks] = strings.Join(parts, ",")
	return env
}

// IsEnv reports whether name is one of the variables Env sets.
func IsEnv(name string) bool {
	return name == EnvLinks || strings.HasPrefix(name, "LONGARM_LINK_")
}
