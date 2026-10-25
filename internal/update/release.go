package update

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

// Release file names (the hub's release store, `longarm admin release
// publish`): binaries <tool>-<os>-<arch>[.exe] and packages
// longarm-<os>-<arch>.<ext>, each optionally with <file>.minisig.
var (
	BinaryFileRE  = regexp.MustCompile(`^(longarm|longarmd|longarm-tray|longarm-hub|longarm-relay)-(windows|linux|darwin)-(amd64|arm64)(\.exe)?$`)
	PackageFileRE = regexp.MustCompile(`^longarm-(windows|linux|darwin)-(amd64|arm64)\.(msi|deb|rpm|pkg|tar\.gz|zip)$`)
	VersionRE     = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)
	ChannelRE     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// IsReleaseFile reports whether name is a release file (not its signature).
func IsReleaseFile(name string) bool {
	return BinaryFileRE.MatchString(name) || PackageFileRE.MatchString(name)
}

// IsUpdateBinary reports whether name is a binary workers update themselves
// with (it must be signed when the hub has a public key).
func IsUpdateBinary(name string) bool {
	m := BinaryFileRE.FindStringSubmatch(name)
	return m != nil && slices.Contains(Binaries, m[1])
}

// ReleaseFiles lists the release files in dir (sorted, signatures excluded).
func ReleaseFiles(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && IsReleaseFile(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// SignDir writes <file>.minisig for every release file in dir, bound to
// version (the trusted comment), with the private key in keyFile.
func SignDir(dir, version, keyFile, password string) ([]string, error) {
	sk, err := LoadPrivateKey(keyFile, password)
	if err != nil {
		return nil, err
	}
	files, err := ReleaseFiles(dir)
	if err != nil {
		return nil, err
	}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, name+SigExt), Sign(sk, data, name, version), 0o644); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// DefaultKeyDir is where `longarm-hub release keygen` puts the release key pair
// by default and `longarm admin release sign|publish` look for it: "release" in
// longarm's config dir (LONGARM_CONFIG_DIR, else ~/.config/longarm) on the admin's
// machine. The private key never goes to the hub.
func DefaultKeyDir() string {
	d := os.Getenv("LONGARM_CONFIG_DIR")
	if d == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "release"
		}
		d = filepath.Join(home, ".config", "longarm")
	}
	return filepath.Join(d, "release")
}

// KeyFile / PubFile are the key pair's file names.
const (
	KeyFile = "longarm-release.key"
	PubFile = "longarm-release.pub"
)
