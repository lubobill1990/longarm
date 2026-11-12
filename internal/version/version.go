// Package version carries the build version (set with
// -ldflags "-X longarm/internal/version.Version=v0.1.0")
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
//	-ldflags "-X longarm/internal/version.DefaultHub=longarm.example.com"
var DefaultHub = ""

// DefaultHubCA optionally pins DefaultHub's CA (sha256 hex of its
// certificate, longarm-hub ca show; repository variable
// LONGARM_DEFAULT_HUB_CA). Empty: the CA fetched over HTTPS is trusted.
var DefaultHubCA = ""

// RepoURL is the repository this build came from (https://github.com/OWNER/REPO;
// the release workflows set it from github.server_url and github.repository):
// a hub's [web] repo_url and feedback_url default to it, the tray's links and
// "Add a machine" downloads too. Empty: local builds (no project links).
//
//	-ldflags "-X longarm/internal/version.RepoURL=https://github.com/OWNER/REPO"
var RepoURL = ""
