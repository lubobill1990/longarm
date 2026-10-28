package hub

import (
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/lubobill1990/longarm/internal/hub/store"
)

// Update notices: nodes that can't update themselves (MSI installs, clients)
// learn about the newest stable release whose files the hub has. Workers get
// it with CheckUpdate, clients with Whoami, the tray and the panel from
// GET /releases/latest.json.

// latestTTL: how long the newest release is cached (Whoami runs on every
// hub session of the CLI).
const latestTTL = time.Minute

type latestCache struct {
	mu      sync.Mutex
	at      time.Time
	version string
	files   map[string]store.Release
}

// latestNotice is latestRelease, cached for latestTTL.
func (h *Hub) latestNotice() (string, map[string]store.Release) {
	c := &h.latest
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < latestTTL {
		return c.version, c.files
	}
	v, files, err := h.latestRelease()
	if err != nil {
		h.log.Warn("latest release", "err", err)
		return c.version, c.files // the last good answer
	}
	c.version, c.files, c.at = v, files, time.Now()
	return v, files
}

// latestPath is where a client on goos/goarch gets version: its MSI when the
// release has one, else the install script.
func latestPath(version string, files map[string]store.Release, goos, goarch string) string {
	if version == "" {
		return ""
	}
	if goos == "windows" {
		if goarch == "" {
			goarch = "amd64"
		}
		if msi := "longarm-windows-" + goarch + ".msi"; files[msi].File != "" {
			return "/releases/" + version + "/" + msi
		}
		return "/install.ps1"
	}
	return "/install.sh"
}

// latestFile is a release file in GET /releases/latest.json.
type latestFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	OS     string `json:"os,omitempty"`
	Arch   string `json:"arch,omitempty"`
	Kind   string `json:"kind,omitempty"`
	Signed bool   `json:"signed"`
}

// latestJSON serves GET /releases/latest.json (public, like the install
// scripts): {"version", "files": [...]}; version "" = no release. The tray
// checks a downloaded MSI against this sha256 and against the .minisig.
func (h *Hub) latestJSON(w http.ResponseWriter, _ *http.Request) {
	v, files := h.latestNotice()
	out := struct {
		Version string       `json:"version"`
		Files   []latestFile `json:"files"`
	}{Version: v, Files: []latestFile{}}
	for _, r := range files {
		out.Files = append(out.Files, latestFile{Name: r.File, SHA256: r.SHA256, Size: r.Size, OS: r.OS, Arch: r.Arch, Kind: r.Kind, Signed: len(r.Sig) > 0})
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Name < out.Files[j].Name })
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, out)
}
