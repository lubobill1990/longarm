package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Verification of a Windows installer (MSI) from the hub's release store,
// used by the LongarmUpdate service (internal/updatesvc); the tray's own
// update banner checks the same (sha256, then the signature: its verifyMSI).

// MaxPackageSize caps a downloaded package (the MSI is tens of MB).
const MaxPackageSize = 512 << 20

// MaxSignatureSize caps a .minisig file.
const MaxSignatureSize = 8 << 10

// MSIName is the release file name of the Windows installer for arch
// (amd64, arm64): longarm-windows-<arch>.msi.
func MSIName(arch string) string { return "longarm-windows-" + arch + ".msi" }

// Errors of CheckNewer.
var (
	// ErrSameVersion: the target is the version installed (not an error
	// for "install the latest": there is nothing to do).
	ErrSameVersion = errors.New("that version is installed already")
	// ErrDowngrade: the target is older than the version installed.
	ErrDowngrade = errors.New("refusing a downgrade")
)

// CheckNewer accepts target only when it is strictly newer than installed
// (Compare). A build without a version ("dev") never updates itself: the
// installed version is unknown.
func CheckNewer(installed, target string) error {
	if !ValidVersion(target) {
		return fmt.Errorf("invalid version %q", target)
	}
	if _, _, ok := split(installed); !ok {
		return fmt.Errorf("the installed version %q is not a release version", installed)
	}
	switch c := Compare(target, installed); {
	case c == 0:
		return ErrSameVersion
	case c < 0:
		return fmt.Errorf("%w: %s is older than the installed %s", ErrDowngrade, target, installed)
	}
	return nil
}

// VerifyPackage checks a downloaded release package: its size and sha256
// against the hub's release record, and its minisign signature against
// publicKey (the key built into the verifying binary, never one from the
// hub), whose trusted comment must name exactly name and version.
func VerifyPackage(publicKey string, data, sig []byte, name, version, wantSHA256 string, wantSize int64) error {
	if strings.TrimSpace(publicKey) == "" {
		return errors.New("this build has no release key: it can't verify updates")
	}
	if !PackageFileRE.MatchString(name) {
		return fmt.Errorf("%q is not a release package", name)
	}
	if !ValidVersion(version) {
		return fmt.Errorf("invalid version %q", version)
	}
	if wantSize > 0 && int64(len(data)) != wantSize {
		return fmt.Errorf("%s: %d bytes, the release record says %d", name, len(data), wantSize)
	}
	sum := sha256.Sum256(data)
	if want := strings.ToLower(strings.TrimSpace(wantSHA256)); want == "" || hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("%s: sha256 %s does not match the release record (%q)", name, hex.EncodeToString(sum[:]), wantSHA256)
	}
	if len(sig) == 0 {
		return fmt.Errorf("%s: not signed", name)
	}
	return Verify(publicKey, data, sig, name, version)
}
