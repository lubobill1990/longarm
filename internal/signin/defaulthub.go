package signin

import (
	"strings"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/version"
)

// Where a hub to sign in to came from (FallbackHub).
const (
	HubFromInstaller = "installer" // the MSI recorded it (InstalledHub)
	HubFromBuild     = "built-in"  // this build's default (version.DefaultHub)
)

// DefaultHub is the hub built into this release (version.DefaultHub, the
// repository variable LONGARM_DEFAULT_HUB at release time) and its optional
// CA pin; "" when the build has none.
func DefaultHub() (hub, fingerprint string) {
	hub = strings.TrimSpace(version.DefaultHub)
	if hub == "" {
		return "", ""
	}
	return hub, pki.NormalizeFingerprint(strings.TrimSpace(version.DefaultHubCA))
}

// FallbackHub is the hub to sign in to when the user gave none and the
// config / data dir names none: the one the installer recorded, else the
// build's default. from says which (HubFromInstaller, HubFromBuild); all
// empty when there is neither - the caller then asks for a hub.
func FallbackHub() (hub, fingerprint, from string) {
	return fallbackHub(InstalledHub, DefaultHub)
}

func fallbackHub(installed, builtIn func() (string, string)) (hub, fingerprint, from string) {
	if h, fp := installed(); strings.TrimSpace(h) != "" {
		h = strings.TrimSpace(h)
		if fp == "" {
			// the installer recorded the built-in hub (the MSI's Default):
			// the build's pin applies
			if dh, dfp := builtIn(); dh != "" && sameHub(dh, h) {
				fp = dfp
			}
		}
		return h, fp, HubFromInstaller
	}
	if h, fp := builtIn(); h != "" {
		return h, fp, HubFromBuild
	}
	return "", "", ""
}

// sameHub compares two hub addresses as typed ("example.com",
// "https://example.com/", "example.com:7443") by host.
func sameHub(a, b string) bool { return strings.EqualFold(hubHost(a), hubHost(b)) }

func hubHost(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i:], "]") {
		s = s[:i]
	}
	return strings.Trim(s, "[]")
}
