package pki

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
)

// replaceDomain separates replacement proofs from every other signature
// a node key makes (TLS handshakes sign their own transcripts).
const replaceDomain = "longarm link replace v1\x00"

// ReplaceMessage is what a node's key signs for a pairing to replace it
// (StartLinkRequest.replaces): the SHA-256 of each of the pairing's CSRs
// (missing ones: of nothing). The proof is bound to the new keys: it can't
// be replayed for a pairing of anyone else's keys.
func ReplaceMessage(csrPEM, workerCSRPEM, adminCSRPEM []byte) []byte {
	m := []byte(replaceDomain)
	for _, b := range [][]byte{csrPEM, workerCSRPEM, adminCSRPEM} {
		h := sha256.Sum256(b)
		m = append(m, h[:]...)
	}
	return m
}

// SignReplace signs msg (ReplaceMessage) with the node key of a
// certificate bundle (certPEM, keyPEM) - checking that they belong
// together - for VerifyReplace: ECDSA with SHA-256, ASN.1.
func SignReplace(certPEM, keyPEM, msg []byte) ([]byte, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	k, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an ECDSA node key")
	}
	h := sha256.Sum256(msg)
	return ecdsa.SignASN1(rand.Reader, k, h[:])
}

// VerifyReplace checks SignReplace's signature with c's public key.
func VerifyReplace(c *x509.Certificate, msg, sig []byte) error {
	pub, ok := c.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return errors.New("not an ECDSA certificate")
	}
	h := sha256.Sum256(msg)
	if !ecdsa.VerifyASN1(pub, h[:], sig) {
		return errors.New("the signature does not match the certificate's key")
	}
	return nil
}
