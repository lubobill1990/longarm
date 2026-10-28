package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckNewer(t *testing.T) {
	for _, tc := range []struct {
		installed, target string
		want              error
	}{
		{"0.1.8", "0.1.9", nil},
		{"0.1.9", "0.1.9", ErrSameVersion},
		{"v0.1.9", "0.1.9", ErrSameVersion},
		{"0.1.9", "0.1.8", ErrDowngrade},
		{"0.1.9", "0.1.9-rc.1", ErrDowngrade},
		{"0.1.9-rc.1", "0.1.9", nil},
	} {
		if err := CheckNewer(tc.installed, tc.target); !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("%s -> %s: %v, want %v", tc.installed, tc.target, err, tc.want)
		}
	}
	for _, bad := range [][2]string{{"dev", "0.1.9"}, {"0.1.8", "../0.1.9"}, {"0.1.8", ""}} {
		if err := CheckNewer(bad[0], bad[1]); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestVerifyPackage(t *testing.T) {
	pub, priv, err := GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	kf := filepath.Join(t.TempDir(), "k")
	os.WriteFile(kf, priv, 0o600)
	sk, err := LoadPrivateKey(kf, "")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("msi")
	name := MSIName("amd64")
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	sig := Sign(sk, data, name, "0.1.9")
	if err := VerifyPackage(pub, data, sig, name, "0.1.9", sha, 3); err != nil {
		t.Fatal(err)
	}
	for what, err := range map[string]error{
		"no key":        VerifyPackage("", data, sig, name, "0.1.9", sha, 3),
		"no signature":  VerifyPackage(pub, data, nil, name, "0.1.9", sha, 3),
		"other bytes":   VerifyPackage(pub, []byte("msj"), sig, name, "0.1.9", hex.EncodeToString(func() []byte { s := sha256.Sum256([]byte("msj")); return s[:] }()), 3),
		"sha256":        VerifyPackage(pub, data, sig, name, "0.1.9", sha[:63]+"0", 3),
		"size":          VerifyPackage(pub, data, sig, name, "0.1.9", sha, 4),
		"other version": VerifyPackage(pub, data, sig, name, "0.1.10", sha, 3),
		"other file":    VerifyPackage(pub, data, sig, MSIName("arm64"), "0.1.9", sha, 3),
		"not a package": VerifyPackage(pub, data, sig, "longarmd-windows-amd64.exe", "0.1.9", sha, 3),
	} {
		if err == nil {
			t.Errorf("%s: accepted", what)
		}
	}
}
