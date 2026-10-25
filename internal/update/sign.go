// Package update is longarmd's self-update (DESIGN §3.1 "update", §7.2): the hub
// tells a worker which version to run; the worker downloads the release
// files through the hub, verifies their minisign signatures against the
// public key embedded at build time, swaps its binaries (the running ones
// are renamed, never deleted while shims may still run from them), restarts
// through the service manager, health-checks the new version for a while
// and - if it fails or never comes up - a watchdog running from the old
// binary puts the old files back and restarts again.
package update

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"

	"aead.dev/minisign"
)

// PublicKey is the minisign public key ("RW..." base64 line) that release
// signatures must verify against. Set at build time:
//
//	-ldflags "-X github.com/lubobill1990/longarm/internal/update.PublicKey=RWQ..."
//
// Without it, self-update is disabled (dev builds).
var PublicKey = ""

// SigExt is the extension of signature files next to release files.
const SigExt = ".minisig"

// trustedPrefix starts every trusted comment; it binds a signature to one
// file name and version, so a valid signature of another file or an older
// version can't be replayed.
const trustedPrefix = "longarm"

// TrustedComment is the signed comment of a release file.
func TrustedComment(file, version string) string {
	return fmt.Sprintf("%s file:%s version:%s", trustedPrefix, file, version)
}

// Enabled reports whether this build can verify updates.
func Enabled() bool { return strings.TrimSpace(PublicKey) != "" }

func parsePublicKey(s string) (minisign.PublicKey, error) {
	var pk minisign.PublicKey
	s = strings.TrimSpace(s)
	// Accept the .pub file format too (untrusted comment + key line).
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	if err := pk.UnmarshalText([]byte(s)); err != nil {
		return pk, fmt.Errorf("update public key: %w", err)
	}
	return pk, nil
}

// Verify checks that sig is a valid signature of data under publicKey and
// that its trusted comment names exactly file and version.
func Verify(publicKey string, data, sig []byte, file, version string) error {
	pk, err := parsePublicKey(publicKey)
	if err != nil {
		return err
	}
	if !minisign.Verify(pk, data, sig) {
		return fmt.Errorf("%s: invalid signature", file)
	}
	var s minisign.Signature
	if err := s.UnmarshalText(sig); err != nil {
		return fmt.Errorf("%s: signature: %w", file, err)
	}
	if want := TrustedComment(file, version); strings.TrimSpace(s.TrustedComment) != want {
		return fmt.Errorf("%s: signature is for %q, want %q", file, s.TrustedComment, want)
	}
	return nil
}

// Sign signs data for a release file (used by `longarm-hub release sign` and
// the packaging scripts).
func Sign(priv minisign.PrivateKey, data []byte, file, version string) []byte {
	return minisign.SignWithComments(priv, data, TrustedComment(file, version), "signature from Longarm release key")
}

// GenerateKey creates a key pair: the public key line (for PublicKey) and
// the private key file contents, encrypted with password unless it is "".
func GenerateKey(password string) (pub string, privFile []byte, err error) {
	pk, sk, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	if password != "" {
		privFile, err = minisign.EncryptKey(password, sk)
	} else {
		privFile, err = sk.MarshalText()
	}
	if err != nil {
		return "", nil, err
	}
	b, err := pk.MarshalText()
	// Only the key line ("RW..."), without the untrusted comment line, so it
	// fits into -ldflags -X.
	s := strings.TrimSpace(string(b))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	return s, privFile, err
}

// LoadPrivateKey reads a private key file (encrypted ones need password).
func LoadPrivateKey(path, password string) (minisign.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return minisign.PrivateKey{}, err
	}
	if minisign.IsEncrypted(b) {
		if password == "" {
			return minisign.PrivateKey{}, errors.New(path + " is encrypted: set the password")
		}
		return minisign.DecryptKey(password, b)
	}
	var sk minisign.PrivateKey
	err = sk.UnmarshalText(b)
	return sk, err
}
