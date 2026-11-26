//go:build linux

package screen

import (
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// Cisco's OpenH264 is not bundled: Cisco's patent licence covers only the
// binary Cisco distributes, downloaded from Cisco. It is found, in order,
// at $LONGARM_OPENH264, in the system's libraries, in the user's cache,
// or downloaded from ciscobinary.openh264.org into the cache.

const openH264Version = "2.7.0"

type openH264Build struct {
	file   string // ciscobinary.openh264.org/<file>.bz2
	sha256 string // of the library (not the .bz2)
}

// The hashes are Cisco's: the signed <file>.signed.sha256.txt next to
// each binary (linked from the v2.7.0 release notes on
// github.com/cisco/openh264/releases), checked against the downloads.
var openH264Builds = map[string]openH264Build{
	"amd64": {"libopenh264-2.7.0-linux64.8.so", "1522f873a482923c5ec018a4415e1866898197528828c50f90fd300c94620df3"},
	"arm64": {"libopenh264-2.7.0-linux-arm64.8.so", "9ac664b0154fd0868491c2a904397b8a05779874c0603b8b1f16ad15de1ffd82"},
}

// openH264Cached is where the download goes ("" when this OS/arch has no
// Cisco build or there is no cache directory).
func openH264Cached() (path string, b openH264Build) {
	b, ok := openH264Builds[runtime.GOARCH]
	if !ok {
		return "", b
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", b
	}
	return filepath.Join(dir, "longarm", "openh264", openH264Version, b.file), b
}

// openH264Verified: the cached library is there and is Cisco's.
func openH264Verified() string {
	path, b := openH264Cached()
	if path == "" {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != b.sha256 {
		return ""
	}
	return path
}

// DownloadOpenH264 fetches Cisco's OpenH264 into the user's cache (if it
// is not there yet) and returns its path. H.264 on Linux uses it.
func DownloadOpenH264(ctx context.Context) (string, error) {
	if p := openH264Verified(); p != "" {
		return p, nil
	}
	path, b := openH264Cached()
	if path == "" {
		return "", fmt.Errorf("no OpenH264 build from Cisco for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	url := "https://ciscobinary.openh264.org/" + b.file + ".bz2"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download OpenH264: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download OpenH264: %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(bzip2.NewReader(io.LimitReader(resp.Body, 16<<20)), 32<<20))
	if err != nil {
		return "", fmt.Errorf("download OpenH264: %w", err)
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != b.sha256 {
		return "", fmt.Errorf("download OpenH264: %s is not Cisco's build (SHA-256 %x)", url, sum)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// Another process (the viewer next to the worker) may fetch it too:
	// write aside, then rename.
	f, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, bytes.NewReader(data))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return path, nil
}
