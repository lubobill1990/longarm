// Package version carries the build version (set with
// -ldflags "-X github.com/lubobill1990/longarm/internal/version.Version=v0.1.0")
// and the built-in default hub.
package version

// Version of longarmd / longarm.
var Version = "dev"

// DefaultHub is the hub this build signs in to when none is given and no
// installer recorded one (`longarm login`, `longarmd login`, the tray, the
// MSI's "Default" choice): a domain such as longarm.example.com, whose CA
// comes from https://<domain>/ca.crt at sign-in. Empty: no default (forks,
// local builds). Set at release time from the repository variable
// LONGARM_DEFAULT_HUB:
//
//	-ldflags "-X github.com/lubobill1990/longarm/internal/version.DefaultHub=longarm.example.com"
var DefaultHub = ""

// DefaultHubCA optionally pins DefaultHub's CA (sha256 hex of its
// certificate, longarm-hub ca show; repository variable
// LONGARM_DEFAULT_HUB_CA). Empty: the CA fetched over HTTPS is trusted.
var DefaultHubCA = ""
